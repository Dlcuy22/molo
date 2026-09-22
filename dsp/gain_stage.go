package dsp

import "math"

// The standard per-effect gains are applied by the framework rather than by
// each implementation, so every effect trims the same way and an implementation
// that forgets them cannot exist. An effect carries a gainState published the
// same way as its own parameters, and its Process wraps its work in applyGains.

// gainState is the compiled form of the two gain parameters. A zero value means
// both are unity, so an effect that never sets them pays nothing.
type gainState struct {
	in  float32
	out float32
}

// compileGains reads the standard gain parameters from a store and converts
// them to linear factors. It is called whenever the store changes.
func compileGains(store *paramStore) gainState {
	return gainState{
		in:  float32(dbToLinear(store.Float(ParamInputGain))),
		out: float32(dbToLinear(store.Float(ParamOutputGain))),
	}
}

// applyGains runs the input gain, then fn, then the output gain over the first
// frames interleaved samples of buf. Bypass is the caller's decision, not this
// helper's, because a bypassed effect must skip its own work as well.
func applyGains(buf []float32, frames, ch int, g gainState, fn func()) {
	n := frames * ch
	if n > len(buf) {
		n = len(buf)
	}
	if n <= 0 {
		return
	}
	n -= n % ch

	// The common case is both gains at unity, which is a no-op and lets an
	// effect avoid a multiply per sample.
	if g.in == 1 && g.out == 1 {
		fn()

		return
	}
	if g.in != 1 {
		for i := range buf[:n] {
			buf[i] *= g.in
		}
	}
	fn()
	if g.out != 1 {
		for i := range buf[:n] {
			buf[i] *= g.out
		}
	}
}

// dbToLinear converts a dBFS gain to a linear multiplier. It lives here rather
// than in an implementation because the standard gains need it and every
// implementation that wants its own dB parameter needs the same conversion.
func dbToLinear(db float64) float64 {
	if db == 0 {
		return 1
	}

	return math.Pow(10, db/20)
}
