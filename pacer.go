package main

import (
	"math"
	"time"
)

// plan describes an open-loop load shape: a linear ramp from 0 to rate over
// ramp, followed by a constant hold at rate.
type plan struct {
	rate float64 // peak requests per second
	ramp time.Duration
	hold time.Duration
}

func (p plan) rampSeconds() float64 { return p.ramp.Seconds() }

// rampRequests is the number of requests issued during the ramp:
// the integral of rate*t/ramp over [0, ramp].
func (p plan) rampRequests() float64 { return p.rate * p.rampSeconds() / 2 }

// total is the number of requests the whole plan issues.
func (p plan) total() int {
	return int(p.rampRequests() + p.rate*p.hold.Seconds())
}

// duration is the planned wall time of the dispatch phase.
func (p plan) duration() time.Duration { return p.ramp + p.hold }

// at returns the offset from start at which request n (0-based) is due.
// It inverts the cumulative request curve N(t) = rate*t²/(2*ramp) during the
// ramp and N(t) = rate*(t - ramp/2) afterwards.
func (p plan) at(n int) time.Duration {
	f := float64(n)
	var t float64
	if f < p.rampRequests() {
		t = math.Sqrt(2 * p.rampSeconds() * f / p.rate)
	} else {
		t = p.rampSeconds()/2 + f/p.rate
	}
	return time.Duration(t * float64(time.Second))
}

// targetRate is the instantaneous planned rate at offset t.
func (p plan) targetRate(t time.Duration) float64 {
	if t >= p.ramp {
		return p.rate
	}
	return p.rate * t.Seconds() / p.rampSeconds()
}
