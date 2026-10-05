// Envelope detector implementation.
//
// A one-pole attack and release envelope detector supporting peak and RMS
// modes. It provides sample-by-sample and frame-based envelope tracking for
// dynamics processors.
//
// Clean-room note: this file implements the standard peak and RMS one-pole
// envelope detector from public signal processing theory. It is not a
// translation of any GPL or LGPL C code.

package dsp

import "math"

type envMode int

const (
	envPeak envMode = iota
	envRMS
)

// envState tracks signal level using one-pole attack and release smoothing.
// An instance performs no heap allocations but is not safe for concurrent use.
type envState struct {
	attack  float64
	release float64
	mode    envMode
	env     float64
}

// newEnvState calculates smoothing coefficients and returns an initialised detector.
func newEnvState(attackSec, releaseSec float64, rate int, mode envMode) *envState {
	s := &envState{mode: mode}
	s.setAttackRelease(attackSec, releaseSec, rate)
	return s
}

// setAttackRelease updates smoothing coefficients without resetting the envelope.
func (s *envState) setAttackRelease(attackSec, releaseSec float64, rate int) {
	if rate <= 0 {
		rate = 48000
	}
	s.attack = envCoeff(attackSec, rate)
	s.release = envCoeff(releaseSec, rate)
}

// envCoeff maps a time constant in seconds to a one-pole smoothing factor.
func envCoeff(sec float64, rate int) float64 {
	if sec <= 0 || math.IsNaN(sec) {
		return 1.0
	}
	c := 1.0 - math.Exp(-1.0/(sec*float64(rate)))
	if c > 1.0 {
		return 1.0
	}
	return c
}

// step processes one sample and updates the envelope value.
// It performs no allocations and is not safe for concurrent use.
func (s *envState) step(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		x = 0
	}
	var v float64
	if s.mode == envPeak {
		v = math.Abs(x)
	} else {
		v = x * x
	}

	if v > s.env {
		if s.attack == 1.0 {
			s.env = v
		} else {
			s.env += s.attack * (v - s.env)
		}
	} else {
		if s.release == 1.0 {
			s.env = v
		} else {
			s.env += s.release * (v - s.env)
		}
	}

	if s.mode == envRMS {
		return math.Sqrt(s.env)
	}
	return s.env
}

// stepFrame processes a block of samples and updates the envelope value.
// Time constants are per call; callers wanting per-sample smoothing call step per sample.
func (s *envState) stepFrame(frame []float64) float64 {
	var v float64
	if s.mode == envPeak {
		for _, x := range frame {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				x = 0
			}
			ax := math.Abs(x)
			if ax > v {
				v = ax
			}
		}
	} else {
		if len(frame) > 0 {
			var sumSq float64
			for _, x := range frame {
				if math.IsNaN(x) || math.IsInf(x, 0) {
					x = 0
				}
				sumSq += x * x
			}
			v = sumSq / float64(len(frame))
		}
	}

	if v > s.env {
		if s.attack == 1.0 {
			s.env = v
		} else {
			s.env += s.attack * (v - s.env)
		}
	} else {
		if s.release == 1.0 {
			s.env = v
		} else {
			s.env += s.release * (v - s.env)
		}
	}

	if s.mode == envRMS {
		return math.Sqrt(s.env)
	}
	return s.env
}

// reset clears the tracked envelope value to zero.
func (s *envState) reset() {
	s.env = 0
}
