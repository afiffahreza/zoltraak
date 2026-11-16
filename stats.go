package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	outcome string // "HTTP 200", "HTTP 404", "error: ...", "size mismatch ..."
	ok      bool
	bytes   int64
	lat     time.Duration
	ttfb    time.Duration
}

// sample is one reporting window; it is printed live and kept for the report files.
type sample struct {
	TSeconds   float64 `json:"t_s"`
	TargetRPS  float64 `json:"target_rps"`
	SentPS     float64 `json:"sent_per_s"`
	DonePS     float64 `json:"done_per_s"`
	FailPS     float64 `json:"fail_per_s"`
	Inflight   int64   `json:"inflight"`
	Dropped    int     `json:"dropped_total"`
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	TTFBP95Ms  float64 `json:"ttfb_p95_ms"`
	MBPerSec   float64 `json:"mb_per_s"`
	WindowSecs float64 `json:"window_s"`
}

type latencySummary struct {
	P50Ms float64 `json:"p50_ms"`
	P90Ms float64 `json:"p90_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

type summary struct {
	ElapsedS   float64        `json:"elapsed_s"`
	Sent       int            `json:"sent"`
	SentAvgRPS float64        `json:"sent_avg_rps"`
	Dropped    int            `json:"dropped"`
	OK         int            `json:"ok"`
	Failed     int            `json:"failed"`
	Bytes      int64          `json:"bytes_discarded"`
	AvgMBps    float64        `json:"avg_mb_per_s"`
	Outcomes   map[string]int `json:"outcomes"`
	Latency    latencySummary `json:"latency_total"`
	TTFB       latencySummary `json:"latency_ttfb"`
}

type stats struct {
	inflight atomic.Int64

	mu sync.Mutex
	// current reporting window
	wSent, wDone, wFail int
	wBytes              int64
	wLat, wTTFB         []time.Duration
	// totals
	sent, dropped, ok, fail int
	bytes                   int64
	lat, ttfb               []time.Duration
	outcomes                map[string]int
	series                  []sample
}

func newStats() *stats { return &stats{outcomes: make(map[string]int)} }

func (s *stats) sentOne() {
	s.mu.Lock()
	s.sent++
	s.wSent++
	s.mu.Unlock()
}

func (s *stats) dropOne() {
	s.mu.Lock()
	s.dropped++
	s.mu.Unlock()
}

func (s *stats) record(r result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[r.outcome]++
	s.bytes += r.bytes
	s.wBytes += r.bytes
	s.wDone++
	if !r.ok {
		s.fail++
		s.wFail++
		return
	}
	s.ok++
	s.lat = append(s.lat, r.lat)
	s.ttfb = append(s.ttfb, r.ttfb)
	s.wLat = append(s.wLat, r.lat)
	s.wTTFB = append(s.wTTFB, r.ttfb)
}

// timeseries returns a copy of all reporting windows so far.
func (s *stats) timeseries() []sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sample(nil), s.series...)
}

// report prints and records one sample per interval until done is closed,
// then flushes the final partial window.
func (s *stats) report(p plan, start time.Time, interval time.Duration, done <-chan struct{}, finished chan<- struct{}) {
	defer close(finished)
	fmt.Printf("\n%6s %7s %6s %6s %6s %6s %6s %7s %7s %7s %8s %8s\n",
		"t(s)", "target", "sent/s", "done/s", "fail/s", "infl", "drop", "p50ms", "p95ms", "p99ms", "ttfb95ms", "MB/s")
	tick := time.NewTicker(interval)
	defer tick.Stop()
	last := start
	emit := func(now time.Time) {
		dt := now.Sub(last).Seconds()
		if dt < 0.05 { // skip degenerate windows (e.g. final flush right after a tick)
			return
		}
		last = now
		s.mu.Lock()
		smp := sample{
			TSeconds:   now.Sub(start).Seconds(),
			TargetRPS:  p.targetRate(now.Sub(start) - time.Duration(dt/2*float64(time.Second))), // window-average target
			SentPS:     float64(s.wSent) / dt,
			DonePS:     float64(s.wDone) / dt,
			FailPS:     float64(s.wFail) / dt,
			Inflight:   s.inflight.Load(),
			Dropped:    s.dropped,
			P50Ms:      ms(percentile(s.wLat, 0.50)),
			P95Ms:      ms(percentile(s.wLat, 0.95)),
			P99Ms:      ms(percentile(s.wLat, 0.99)),
			TTFBP95Ms:  ms(percentile(s.wTTFB, 0.95)),
			MBPerSec:   float64(s.wBytes) / dt / 1e6,
			WindowSecs: dt,
		}
		s.wSent, s.wDone, s.wFail, s.wBytes = 0, 0, 0, 0
		s.wLat, s.wTTFB = nil, nil
		s.series = append(s.series, smp)
		s.mu.Unlock()
		fmt.Printf("%6.0f %7.1f %6.1f %6.1f %6.1f %6d %6d %7.0f %7.0f %7.0f %8.0f %8.2f\n",
			smp.TSeconds, smp.TargetRPS, smp.SentPS, smp.DonePS, smp.FailPS, smp.Inflight, smp.Dropped,
			smp.P50Ms, smp.P95Ms, smp.P99Ms, smp.TTFBP95Ms, smp.MBPerSec)
	}
	for {
		select {
		case now := <-tick.C:
			emit(now)
		case <-done:
			emit(time.Now())
			return
		}
	}
}

// summarize computes final totals. Call after all requests have finished.
func (s *stats) summarize(elapsed time.Duration) summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	lat := func(d []time.Duration) latencySummary {
		return latencySummary{
			P50Ms: ms(percentile(d, 0.50)), P90Ms: ms(percentile(d, 0.90)), P95Ms: ms(percentile(d, 0.95)),
			P99Ms: ms(percentile(d, 0.99)), MaxMs: ms(percentile(d, 1)),
		}
	}
	outcomes := make(map[string]int, len(s.outcomes))
	for k, v := range s.outcomes {
		outcomes[k] = v
	}
	return summary{
		ElapsedS:   elapsed.Seconds(),
		Sent:       s.sent,
		SentAvgRPS: float64(s.sent) / elapsed.Seconds(),
		Dropped:    s.dropped,
		OK:         s.ok,
		Failed:     s.fail,
		Bytes:      s.bytes,
		AvgMBps:    float64(s.bytes) / elapsed.Seconds() / 1e6,
		Outcomes:   outcomes,
		Latency:    lat(s.lat),
		TTFB:       lat(s.ttfb),
	}
}

func (m summary) print() {
	fmt.Printf("\n== summary ==\n")
	fmt.Printf("elapsed        %s\n", (time.Duration(m.ElapsedS * float64(time.Second))).Round(time.Millisecond))
	fmt.Printf("sent           %d (%.1f/s avg)\n", m.Sent, m.SentAvgRPS)
	fmt.Printf("dropped        %d (client in-flight cap hit; not sent)\n", m.Dropped)
	fmt.Printf("ok / failed    %d / %d\n", m.OK, m.Failed)
	fmt.Printf("downloaded     %.2f GiB discarded (%.2f MB/s avg)\n", float64(m.Bytes)/(1<<30), m.AvgMBps)

	keys := make([]string, 0, len(m.Outcomes))
	for k := range m.Outcomes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println("outcomes")
	for _, k := range keys {
		fmt.Printf("  %-40s %d\n", k, m.Outcomes[k])
	}
	for _, l := range []struct {
		name string
		s    latencySummary
	}{{"latency (total)", m.Latency}, {"latency (ttfb) ", m.TTFB}} {
		fmt.Printf("%s p50=%.0fms p90=%.0fms p95=%.0fms p99=%.0fms max=%.0fms\n",
			l.name, l.s.P50Ms, l.s.P90Ms, l.s.P95Ms, l.s.P99Ms, l.s.MaxMs)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// percentile sorts d in place and returns the q-quantile (nearest rank).
func percentile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	i := int(q*float64(len(d))+0.9999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(d) {
		i = len(d) - 1
	}
	return d[i]
}
