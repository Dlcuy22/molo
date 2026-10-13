// Crossfade gain curve.
//
// A crossfade overlaps two tracks: the outgoing one fades out while the
// incoming one fades in. The two gains are the only thing this file decides.
// The mixing itself lives on the real-time path, where the two streams are
// summed; keeping the curve here as a pure function means it is testable
// without a session, a device or a streamer.
//
// The curve is equal-power, not linear. Two uncorrelated tracks summed with
// linear gains dip about 3 dB in the middle of the blend, because the perceived
// loudness of a sum is the power sum, not the amplitude sum. cos/sin over a
// quarter turn keeps the power constant: cos^2 + sin^2 == 1, so the blend holds
// its level from one track to the other. This is the standard choice for a
// musical crossfade.

package dsp

import "math"

// CrossfadeGains returns the outgoing and incoming gains at t, where t runs
// from 0 (all outgoing) to 1 (all incoming). Values outside [0, 1] are clamped,
// so a caller that overshoots its own ramp ends at a clean endpoint rather than
// a negative or amplified gain.
//
// The returned pair satisfies out^2 + in^2 == 1 across the ramp, which is what
// keeps a crossfade between uncorrelated material at a constant perceived
// level. At t == 0 it is (1, 0) and at t == 1 it is (0, 1).
func CrossfadeGains(t float64) (out, in float64) {
	switch {
	case t <= 0:
		return 1, 0
	case t >= 1:
		return 0, 1
	}

	angle := t * math.Pi / 2

	return math.Cos(angle), math.Sin(angle)
}
