package main

import (
	"fmt"
	"html/template"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// writeHTML renders a single self-contained HTML file (inline CSS + inline SVG, no JS, no network).
func writeHTML(path string, r report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := htmlTmpl.Execute(f, buildView(r)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type card struct{ Label, Value, Class string }
type outcomeRow struct {
	Name, Count, Pct string
	OK               bool
}
type latRow struct{ Name, P50, P90, P95, P99, Max string }
type legendItem struct{ Name, Class string }
type chartView struct {
	Title  string
	Legend []legendItem
	SVG    template.HTML // generated here from numbers only
}
type htmlView struct {
	Title       string
	Started     string
	Interrupted bool
	Warnings    []string
	Cards       []card
	Config      [][2]string
	Outcomes    []outcomeRow
	Latency     []latRow
	Charts      []chartView
}

func buildView(r report) htmlView {
	c, s := r.Config, r.Summary
	v := htmlView{
		Title:       "archive-loadtest report: " + c.Base,
		Started:     r.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
		Interrupted: r.Interrupted,
	}
	if s.Dropped > 0 {
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"%d requests were dropped because the client hit its in-flight cap (%d). The load generator, not the server, was a bottleneck; raise --max-inflight or reduce --rate.",
			s.Dropped, c.MaxInflight))
	}
	if r.Interrupted {
		v.Warnings = append(v.Warnings, "Run was interrupted before the plan completed.")
	}

	cls := func(bad bool, k string) string {
		if bad {
			return k
		}
		return ""
	}
	v.Cards = []card{
		{"Sent", strconv.Itoa(s.Sent) + " / " + strconv.Itoa(c.PlannedRequests), ""},
		{"OK", strconv.Itoa(s.OK), ""},
		{"Failed", strconv.Itoa(s.Failed), cls(s.Failed > 0, "bad")},
		{"Dropped (client)", strconv.Itoa(s.Dropped), cls(s.Dropped > 0, "warn")},
		{"Downloaded (discarded)", fmt.Sprintf("%.2f GiB", float64(s.Bytes)/(1<<30)), ""},
		{"Avg throughput", fmt.Sprintf("%.1f MB/s", s.AvgMBps), ""},
		{"Latency p95", fmt.Sprintf("%.0f ms", s.Latency.P95Ms), ""},
		{"Latency p99", fmt.Sprintf("%.0f ms", s.Latency.P99Ms), ""},
	}

	dur := func(sec float64) string { return time.Duration(sec * float64(time.Second)).String() }
	v.Config = [][2]string{
		{"Base URL", c.Base}, {"Index base", c.IndexBase},
	}
	if c.HostHeader != "" {
		v.Config = append(v.Config, [2]string{"Host header", c.HostHeader})
	}
	v.Config = append(v.Config,
		[2]string{"Suites", strings.Join(c.Suites, ", ")},
		[2]string{"Components", strings.Join(c.Components, ", ")},
		[2]string{"Architecture", c.Arch},
		[2]string{"Peak rate", fmt.Sprintf("%.0f req/s", c.PeakRPS)},
		[2]string{"Ramp / hold", dur(c.RampSeconds) + " / " + dur(c.HoldSeconds)},
		[2]string{"Planned requests", strconv.Itoa(c.PlannedRequests)},
		[2]string{"Planned payload", fmt.Sprintf("%.2f GiB", float64(c.PlannedBytes)/(1<<30))},
		[2]string{"Min package size", sizeOrNone(c.MinSizeBytes)},
		[2]string{"Max package size", sizeOrNone(c.MaxSizeBytes)},
		[2]string{"Distinct packages", distinctOrAll(c.Distinct)},
		[2]string{"Max in-flight", strconv.Itoa(c.MaxInflight)},
		[2]string{"Request timeout", dur(c.TimeoutSeconds)},
		[2]string{"Seed", strconv.FormatUint(c.Seed, 10)},
		[2]string{"Elapsed", dur(math.Round(s.ElapsedS*10) / 10)},
	)

	names := make([]string, 0, len(s.Outcomes))
	total := 0
	for k, n := range s.Outcomes {
		names = append(names, k)
		total += n
	}
	sort.Strings(names)
	for _, k := range names {
		n := s.Outcomes[k]
		v.Outcomes = append(v.Outcomes, outcomeRow{
			Name: k, Count: strconv.Itoa(n), Pct: fmt.Sprintf("%.2f%%", 100*float64(n)/float64(total)), OK: k == "HTTP 200",
		})
	}

	lr := func(name string, l latencySummary) latRow {
		f := func(x float64) string { return fmt.Sprintf("%.0f ms", x) }
		return latRow{name, f(l.P50Ms), f(l.P90Ms), f(l.P95Ms), f(l.P99Ms), f(l.MaxMs)}
	}
	v.Latency = []latRow{lr("Total", s.Latency), lr("Time to first byte", s.TTFB)}

	ts := r.Timeseries
	xs := make([]float64, len(ts))
	col := func(f func(sample) float64) []float64 {
		out := make([]float64, len(ts))
		for i, p := range ts {
			out[i] = f(p)
		}
		return out
	}
	for i, p := range ts {
		xs[i] = p.TSeconds
	}
	chart := func(title string, ss ...chartSeries) chartView {
		cv := chartView{Title: title, SVG: template.HTML(lineChart(xs, ss, c.RampSeconds))}
		for _, x := range ss {
			cv.Legend = append(cv.Legend, legendItem{x.Name, x.Class})
		}
		return cv
	}
	v.Charts = []chartView{
		chart("Request rate (req/s)",
			chartSeries{"Target (planned)", "tgt dash", col(func(p sample) float64 { return p.TargetRPS })},
			chartSeries{"Sent", "c1", col(func(p sample) float64 { return p.SentPS })},
			chartSeries{"Completed", "c3", col(func(p sample) float64 { return p.DonePS })},
			chartSeries{"Failed", "c4", col(func(p sample) float64 { return p.FailPS })}),
		chart("Latency of successful requests (ms)",
			chartSeries{"p50", "c3", col(func(p sample) float64 { return p.P50Ms })},
			chartSeries{"p95", "c2", col(func(p sample) float64 { return p.P95Ms })},
			chartSeries{"p99", "c4", col(func(p sample) float64 { return p.P99Ms })},
			chartSeries{"TTFB p95", "c1 dash", col(func(p sample) float64 { return p.TTFBP95Ms })}),
		chart("Throughput (MB/s)",
			chartSeries{"Downloaded", "c5", col(func(p sample) float64 { return p.MBPerSec })}),
		chart("Client concurrency",
			chartSeries{"In-flight requests", "c1", col(func(p sample) float64 { return float64(p.Inflight) })},
			chartSeries{"Dropped (cumulative)", "c4", col(func(p sample) float64 { return float64(p.Dropped) })}),
	}
	return v
}

func distinctOrAll(n int) string {
	if n <= 0 {
		return "every request unique"
	}
	return strconv.Itoa(n)
}

func sizeOrNone(n int64) string {
	if n <= 0 {
		return "no limit"
	}
	return fmt.Sprintf("%d bytes", n)
}

type chartSeries struct {
	Name, Class string
	Vals        []float64
}

const (
	cw, ch         = 900.0, 260.0
	ml, mr, mt, mb = 56.0, 16.0, 12.0, 34.0
)

// lineChart renders an inline SVG line chart. rampEnd (seconds) draws a marker where the hold phase starts.
func lineChart(xs []float64, ss []chartSeries, rampEnd float64) string {
	if len(xs) < 2 {
		return `<p class="muted">Not enough samples to chart (run was shorter than two intervals).</p>`
	}
	xmax := xs[len(xs)-1]
	ymax := 0.0
	for _, s := range ss {
		for _, y := range s.Vals {
			ymax = math.Max(ymax, y)
		}
	}
	ymax = niceCeil(ymax)
	px := func(x float64) float64 { return ml + x/xmax*(cw-ml-mr) }
	py := func(y float64) float64 { return mt + (1-y/ymax)*(ch-mt-mb) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" xmlns="http://www.w3.org/2000/svg">`, cw, ch)
	for i := 0; i <= 5; i++ {
		v := ymax * float64(i) / 5
		y := py(v)
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="tick" x="%.1f" y="%.1f" text-anchor="end">%s</text>`,
			ml, y, cw-mr, y, ml-6, y+4, tickLabel(v))
	}
	step := niceXStep(xmax)
	for t := 0.0; t <= xmax+1e-9; t += step {
		x := px(t)
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="tick" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`,
			x, mt, x, ch-mb, x, ch-mb+16, tickLabel(t))
	}
	fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.1f" text-anchor="end">time (s)</text>`, cw-mr, ch-4.0)
	if rampEnd > 0 && rampEnd < xmax {
		x := px(rampEnd)
		fmt.Fprintf(&b, `<line class="phase" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="tick" x="%.1f" y="%.1f">hold</text>`,
			x, mt, x, ch-mb, x+4, mt+10)
	}
	for _, s := range ss {
		b.WriteString(`<path class="line ` + s.Class + `" d="`)
		for i, y := range s.Vals {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&b, "%s%.1f,%.1f ", cmd, px(xs[i]), py(y))
		}
		b.WriteString(`"/>`)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// niceCeil rounds v up to 1, 2, 2.5, 5 or 10 times a power of ten.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*e*(1+1e-12) {
			return m * e
		}
	}
	return 10 * e
}

// niceXStep picks a time-axis step giving roughly 6 ticks.
func niceXStep(xmax float64) float64 {
	for _, s := range []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600} {
		if xmax/s <= 7 {
			return s
		}
	}
	return 3600
}

func tickLabel(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

var htmlTmpl = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#fff;--fg:#1c1e21;--muted:#6b7280;--border:#e5e7eb;--surface:#f6f7f9;
--c1:#2563eb;--c2:#ea580c;--c3:#16a34a;--c4:#dc2626;--c5:#7c3aed;--tgt:#6b7280}
@media (prefers-color-scheme:dark){:root{--bg:#14161a;--fg:#e6e8eb;--muted:#9aa1ab;--border:#2a2e35;--surface:#1b1e23;
--c1:#60a5fa;--c2:#fb923c;--c3:#4ade80;--c4:#f87171;--c5:#a78bfa;--tgt:#9aa1ab}}
body{margin:0;padding:24px;background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,-apple-system,Segoe UI,sans-serif}
main{max-width:1000px;margin:0 auto}
h1{font-size:20px;margin:0 0 2px}h2{font-size:16px;margin:28px 0 8px}
.muted{color:var(--muted)}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(150px,1fr));gap:10px;margin:16px 0}
.card{background:var(--surface);border:1px solid var(--border);border-radius:6px;padding:10px 12px}
.card .l{font-size:12px;color:var(--muted)}.card .v{font-size:20px;font-weight:600}
.card.bad .v{color:var(--c4)}.card.warn .v{color:var(--c2)}
.banner{border:1px solid var(--c2);border-left-width:4px;border-radius:6px;padding:8px 12px;margin:8px 0}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:5px 10px;border-bottom:1px solid var(--border)}
th{font-weight:600;color:var(--muted);font-size:12px}
td.n,th.n{text-align:right;font-variant-numeric:tabular-nums}
.cfg td:first-child{color:var(--muted);width:200px}
.fail{color:var(--c4)}
.chart{margin:0 0 20px}.chart h3{font-size:14px;margin:0 0 4px}
.legend{display:flex;gap:14px;flex-wrap:wrap;font-size:12px;color:var(--muted);margin-bottom:2px}
.legend i{display:inline-block;width:14px;height:0;border-top-width:3px;border-top-style:solid;vertical-align:middle;margin-right:5px}
.legend i.dash{border-top-style:dashed}
svg{width:100%;height:auto;display:block}
.grid{stroke:var(--border);stroke-width:1}.phase{stroke:var(--muted);stroke-width:1;stroke-dasharray:3 3}
.tick{fill:var(--muted);font-size:11px}
.line{fill:none;stroke-width:1.8;stroke-linejoin:round}.line.dash{stroke-dasharray:5 4}
.c1{stroke:var(--c1);border-color:var(--c1)}.c2{stroke:var(--c2);border-color:var(--c2)}
.c3{stroke:var(--c3);border-color:var(--c3)}.c4{stroke:var(--c4);border-color:var(--c4)}
.c5{stroke:var(--c5);border-color:var(--c5)}.tgt{stroke:var(--tgt);border-color:var(--tgt)}
</style></head><body><main>
<h1>{{.Title}}</h1>
<div class="muted">Started {{.Started}}</div>
{{range .Warnings}}<div class="banner">{{.}}</div>{{end}}
<div class="cards">{{range .Cards}}<div class="card {{.Class}}"><div class="l">{{.Label}}</div><div class="v">{{.Value}}</div></div>{{end}}</div>

{{range .Charts}}<div class="chart"><h3>{{.Title}}</h3>
<div class="legend">{{range .Legend}}<span><i class="{{.Class}}"></i>{{.Name}}</span>{{end}}</div>
{{.SVG}}</div>
{{end}}

<h2>Outcomes</h2>
<table><tr><th>Outcome</th><th class="n">Count</th><th class="n">Share</th></tr>
{{range .Outcomes}}<tr><td{{if not .OK}} class="fail"{{end}}>{{.Name}}</td><td class="n">{{.Count}}</td><td class="n">{{.Pct}}</td></tr>{{end}}
</table>

<h2>Latency (successful requests)</h2>
<table><tr><th></th><th class="n">p50</th><th class="n">p90</th><th class="n">p95</th><th class="n">p99</th><th class="n">max</th></tr>
{{range .Latency}}<tr><td>{{.Name}}</td><td class="n">{{.P50}}</td><td class="n">{{.P90}}</td><td class="n">{{.P95}}</td><td class="n">{{.P99}}</td><td class="n">{{.Max}}</td></tr>{{end}}
</table>

<h2>Configuration</h2>
<table class="cfg">{{range .Config}}<tr><td>{{index . 0}}</td><td>{{index . 1}}</td></tr>{{end}}</table>
</main></body></html>
`))
