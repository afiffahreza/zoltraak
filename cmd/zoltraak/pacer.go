package main

import (
	"errors"
	"math"
	"time"
)

// plan describes either a linear ramp or immediate, capped rate steps, followed
// by a hold at the peak rate. A zero step selects the linear shape.
type plan struct {
	rate      float64 // peak requests per second
	ramp      time.Duration
	hold      time.Duration
	step      float64
	stepEvery time.Duration
}

func (p plan) shape() string {
	if p.step > 0 {
		return "step"
	}
	return "linear"
}

// stepStages counts the full intervals below peak; the peak itself lasts hold.
func (p plan) stepStages() float64 {
	k := math.Max(0, math.Ceil(p.rate/p.step)-1)
	// Division may round an integral ratio just above an integer. Match the
	// same capped multiplication used by targetRate instead of adding a stage.
	if k > 0 && p.step*k >= p.rate {
		k--
	} else if p.step*(k+1) < p.rate {
		k++
	}
	return k
}

func (p plan) rampDuration() time.Duration {
	if p.step > 0 {
		return time.Duration(p.stepStages()) * p.stepEvery
	}
	return p.ramp
}

func (p plan) rampSeconds() float64 { return p.rampDuration().Seconds() }

func (p plan) rampRequests() float64 {
	if p.step > 0 {
		k := p.stepStages()
		return (p.step * k) * ((k + 1) / 2) * p.stepEvery.Seconds()
	}
	return p.rate * p.rampSeconds() / 2
}

// validate checks all conversions before scheduling or allocating request lists.
func (p plan) validate() error {
	if math.IsNaN(p.rate) || math.IsInf(p.rate, 0) || p.rate <= 0 {
		return errors.New("--rate must be finite and > 0")
	}
	if math.IsNaN(p.step) || math.IsInf(p.step, 0) || p.step < 0 {
		return errors.New("--step must be finite and > 0")
	}
	if p.hold < 0 || (p.step == 0 && p.ramp < 0) {
		return errors.New("--ramp and --hold must be >= 0")
	}
	const maxDuration = time.Duration(1<<63 - 1)
	if p.step > 0 {
		if p.stepEvery <= 0 {
			return errors.New("--step-every must be > 0")
		}
		k := p.stepStages()
		if math.IsInf(k, 0) || k >= float64(maxDuration) || int64(k) > int64((maxDuration-p.hold)/p.stepEvery) {
			return errors.New("plan duration is too large")
		}
	} else if p.ramp > maxDuration-p.hold {
		return errors.New("plan duration is too large")
	}
	budget := p.rampRequests() + p.rate*p.hold.Seconds()
	if math.IsNaN(budget) || math.IsInf(budget, 0) || budget >= float64(int(^uint(0)>>1)) {
		return errors.New("plan request count is too large")
	}
	if budget < 1 {
		return errors.New("plan issues zero requests")
	}
	return nil
}

// total rounds only the complete cumulative budget, never individual stages.
func (p plan) total() int { return int(p.rampRequests() + p.rate*p.hold.Seconds()) }

func (p plan) duration() time.Duration { return p.rampDuration() + p.hold }

// at inverts the cumulative request curve for request n (0-based), preserving
// the immediate first request and the original linear schedule.
func (p plan) at(n int) time.Duration {
	f := float64(n)
	var t float64
	if p.step > 0 {
		k := p.stepStages()
		every := p.stepEvery.Seconds()
		rampBudget := (p.step * k) * ((k + 1) / 2) * every
		if f >= rampBudget {
			t = (time.Duration(k) * p.stepEvery).Seconds() + (f-rampBudget)/p.rate
		} else {
			unit := p.step * every
			x := f / unit
			stage := math.Floor(4 * x / (math.Sqrt(1+8*x) + 1))
			before := unit * stage * (stage + 1) / 2
			// Correct floating-point error when n falls exactly on a boundary.
			if stage > 0 && before > f {
				stage--
				before = unit * stage * (stage + 1) / 2
			} else if next := unit * (stage + 1) * (stage + 2) / 2; next <= f {
				stage++
				before = next
			}
			t = stage*every + (f-before)/(p.step*(stage+1))
		}
	} else if f < p.rampRequests() {
		t = math.Sqrt(2 * p.rampSeconds() * f / p.rate)
	} else {
		t = p.rampSeconds()/2 + f/p.rate
	}
	ns := t * float64(time.Second)
	if ns >= float64(time.Duration(1<<63-1)) {
		return p.duration()
	}
	return time.Duration(ns)
}

// cumulativeRequests integrates the planned rate, clipped to the dispatch phase.
func (p plan) cumulativeRequests(t time.Duration) float64 {
	if t <= 0 {
		return 0
	}
	if t >= p.duration() {
		return p.rampRequests() + p.rate*p.hold.Seconds()
	}
	if t >= p.rampDuration() {
		return p.rampRequests() + p.rate*(t-p.rampDuration()).Seconds()
	}
	if p.step > 0 {
		stage := float64(t / p.stepEvery)
		return (p.step*stage)*((stage+1)/2)*p.stepEvery.Seconds() +
			p.step*(stage+1)*(t%p.stepEvery).Seconds()
	}
	return p.rate * t.Seconds() * t.Seconds() / (2 * p.rampSeconds())
}

// targetRate is the instantaneous planned rate at offset t.
func (p plan) targetRate(t time.Duration) float64 {
	if t < 0 {
		return 0
	}
	if p.step > 0 {
		return math.Min(p.rate, p.step*(float64(t/p.stepEvery)+1))
	}
	if t >= p.ramp {
		return p.rate
	}
	return p.rate * t.Seconds() / p.rampSeconds()
}
