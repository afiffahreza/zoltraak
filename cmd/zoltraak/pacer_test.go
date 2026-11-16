package main

import (
	"math"
	"testing"
	"time"
)

func TestStepPlanCapAndBoundaries(t *testing.T) {
	p := plan{rate: 5, step: 2, stepEvery: 3 * time.Second, hold: 4 * time.Second}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	if p.shape() != "step" || p.rampSeconds() != 6 || p.duration() != 10*time.Second || p.total() != 38 {
		t.Fatalf("unexpected plan: shape=%s ramp=%g duration=%s total=%d", p.shape(), p.rampSeconds(), p.duration(), p.total())
	}
	for _, tc := range []struct {
		at   time.Duration
		rate float64
	}{
		{-time.Nanosecond, 0},
		{0, 2},
		{3*time.Second - time.Nanosecond, 2},
		{3 * time.Second, 4},
		{6*time.Second - time.Nanosecond, 4},
		{6 * time.Second, 5},
		{9 * time.Second, 5},
	} {
		if got := p.targetRate(tc.at); got != tc.rate {
			t.Errorf("targetRate(%s) = %g, want %g", tc.at, got, tc.rate)
		}
	}
	for _, tc := range []struct {
		n  int
		at time.Duration
	}{
		{0, 0}, {5, 2500 * time.Millisecond}, {6, 3 * time.Second},
		{17, 5750 * time.Millisecond}, {18, 6 * time.Second},
		{37, 9800 * time.Millisecond},
	} {
		if got := p.at(tc.n); got != tc.at {
			t.Errorf("at(%d) = %s, want %s", tc.n, got, tc.at)
		}
	}
}

func TestStepStartsAtPeakWhenStepReachesCap(t *testing.T) {
	for _, step := range []float64{5, 8} {
		p := plan{rate: 5, step: step, stepEvery: time.Hour, hold: 2 * time.Second}
		if p.rampSeconds() != 0 || p.duration() != 2*time.Second || p.total() != 10 || p.targetRate(0) != 5 {
			t.Errorf("step=%g does not start directly at peak", step)
		}
		if got := p.at(9); got != 1800*time.Millisecond {
			t.Errorf("step=%g at(9)=%s", step, got)
		}
	}
}

func TestStepFractionalBudgetsAreNotRoundedPerStage(t *testing.T) {
	p := plan{rate: 1, step: 0.25, stepEvery: time.Second, hold: time.Second}
	if p.total() != 2 {
		t.Fatalf("total()=%d, want floor(0.25+0.5+0.75+1)=2", p.total())
	}
	if got := p.at(1); got != 2*time.Second+333333333*time.Nanosecond {
		t.Fatalf("at(1)=%s, want a deadline within the third stage", got)
	}
	for _, tc := range []struct {
		at     time.Duration
		budget float64
	}{
		{-time.Second, 0}, {0, 0}, {time.Second, 0.25},
		{2 * time.Second, 0.75}, {3 * time.Second, 1.5},
		{3500 * time.Millisecond, 2}, {4 * time.Second, 2.5},
		{10 * time.Second, 2.5},
	} {
		if got := p.cumulativeRequests(tc.at); got != tc.budget {
			t.Errorf("cumulativeRequests(%s)=%g, want %g", tc.at, got, tc.budget)
		}
	}
}

func TestLinearScheduleUnchanged(t *testing.T) {
	p := plan{rate: 4, ramp: 2 * time.Second, hold: time.Second}
	if p.shape() != "linear" || p.total() != 8 || p.duration() != 3*time.Second {
		t.Fatal("linear plan totals changed")
	}
	for n := range p.total() {
		f := float64(n)
		want := time.Duration(math.Sqrt(f) * float64(time.Second))
		if n >= 4 {
			want = time.Duration((1 + f/4) * float64(time.Second))
		}
		if got := p.at(n); got != want {
			t.Errorf("at(%d)=%s, want original linear deadline %s", n, got, want)
		}
	}
	p = plan{rate: 2, hold: time.Second}
	if p.total() != 2 || p.at(0) != 0 || p.at(1) != 500*time.Millisecond || p.targetRate(0) != 2 {
		t.Fatal("zero-ramp linear plan changed")
	}
}

func TestCumulativeWindowAverages(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    plan
		from time.Duration
		to   time.Duration
		want float64
	}{
		{"step boundary", plan{rate: 5, step: 2, stepEvery: 3 * time.Second, hold: 4 * time.Second}, 2 * time.Second, 5 * time.Second, 10.0 / 3},
		{"multiple steps", plan{rate: 5, step: 2, stepEvery: 3 * time.Second, hold: 4 * time.Second}, 0, 7 * time.Second, 23.0 / 7},
		{"linear ramp boundary", plan{rate: 4, ramp: 2 * time.Second, hold: time.Second}, time.Second, 3 * time.Second, 3.5},
		{"draining after hold", plan{rate: 4, ramp: 2 * time.Second, hold: time.Second}, 2500 * time.Millisecond, 4 * time.Second, 4.0 / 3},
		{"after plan", plan{rate: 4, ramp: 2 * time.Second, hold: time.Second}, 4 * time.Second, 5 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (tc.p.cumulativeRequests(tc.to) - tc.p.cumulativeRequests(tc.from)) / (tc.to - tc.from).Seconds()
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("window average=%g, want %g", got, tc.want)
			}
		})
	}
}

func TestStepScheduleInvertsCumulativeBudget(t *testing.T) {
	p := plan{rate: 3.25, step: 0.75, stepEvery: 1500 * time.Millisecond, hold: 2 * time.Second}
	last := time.Duration(-1)
	for n := range p.total() {
		at := p.at(n)
		if at < last || at >= p.duration() {
			t.Fatalf("at(%d)=%s is outside the ordered schedule", n, at)
		}
		if got := p.cumulativeRequests(at); math.Abs(got-float64(n)) > p.rate/float64(time.Second) {
			t.Errorf("cumulativeRequests(at(%d))=%g", n, got)
		}
		last = at
	}
}

func TestPlanRejectsUnrepresentableValues(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)
	for _, p := range []plan{
		{rate: math.NaN(), hold: time.Second},
		{rate: math.Inf(1), hold: time.Second},
		{rate: 1, step: math.NaN(), hold: time.Second},
		{rate: 1, step: math.Inf(1), hold: time.Second},
		{rate: 1, step: 0.5, stepEvery: 0, hold: time.Second},
		{rate: 1, ramp: maxDuration, hold: time.Second},
		{rate: 2, step: 1, stepEvery: maxDuration, hold: time.Second},
		{rate: 1, step: math.SmallestNonzeroFloat64, stepEvery: time.Second},
		{rate: math.MaxFloat64, hold: time.Second},
		{rate: 0.1, hold: time.Second},
	} {
		if err := p.validate(); err == nil {
			t.Errorf("validate(%+v) succeeded", p)
		}
	}
}

func TestDecimalStepsDoNotAddSpuriousStage(t *testing.T) {
	p := plan{rate: 0.07, step: 0.01, stepEvery: time.Second, hold: time.Minute}
	if got := p.rampDuration(); got != 6*time.Second {
		t.Fatalf("rampDuration()=%s, want six intervals below peak", got)
	}
	if got := p.targetRate(p.rampDuration()); got != p.rate {
		t.Fatalf("rate at peak=%g, want %g", got, p.rate)
	}
}
