// This file adds a vectorized real-input FFT behind the same Magnitudes
// contract as the scalar Analyzer in fft.go, and the selector that picks
// between them.
//
// The vectorized path is the f32.STFTPlan from github.com/tphakala/simd: the
// frame is packed with Deinterleave2 + Mul, the core is radix-4
// ButterflyComplexStage4, and the real-input unravel is RealFFTUnpack. Every
// arithmetic pass runs through the library's AVX+FMA / NEON kernels, with a
// pure-Go fallback on other architectures.
//
// The scalar Analyzer stays as the reference implementation and the fallback
// for a CPU with neither AVX nor NEON, where the plan's own scalar path would
// not beat the radix-2 loop.
package spectrum

import (
	"math"
	"runtime"

	"github.com/tphakala/simd/cpu"
	"github.com/tphakala/simd/f32"
)

// Transform is the FFT surface the Runner needs. Both the scalar Analyzer and
// the vectorized simdAnalyzer satisfy it, so the visualizer is agnostic to
// which one the CPU picked.
type Transform interface {
	// Magnitudes transforms the first Size samples and writes their magnitudes
	// to out, low frequency first. A short input is windowed and zero-padded.
	Magnitudes(samples []float32, out []float64) int
	// Bins is the number of usable magnitude bins, size/2.
	Bins() int
	// Size is the frame length the transform accepts.
	Size() int
}

// NewTransform returns the fastest transform available for size-point frames:
// the vectorized plan when the CPU has SIMD wide enough to beat the scalar
// loop, and the scalar radix-2 Analyzer otherwise. size must be a positive
// power of two.
func NewTransform(size int) (Transform, error) {
	if useSIMD() {
		if a, err := newSIMDAnalyzer(size); err == nil {
			return a, nil
		}
		// A size the plan rejects (it should not, given the same power-of-two
		// contract) falls through to the scalar path rather than failing the
		// display.
	}

	return NewAnalyzer(size)
}

// useSIMD reports whether the vectorized plan is expected to win. The plan's
// kernels need AVX+FMA on amd64 or NEON on arm64 to beat the scalar loop; on
// anything else its pure-Go fallback would be slower, so the scalar radix-2 is
// the better choice.
func useSIMD() bool {
	switch runtime.GOARCH {
	case "amd64":
		return cpu.HasAVX()
	case "arm64":
		return cpu.HasNEON()
	default:
		return false
	}
}

// simdAnalyzer is the vectorized transform. It owns the plan's scratch and its
// own window and spectrum buffers, so a steady display allocates nothing per
// frame. Like the plan itself, one simdAnalyzer is not safe for concurrent use;
// the Runner calls it from its single ticker goroutine.
type simdAnalyzer struct {
	size   int
	plan   *f32.STFTPlan
	window []float32
	spec   []complex64
}

// newSIMDAnalyzer builds a vectorized transform for size-point frames.
func newSIMDAnalyzer(size int) (*simdAnalyzer, error) {
	if size < 2 || size&(size-1) != 0 {
		return nil, ErrFFTSize
	}
	plan, err := f32.NewSTFTPlan(size)
	if err != nil {
		return nil, err
	}
	a := &simdAnalyzer{
		size:   size,
		plan:   plan,
		window: make([]float32, size),
		spec:   make([]complex64, size/2+1),
	}
	for i := range a.window {
		// The same periodic Hann the scalar Analyzer applies.
		a.window[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(size)))
	}

	return a, nil
}

// Size is the frame length the analyzer accepts.
func (a *simdAnalyzer) Size() int { return a.size }

// Bins is the number of usable magnitude bins, size/2, matching the scalar
// analyzer (DC through size/2-1, Nyquist excluded).
func (a *simdAnalyzer) Bins() int { return a.size / 2 }

// Magnitudes transforms the first Size samples and writes their magnitudes to
// out, matching the scalar Analyzer's contract: the same periodic Hann window,
// the same 4/N magnitude scale, and a short frame zero-padded.
func (a *simdAnalyzer) Magnitudes(samples []float32, out []float64) int {
	// RFFT windows the frame and (for a short frame) zero-pads it, then writes
	// the Hermitian half-spectrum, so no separate window pass is needed.
	a.plan.RFFT(a.spec, samples, a.window)

	bins := min(a.Bins(), len(out))
	// The forward transform's magnitude grows with N; normalising by N and the
	// window's coherent gain keeps a level change from riding a config change,
	// the same 4/N the scalar path uses.
	scale := 2 / (float64(a.size) * 0.5)
	for k := range bins {
		out[k] = math.Hypot(float64(real(a.spec[k])), float64(imag(a.spec[k]))) * scale
	}

	return bins
}
