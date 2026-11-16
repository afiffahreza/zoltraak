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
	base, indexBase, host          string
	suites, components             []string
	arch                           string
	rate                           float64
	ramp, hold                     time.Duration
	step                           float64
	stepEvery                      time.Duration
	rampSet, stepSet, stepEverySet bool
	maxInflight                    int
	timeout                        time.Duration
	maxSize, minSize               int64
	distinct                       int
	seed                           uint64
	interval                       time.Duration
	dryRun                         bool
	out                            string
}

func parseFlags() config {
	var (
		cfg                config
		suites, components string
		seed               int64
	)
	flag.StringVar(&cfg.base, "target", "", "(required) archive root URL of the server to load test")
	flag.StringVar(&cfg.indexBase, "index", "", "archive root URL used to fetch Packages.gz indexes (default: --target)")
	flag.StringVar(&cfg.host, "host", "", "override the Host header (e.g. archive.ubuntu.com when --target is an IP)")
	flag.StringVar(&suites, "suites", "noble,noble-updates,noble-security", "comma-separated suites to draw packages from")
	flag.StringVar(&components, "components", "main,restricted,universe,multiverse", "comma-separated components")
	flag.StringVar(&cfg.arch, "arch", "amd64", "architecture (arm64/riscv64/... live on ports.ubuntu.com)")
	flag.Float64Var(&cfg.rate, "rate", 5, "peak requests per second")
	flag.DurationVar(&cfg.ramp, "ramp", time.Minute, "time to ramp linearly from 0 to --rate (conflicts with --step)")
	flag.Float64Var(&cfg.step, "step", 0, "increase requests per second by this amount each --step-every, starting immediately and capped at --rate (conflicts with --ramp)")
	flag.DurationVar(&cfg.stepEvery, "step-every", 5*time.Minute, "time between rate increases (requires --step)")
	flag.DurationVar(&cfg.hold, "hold", time.Minute, "time to hold at --rate after reaching peak")
	flag.IntVar(&cfg.maxInflight, "max-inflight", 100, "client-side cap on concurrent requests; excess requests are dropped and counted")
	flag.DurationVar(&cfg.timeout, "timeout", 60*time.Second, "per-request timeout")
	flag.Var(newSizeFlag(&cfg.maxSize, 1<<20), "max-size", "skip packages larger than this `size`: bytes or K/M/G suffix, e.g. 1M, 512K, 200M (0 = no limit)")
	flag.Var(newSizeFlag(&cfg.minSize, 0), "min-size", "skip packages smaller than this `size`: bytes or K/M/G suffix, e.g. 10M (0 = no limit)")
	flag.IntVar(&cfg.distinct, "distinct", 0, "request from a pool of this many distinct packages, picked at random with repeats (0 = every request is a unique package)")
	flag.Int64Var(&seed, "seed", time.Now().UnixNano(), "shuffle seed; reuse it to replay the same request order")
	flag.DurationVar(&cfg.interval, "interval", time.Second, "stats print interval")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "fetch indexes, print the plan, and exit without sending load")
	flag.StringVar(&cfg.out, "out", "", "directory to write report-<timestamp>.json/.csv/.html (summary + per-interval time series + charts)")
	for long, short := range shortcuts {
		flag.Var(flag.Lookup(long).Value, short, flag.Lookup(long).Usage)
	}
	flag.Usage = usage
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "ramp":
			cfg.rampSet = true
		case "step":
			cfg.stepSet = true
		case "step-every":
			cfg.stepEverySet = true
		}
	})

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

// shortcuts maps long flag names to their single-letter aliases. An alias
// writes to the same variable as its long flag.
var shortcuts = map[string]string{"target": "t", "index": "i"}

// usage prints help with GNU-style double-dash flags and their single-dash
// shortcuts. The flag package accepts both -flag and --flag.
func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, "Usage: %s [options]\n\nOptions:\n", os.Args[0])
	isShortcut := make(map[string]bool, len(shortcuts))
	for _, s := range shortcuts {
		isShortcut[s] = true
	}
	flag.VisitAll(func(f *flag.Flag) {
		if isShortcut[f.Name] {
			return
		}
		name, desc := flag.UnquoteUsage(f)
		line := "  --" + f.Name
		if s, ok := shortcuts[f.Name]; ok {
			line = "  -" + s + ", --" + f.Name
		}
		if name != "" {
			line += " " + name
		}
		fmt.Fprintf(out, "%s\n    \t%s", line, desc)
		if f.DefValue != "" && f.DefValue != "0" && f.DefValue != "false" {
			fmt.Fprintf(out, " (default %s)", f.DefValue)
		}
		fmt.Fprintln(out)
	})
	fmt.Fprintf(out, "  -h, --help\n    \tshow this help\n")
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

// validateBase requires --target and checks that it is an http(s) URL with a host.
func validateBase(base string) error {
	if base == "" {
		return errors.New("--target (-t) is required: the archive root URL of the server to load test, e.g. http://SERVER/ubuntu")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("--target %q must be an http:// or https:// URL with a host", base)
	}
	return nil
}

// pacingPlan validates explicit flag combinations before any index is fetched.
func (cfg config) pacingPlan() (plan, error) {
	if cfg.stepSet && cfg.step <= 0 {
		return plan{}, errors.New("--step must be finite and > 0")
	}
	stepped := cfg.stepSet || cfg.step != 0
	if stepped && cfg.rampSet {
		return plan{}, errors.New("--step and --ramp cannot be used together")
	}
	if cfg.stepEverySet && !stepped {
		return plan{}, errors.New("--step-every requires --step")
	}
	if cfg.stepEverySet && cfg.stepEvery <= 0 {
		return plan{}, errors.New("--step-every must be > 0")
	}
	if stepped && !cfg.stepEverySet && cfg.stepEvery == 0 {
		cfg.stepEvery = 5 * time.Minute
	}
	p := plan{rate: cfg.rate, ramp: cfg.ramp, hold: cfg.hold, step: cfg.step, stepEvery: cfg.stepEvery}
	return p, p.validate()
}

// waitUntil uses one reusable timer and remains interruptible during the tail
// after the final request, so --hold always gets its complete planned duration.
func waitUntil(ctx context.Context, timer *time.Timer, deadline time.Time) bool {
	if ctx.Err() != nil {
		return false
	}
	if d := time.Until(deadline); d > 0 {
		timer.Reset(d)
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	return ctx.Err() == nil
}

func run(cfg config) error {
	if err := validateBase(cfg.base); err != nil {
		return err
	}
	if cfg.maxInflight <= 0 || cfg.interval <= 0 {
		return errors.New("--max-inflight and --interval must be > 0")
	}
	if cfg.minSize < 0 || cfg.distinct < 0 || (cfg.maxSize > 0 && cfg.minSize > cfg.maxSize) {
		return errors.New("--min-size and --distinct must be >= 0, and --min-size must not exceed --max-size")
	}
	pl, err := cfg.pacingPlan()
	if err != nil {
		return err
	}
	total := pl.total()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := loadPool(ctx, newClient(cfg, 2*time.Minute), cfg)
	if err != nil {
		return err
	}
	shuffle(pool, cfg.seed)
	reqs, err := schedule(pool, total, cfg.distinct, cfg.seed)
	if err != nil {
		return err
	}

	var bytes, footprint int64
	uniq := make(map[string]struct{}, len(reqs))
	for _, p := range reqs {
		bytes += p.size
		if _, seen := uniq[p.path]; !seen {
			uniq[p.path] = struct{}{}
			footprint += p.size
		}
	}
	avg := float64(bytes) / float64(total)
	if pl.step > 0 {
		fmt.Printf("\nplan: step %g rps every %s, starting at %g rps, capped at %g rps after %s, hold %s",
			pl.step, pl.stepEvery, pl.targetRate(0), pl.rate, pl.rampDuration(), pl.hold)
	} else {
		fmt.Printf("\nplan: linear ramp 0->%g rps over %s, hold %s", pl.rate, pl.ramp, pl.hold)
	}
	fmt.Printf(" => %d requests over %s, %d distinct packages (shuffled, seed %d)\n",
		total, pl.duration(), len(uniq), cfg.seed)
	fmt.Printf("      payload %.2f GiB total, avg %.0f KiB/request, ~%.0f MB/s (%.2f Gbit/s) at peak\n",
		float64(bytes)/(1<<30), avg/1024, avg*cfg.rate/1e6, avg*cfg.rate*8/1e9)
	fmt.Printf("      distinct footprint %.2f GiB (total size of the distinct packages requested)\n",
		float64(footprint)/(1<<30))
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

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
dispatch:
	for n := range total {
		if !waitUntil(ctx, timer, start.Add(pl.at(n))) {
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
		}(reqs[n])
	}
	waitUntil(ctx, timer, start.Add(pl.duration()))

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
	stepEverySeconds := 0.0
	if pl.step > 0 {
		stepEverySeconds = pl.stepEvery.Seconds()
	}
	paths, err := writeReport(cfg.out, report{
		StartedAt:   start,
		Interrupted: interrupted,
		Config: reportConfig{
			Base: cfg.base, IndexBase: cfg.indexBase, HostHeader: cfg.host,
			Suites: cfg.suites, Components: cfg.components, Arch: cfg.arch,
			Shape: pl.shape(), StepRPS: pl.step, StepEverySeconds: stepEverySeconds,
			PeakRPS: cfg.rate, RampSeconds: pl.rampSeconds(), HoldSeconds: cfg.hold.Seconds(),
			MaxInflight: cfg.maxInflight, TimeoutSeconds: cfg.timeout.Seconds(),
			MaxSizeBytes: cfg.maxSize, MinSizeBytes: cfg.minSize, Distinct: cfg.distinct, Seed: cfg.seed,
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
