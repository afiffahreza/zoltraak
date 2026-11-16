package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type reportConfig struct {
	Base             string   `json:"base"`
	IndexBase        string   `json:"index_base"`
	HostHeader       string   `json:"host_header,omitempty"`
	Suites           []string `json:"suites"`
	Components       []string `json:"components"`
	Arch             string   `json:"arch"`
	PeakRPS          float64  `json:"peak_rps"`
	Shape            string   `json:"shape"`
	StepRPS          float64  `json:"step_rps,omitempty"`
	StepEverySeconds float64  `json:"step_every_s,omitempty"`
	RampSeconds      float64  `json:"ramp_s"`
	HoldSeconds      float64  `json:"hold_s"`
	MaxInflight      int      `json:"max_inflight"`
	TimeoutSeconds   float64  `json:"timeout_s"`
	MaxSizeBytes     int64    `json:"max_size_bytes"`
	MinSizeBytes     int64    `json:"min_size_bytes"`
	Distinct         int      `json:"distinct"`
	Seed             uint64   `json:"seed"`
	PlannedRequests  int      `json:"planned_requests"`
	PlannedBytes     int64    `json:"planned_bytes"`
}

type report struct {
	StartedAt   time.Time    `json:"started_at"`
	Interrupted bool         `json:"interrupted"`
	Config      reportConfig `json:"config"`
	Summary     summary      `json:"summary"`
	Timeseries  []sample     `json:"timeseries"`
}

// writeReport writes <dir>/report-<UTC time>.{json,csv,html} and returns their paths.
func writeReport(dir string, r report) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	stem := filepath.Join(dir, "report-"+r.StartedAt.UTC().Format("20060102T150405Z"))
	jsonPath, csvPath, htmlPath := stem+".json", stem+".csv", stem+".html"

	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(jsonPath, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}

	f, err := os.Create(csvPath)
	if err != nil {
		return nil, err
	}
	w := csv.NewWriter(f)
	w.Write([]string{"t_s", "target_rps", "sent_per_s", "done_per_s", "fail_per_s", "inflight", "dropped_total",
		"p50_ms", "p95_ms", "p99_ms", "ttfb_p95_ms", "mb_per_s"})
	for _, s := range r.Timeseries {
		w.Write([]string{
			f2(s.TSeconds), f2(s.TargetRPS), f2(s.SentPS), f2(s.DonePS), f2(s.FailPS),
			strconv.FormatInt(s.Inflight, 10), strconv.Itoa(s.Dropped),
			f2(s.P50Ms), f2(s.P95Ms), f2(s.P99Ms), f2(s.TTFBP95Ms), f2(s.MBPerSec),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", csvPath, err)
	}

	if err := writeHTML(htmlPath, r); err != nil {
		return nil, fmt.Errorf("write %s: %w", htmlPath, err)
	}
	return []string{jsonPath, csvPath, htmlPath}, nil
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
