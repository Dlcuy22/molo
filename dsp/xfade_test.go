package dsp

import (
	"math"
	"testing"
)

// TestCrossfadeGainsEndpoints pins the two ends of the ramp: at the start the
// outgoing track carries everything, at the end the incoming one does.
func TestCrossfadeGainsEndpoints(t *testing.T) {
	out, in := CrossfadeGains(0)
	if out != 1 || in != 0 {
		t.Fatalf("CrossfadeGains(0) = (%v, %v), want (1, 0)", out, in)
	}

	out, in = CrossfadeGains(1)
	if out != 0 || in != 1 {
		t.Fatalf("CrossfadeGains(1) = (%v, %v), want (0, 1)", out, in)
	}
}

// TestCrossfadeGainsClampsOutOfRange keeps a caller that overshoots its ramp
// from producing a negative or amplified gain.
func TestCrossfadeGainsClampsOutOfRange(t *testing.T) {
	out, in := CrossfadeGains(-0.5)
	if out != 1 || in != 0 {
		t.Fatalf("CrossfadeGains(-0.5) = (%v, %v), want (1, 0)", out, in)
	}

	out, in = CrossfadeGains(1.5)
	if out != 0 || in != 1 {
		t.Fatalf("CrossfadeGains(1.5) = (%v, %v), want (0, 1)", out, in)
	}
}

// TestCrossfadeGainsIsEqualPower is the property that makes the curve
// equal-power: the sum of the squared gains stays at unity across the whole
// ramp, so two uncorrelated tracks do not dip in the middle.
func TestCrossfadeGainsIsEqualPower(t *testing.T) {
	for i := 0; i <= 100; i++ {
		tt := float64(i) / 100
		out, in := CrossfadeGains(tt)
		power := out*out + in*in
		if math.Abs(power-1) > 1e-9 {
			t.Fatalf("CrossfadeGains(%v): out^2+in^2 = %v, want 1", tt, power)
		}
	}
}

// TestCrossfadeGainsIsMonotone pins that each side moves in one direction:
// the outgoing gain never rises and the incoming gain never falls, so the blend
// cannot wobble.
func TestCrossfadeGainsIsMonotone(t *testing.T) {
	prevOut, prevIn := CrossfadeGains(0)
	for i := 1; i <= 100; i++ {
		tt := float64(i) / 100
		out, in := CrossfadeGains(tt)
		if out > prevOut {
			t.Fatalf("outgoing gain rose at t=%v: %v > %v", tt, out, prevOut)
		}
		if in < prevIn {
			t.Fatalf("incoming gain fell at t=%v: %v < %v", tt, in, prevIn)
		}
		prevOut, prevIn = out, in
	}
}

// TestCrossfadeGainsMidpoint pins the exact middle, so a later change to the
// curve is caught rather than absorbed by the property tests. The equal-power
// midpoint is 1/sqrt(2) on both sides, about -3 dB each, summing to unity power.
func TestCrossfadeGainsMidpoint(t *testing.T) {
	out, in := CrossfadeGains(0.5)
	want := 1 / math.Sqrt2
	if math.Abs(out-want) > 1e-12 || math.Abs(in-want) > 1e-12 {
		t.Fatalf("CrossfadeGains(0.5) = (%v, %v), want (%v, %v)", out, in, want, want)
	}
}
