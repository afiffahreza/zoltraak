package main

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func parseTestFlags(t *testing.T, args ...string) config {
	t.Helper()
	oldFlags, oldArgs, oldUsage := flag.CommandLine, os.Args, flag.Usage
	t.Cleanup(func() {
		flag.CommandLine, os.Args, flag.Usage = oldFlags, oldArgs, oldUsage
	})
	flag.CommandLine = flag.NewFlagSet("zoltraak-test", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"zoltraak-test", "--target", "http://127.0.0.1:1"}, args...)
	return parseFlags()
}

func TestPacingFlags(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		shape     string
		ramp      time.Duration
		every     time.Duration
		wantError string
	}{
		{name: "linear defaults", shape: "linear", ramp: time.Minute},
		{name: "step ignores default ramp", args: []string{"--step", "2"}, shape: "step", ramp: 10 * time.Minute, every: 5 * time.Minute},
		{name: "fractional custom interval", args: []string{"--rate", "1", "--step", "0.25", "--step-every", "2s"}, shape: "step", ramp: 6 * time.Second, every: 2 * time.Second},
		{name: "step over cap", args: []string{"--step", "9"}, shape: "step", every: 5 * time.Minute},
		{name: "explicit default ramp conflict", args: []string{"--step", "2", "--ramp", "1m"}, wantError: "cannot be used together"},
		{name: "explicit zero ramp conflict", args: []string{"--step", "2", "--ramp", "0"}, wantError: "cannot be used together"},
		{name: "interval requires step", args: []string{"--step-every", "5m"}, wantError: "requires --step"},
		{name: "zero step", args: []string{"--step", "0"}, wantError: "--step must be"},
		{name: "negative step", args: []string{"--step", "-1"}, wantError: "--step must be"},
		{name: "nan step", args: []string{"--step", "NaN"}, wantError: "--step must be"},
		{name: "infinite step", args: []string{"--step", "+Inf"}, wantError: "--step must be"},
		{name: "zero interval", args: []string{"--step", "2", "--step-every", "0"}, wantError: "--step-every must be"},
		{name: "negative interval", args: []string{"--step", "2", "--step-every", "-1s"}, wantError: "--step-every must be"},
		{name: "nan rate", args: []string{"--rate", "NaN"}, wantError: "--rate must be"},
		{name: "infinite rate", args: []string{"--rate", "Inf"}, wantError: "--rate must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseTestFlags(t, tc.args...)
			p, err := cfg.pacingPlan()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("pacingPlan error=%v, want %q", err, tc.wantError)
				}
				// The unreachable target must never be contacted for invalid flags.
				if err := run(cfg); err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("run error=%v, want validation before fetching indexes", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.shape() != tc.shape || p.rampDuration() != tc.ramp {
				t.Errorf("shape=%s ramp=%s, want %s %s", p.shape(), p.rampDuration(), tc.shape, tc.ramp)
			}
			if p.step > 0 && p.stepEvery != tc.every {
				t.Errorf("stepEvery=%s, want %s", p.stepEvery, tc.every)
			}
		})
	}
}

func TestWaitUntilRespondsToCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	if waitUntil(ctx, timer, time.Now().Add(time.Hour)) {
		t.Fatal("canceled wait reported completion")
	}
	if !waitUntil(context.Background(), timer, time.Now().Add(-time.Second)) {
		t.Fatal("elapsed deadline reported cancellation")
	}
}
