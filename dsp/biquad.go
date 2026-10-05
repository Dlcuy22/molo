// Biquad filter design and state.
//
// Coefficients follow Robert Bristow-Johnson's public Audio EQ Cookbook
// formulae using the bilinear transform. This consciously duplicates the
// designBiquad implementation in script/kernel.go because package dsp cannot
// import script without an import cycle.
//
// Clean-room note: all filter forms are derived directly from the public RBJ
// cookbook equations. No GPL or LGPL C code was consulted or translated.

package dsp

import (
	"fmt"
	"math"
	"strings"
)

// biquadState holds direct-form-I filter memory per channel.
type biquadState struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     []float64
}

// newBiquadState allocates direct-form-I memory for ch channels.
func newBiquadState(ch int) *biquadState {
	if ch < 1 {
		ch = 1
	}

	return &biquadState{
		x1: make([]float64, ch),
		x2: make([]float64, ch),
		y1: make([]float64, ch),
		y2: make([]float64, ch),
	}
}

// setCoeffs updates filter coefficients without clearing channel memory.
func (s *biquadState) setCoeffs(b0, b1, b2, a1, a2 float64) {
	s.b0, s.b1, s.b2, s.a1, s.a2 = b0, b1, b2, a1, a2
}

// step processes one sample on the given channel.
// Non-finite results reset that channel's memory to avoid persistent NaN.
func (s *biquadState) step(x float64, ch int) float64 {
	y := s.b0*x + s.b1*s.x1[ch] + s.b2*s.x2[ch] - s.a1*s.y1[ch] - s.a2*s.y2[ch]
	if math.IsNaN(y) || math.IsInf(y, 0) {
		s.x1[ch], s.x2[ch], s.y1[ch], s.y2[ch] = 0, 0, 0, 0

		return 0
	}
	s.x2[ch], s.x1[ch] = s.x1[ch], x
	s.y2[ch], s.y1[ch] = s.y1[ch], y

	return y
}

// reset clears all channel delay memories while preserving coefficients.
func (s *biquadState) reset() {
	clear(s.x1)
	clear(s.x2)
	clear(s.y1)
	clear(s.y2)
}

// designBiquad computes normalized RBJ cookbook coefficients for a biquad filter.
func designBiquad(kind string, rate int, freq, q, gainDB float64) ([5]float64, error) {
	if rate <= 0 {
		rate = 48000
	}
	if !(freq > 0) || !(freq < float64(rate)/2) {
		return [5]float64{}, fmt.Errorf("dsp: biquad freq %v is outside (0, rate/2)", freq)
	}
	if !(q > 0) {
		q = 0.707
	}
	if math.IsNaN(gainDB) || math.IsInf(gainDB, 0) {
		return [5]float64{}, fmt.Errorf("dsp: biquad gainDB %v is non-finite", gainDB)
	}

	kind = strings.ToLower(strings.TrimSpace(kind))

	w0 := 2 * math.Pi * freq / float64(rate)
	cos, sin := math.Cos(w0), math.Sin(w0)
	alpha := sin / (2 * q)

	var b0, b1, b2, a0, a1, a2 float64
	switch kind {
	case "lowpass", "low-pass", "lp":
		b0 = (1 - cos) / 2
		b1 = 1 - cos
		b2 = b0
		a0 = 1 + alpha
		a1 = -2 * cos
		a2 = 1 - alpha

	case "highpass", "high-pass", "hp":
		b0 = (1 + cos) / 2
		b1 = -(1 + cos)
		b2 = b0
		a0 = 1 + alpha
		a1 = -2 * cos
		a2 = 1 - alpha

	case "bandpass", "band-pass", "bp":
		b0 = alpha
		b1 = 0
		b2 = -alpha
		a0 = 1 + alpha
		a1 = -2 * cos
		a2 = 1 - alpha

	case "peaking", "peak", "bell":
		A := math.Pow(10, gainDB/40)
		b0 = 1 + alpha*A
		b1 = -2 * cos
		b2 = 1 - alpha*A
		a0 = 1 + alpha/A
		a1 = -2 * cos
		a2 = 1 - alpha/A

	case "lowshelf", "low-shelf", "ls":
		A := math.Pow(10, gainDB/40)
		twoSqrtAAlpha := 2 * math.Sqrt(A) * alpha
		b0 = A * ((A + 1) - (A-1)*cos + twoSqrtAAlpha)
		b1 = 2 * A * ((A - 1) - (A+1)*cos)
		b2 = A * ((A + 1) - (A-1)*cos - twoSqrtAAlpha)
		a0 = (A + 1) + (A-1)*cos + twoSqrtAAlpha
		a1 = -2 * ((A - 1) + (A+1)*cos)
		a2 = (A + 1) + (A-1)*cos - twoSqrtAAlpha

	case "highshelf", "high-shelf", "hs":
		A := math.Pow(10, gainDB/40)
		twoSqrtAAlpha := 2 * math.Sqrt(A) * alpha
		b0 = A * ((A + 1) + (A-1)*cos + twoSqrtAAlpha)
		b1 = -2 * A * ((A - 1) + (A+1)*cos)
		b2 = A * ((A + 1) + (A-1)*cos - twoSqrtAAlpha)
		a0 = (A + 1) - (A-1)*cos + twoSqrtAAlpha
		a1 = 2 * ((A - 1) - (A+1)*cos)
		a2 = (A + 1) - (A-1)*cos - twoSqrtAAlpha

	case "notch", "bandstop", "band-stop", "bs":
		b0 = 1
		b1 = -2 * cos
		b2 = 1
		a0 = 1 + alpha
		a1 = -2 * cos
		a2 = 1 - alpha

	case "allpass", "all-pass", "ap":
		b0 = 1 - alpha
		b1 = -2 * cos
		b2 = 1 + alpha
		a0 = 1 + alpha
		a1 = -2 * cos
		a2 = 1 - alpha

	default:
		return [5]float64{}, fmt.Errorf("dsp: unknown biquad kind %q", kind)
	}

	return [5]float64{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0}, nil
}
