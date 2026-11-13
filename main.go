package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	base, indexBase, host string
	suites, components    []string
	arch                  string
	rate                  float64
	ramp, hold            time.Duration
	maxInflight           int
	timeout               time.Duration
	maxSize               int64
	seed                  uint64
	interval              time.Duration
	dryRun                bool
	out                   string
}

func parseFlags() config {
	var (
		cfg                config
		suites, components string
		seed               int64
	)
	flag.StringVar(&cfg.base, "base", "http://archive.ubuntu.com/ubuntu", "archive root URL to load test (the proxy cache)")
	flag.StringVar(&cfg.indexBase, "index-base", "", "archive root URL used to fetch Packages.gz indexes (default: -base)")
	flag.StringVar(&cfg.host, "host", "", "override the Host header (e.g. archive.ubuntu.com when -base is an IP)")
	flag.StringVar(&suites, "suites", "noble,noble-updates,noble-security", "comma-separated suites to draw packages from")
	flag.StringVar(&components, "components", "main,restricted,universe,multiverse", "comma-separated components")
	flag.StringVar(&cfg.arch, "arch", "amd64", "architecture (arm64/riscv64/... live on ports.ubuntu.com)")
	flag.Float64Var(&cfg.rate, "rate", 200, "peak requests per second")
	flag.DurationVar(&cfg.ramp, "ramp", 5*time.Minute, "time to ramp linearly from 0 to -rate")
	flag.DurationVar(&cfg.hold, "hold", 2*time.Minute, "time to hold at -rate after the ramp")
	flag.IntVar(&cfg.maxInflight, "max-inflight", 2000, "client-side cap on concurrent requests; excess requests are dropped and counted")
	flag.DurationVar(&cfg.timeout, "timeout", 60*time.Second, "per-request timeout")
	flag.Int64Var(&cfg.maxSize, "max-size", 0, "skip packages larger than this many bytes (0 = no limit)")
	flag.Int64Var(&seed, "seed", time.Now().UnixNano(), "shuffle seed; reuse it to replay the same request order")
	flag.DurationVar(&cfg.interval, "interval", time.Second, "stats print interval")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "fetch indexes, print the plan, and exit without sending load")
	flag.StringVar(&cfg.out, "out", "", "directory to write report-<timestamp>.json/.csv/.html (summary + per-interval time series + charts)")
	flag.Parse()

	cfg.seed = uint64(seed)
	cfg.base = strings.TrimRight(cfg.base, "/")
	cfg.indexBase = strings.TrimRight(cfg.indexBase, "/")
	if cfg.indexBase == "" {
		cfg.indexBase = cfg.base
	}
	cfg.suites = splitCSV(suites)
	cfg.components = splitCSV(components)
	return cfg
}

func splitCSV(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func newClient(cfg config, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil, // never route load through HTTP(S)_PROXY from the environment
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        cfg.maxInflight,
			MaxIdleConnsPerHost: cfg.maxInflight,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true, // .debs are already compressed; keep bytes on the wire == Size
			ForceAttemptHTTP2:   false,
		},
	}
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	if cfg.rate <= 0 || cfg.ramp < 0 || cfg.hold < 0 || cfg.maxInflight <= 0 {
		return errors.New("-rate and -max-inflight must be > 0; -ramp and -hold must be >= 0")
	}
	pl := plan{rate: cfg.rate, ramp: cfg.ramp, hold: cfg.hold}
	total := pl.total()
	if total == 0 {
		return errors.New("plan issues zero requests")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := loadPool(ctx, newClient(cfg, 2*time.Minute), cfg)
	if err != nil {
		return err
	}
	if len(pool) < total {
		return fmt.Errorf("pool has %d unique packages but the plan needs %d (every request must be unique); "+
			"add -suites/-components, or reduce -rate/-ramp/-hold", len(pool), total)
	}
	shuffle(pool, cfg.seed)
	pool = pool[:total]

	var bytes int64
	for _, p := range pool {
		bytes += p.size
	}
	avg := float64(bytes) / float64(total)
	fmt.Printf("\nplan: ramp 0->%.0f rps over %s, hold %s => %d unique requests (pool %s)\n",
		cfg.rate, cfg.ramp, cfg.hold, total, "shuffled, seed "+fmt.Sprint(cfg.seed))
	fmt.Printf("      payload %.2f GiB total, avg %.0f KiB/package, ~%.0f MB/s (%.2f Gbit/s) at peak\n",
		float64(bytes)/(1<<30), avg/1024, avg*cfg.rate/1e6, avg*cfg.rate*8/1e9)
	if cfg.dryRun {
		return nil
	}

	client := newClient(cfg, cfg.timeout)
	st := newStats()
	sem := make(chan struct{}, cfg.maxInflight)
	var wg sync.WaitGroup

	start := time.Now()
	stopReport, reportDone := make(chan struct{}), make(chan struct{})
	go st.report(pl, start, cfg.interval, stopReport, reportDone)

	timer := time.NewTimer(0)
	defer timer.Stop()
dispatch:
	for n := range total {
		if d := time.Until(start.Add(pl.at(n))); d > 0 {
			timer.Reset(d)
			select {
			case <-ctx.Done():
				break dispatch
			case <-timer.C:
			}
		} else if ctx.Err() != nil {
			break dispatch
		}
		select {
		case sem <- struct{}{}:
		default:
			st.dropOne() // open loop: never block the schedule on slow responses
			continue
		}
		st.sentOne()
		st.inflight.Add(1)
		wg.Add(1)
		go func(p pkg) {
			defer func() {
				st.inflight.Add(-1)
				<-sem
				wg.Done()
			}()
			st.record(fetch(client, cfg, p))
		}(pool[n])
	}

	interrupted := ctx.Err() != nil
	stop() // restore default signal handling: a second Ctrl-C kills immediately
	if interrupted {
		fmt.Printf("\ninterrupted; draining %d in-flight requests (Ctrl-C again to abort)\n", st.inflight.Load())
	}
	wg.Wait()
	close(stopReport)
	<-reportDone
	elapsed := time.Since(start)
	sum := st.summarize(elapsed)
	sum.print()

	if cfg.out == "" {
		return nil
	}
	paths, err := writeReport(cfg.out, report{
		StartedAt:   start,
		Interrupted: interrupted,
		Config: reportConfig{
			Base: cfg.base, IndexBase: cfg.indexBase, HostHeader: cfg.host,
			Suites: cfg.suites, Components: cfg.components, Arch: cfg.arch,
			PeakRPS: cfg.rate, RampSeconds: cfg.ramp.Seconds(), HoldSeconds: cfg.hold.Seconds(),
			MaxInflight: cfg.maxInflight, TimeoutSeconds: cfg.timeout.Seconds(),
			MaxSizeBytes: cfg.maxSize, Seed: cfg.seed,
			PlannedRequests: total, PlannedBytes: bytes,
		},
		Summary:    sum,
		Timeseries: st.timeseries(),
	})
	if err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Println()
	for i, p := range paths {
		label := ""
		if i == 0 {
			label = "report"
		}
		fmt.Printf("%-14s %s\n", label, p)
	}
	return nil
}

// fetch downloads p and discards the body, returning the observed outcome.
func fetch(c *http.Client, cfg config, p pkg) result {
	t0 := time.Now()
	req, err := http.NewRequest(http.MethodGet, cfg.base+"/"+p.path, nil)
	if err != nil {
		return result{outcome: "error: " + err.Error()}
	}
	if cfg.host != "" {
		req.Host = cfg.host
	}
	var ttfb time.Duration
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotFirstResponseByte: func() { ttfb = time.Since(t0) },
	}))

	resp, err := c.Do(req)
	if err != nil {
		return result{outcome: errOutcome(err), lat: time.Since(t0)}
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	lat := time.Since(t0)
	switch {
	case err != nil:
		return result{outcome: errOutcome(err), bytes: n, lat: lat}
	case resp.StatusCode != http.StatusOK:
		return result{outcome: fmt.Sprintf("HTTP %d", resp.StatusCode), bytes: n, lat: lat}
	case n != p.size:
		return result{outcome: "size mismatch (body != index Size)", bytes: n, lat: lat}
	}
	return result{outcome: "HTTP 200", ok: true, bytes: n, lat: lat, ttfb: ttfb}
}

// errOutcome drops the per-request URL from transport errors so outcomes aggregate.
func errOutcome(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := "error: " + err.Error()
	if len(s) > 90 {
		s = s[:90]
	}
	return s
}
