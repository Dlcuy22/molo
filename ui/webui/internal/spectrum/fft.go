// This file is a small real-signal FFT for display use. It exists because the
// player has no spectral analysis and a visualizer needs one: the only other
// analysis in the tree (package analysis) decodes a file offline, which is the
// wrong tool for a live feed.
//
// The transform is iterative radix-2, which is all a visualizer needs: a
// 2048-point frame at 30 fps is a rounding error next to the decoder, so a
// mixed-radix or a lookup-table implementation would be complexity without a
// user-visible payoff.
package spectrum

import (
	"errors"
	"math"
)

// ErrFFTSize reports a transform length that is not a positive power of two, which
// radix-2 cannot decompose.
var ErrFFTSize = errors.New("fft: size must be a positive power of two")

// Analyzer transforms fixed-size real frames. It holds its scratch buffers, so
// a caller that reuses one Analyzer never allocates on the analysis path.
type Analyzer struct {
	size int
	// window is the Hann window applied before the transform. It trades the
	// rectangular window's spectral leakage (which smears a tone across every
	// bin, so a sine reads as a blur) for a slightly wider main lobe, which is
	// the right trade for a display.
	window []float64

	re, im []float64

	// cos/sin are the twiddle factors for the forward transform, precomputed
	// once so a frame costs only butterflies.
	cos, sin []float64
	rev      []int
}

// NewAnalyzer returns an Analyzer for size-point frames. size must be a positive power
// of two.
func NewAnalyzer(size int) (*Analyzer, error) {
	if size < 2 || size&(size-1) != 0 {
		return nil, ErrFFTSize
	}

	a := &Analyzer{
		size:   size,
		window: make([]float64, size),
		re:     make([]float64, size),
		im:     make([]float64, size),
		cos:    make([]float64, size/2),
		sin:    make([]float64, size/2),
		rev:    make([]int, size),
	}
	for i := range size {
		// Periodic Hann, the form that reconstructs cleanly at the frame edges.
		a.window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(size))
	}
	for i := range size {
		a.rev[i] = reverseBits(i, size)
	}
	for i := range size / 2 {
		angle := -2 * math.Pi * float64(i) / float64(size)
		a.cos[i] = math.Cos(angle)
		a.sin[i] = math.Sin(angle)
	}

	return a, nil
}

// Size is the frame length the Analyzer accepts.
func (a *Analyzer) Size() int { return a.size }

// Bins is the number of usable magnitude bins, size/2: the DC bin through the
// Nyquist bin, the spectrum of a real signal being symmetric about Nyquist.
func (a *Analyzer) Bins() int { return a.size / 2 }

// Magnitudes transforms the first Size samples and writes their magnitudes to
// out, low frequency first. It returns the number of bins written, which is
// Bins() when out is long enough.
//
// A short input is windowed and zero-padded rather than rejected, so a caller
// that has not yet buffered a whole frame gets a quieter but valid spectrum
// instead of an error path on every early call.
func (a *Analyzer) Magnitudes(samples []float32, out []float64) int {
	n := a.size
	for i := range n {
		var s float64
		if i < len(samples) {
			s = float64(samples[i]) * a.window[i]
		}
		a.re[i] = s
		a.im[i] = 0
	}

	a.transform()

	bins := min(a.Bins(), len(out))
	for k := range bins {
		a.re[k] = math.Hypot(a.re[k], a.im[k])
	}
	// The magnitude scale of a forward transform grows with N; normalising by
	// N and the window's coherent gain keeps the result comparable across
	// frame sizes, so a level change never rides a config change.
	scale := 2 / (float64(n) * 0.5)
	for k := range bins {
		out[k] = a.re[k] * scale
	}

	return bins
}

// transform runs the in-place iterative radix-2 Cooley-Tukey FFT over the
// prepared re/im buffers.
func (a *Analyzer) transform() {
	n := a.size
	for i := range n {
		if j := a.rev[i]; i < j {
			a.re[i], a.re[j] = a.re[j], a.re[i]
			a.im[i], a.im[j] = a.im[j], a.im[i]
		}
	}

	for length := 2; length <= n; length <<= 1 {
		half := length / 2
		step := n / length
		for start := 0; start < n; start += length {
			for k := range half {
				tw := k * step
				wr, wi := a.cos[tw], a.sin[tw]
				even := start + k
				odd := even + half
				ur, ui := a.re[even], a.im[even]
				vr := a.re[odd]*wr - a.im[odd]*wi
				vi := a.re[odd]*wi + a.im[odd]*wr
				a.re[even] = ur + vr
				a.im[even] = ui + vi
				a.re[odd] = ur - vr
				a.im[odd] = ui - vi
			}
		}
	}
}

// reverseBits returns the index whose low log2(n) bits are those of i reversed,
// which is the bit-reversal permutation the iterative transform expects. The
// source is walked from its low bit up, each step shifting the result left, so
// the result's high bit is the source's low bit.
func reverseBits(i, n int) int {
	var out int
	for k := 1; k < n; k <<= 1 {
		out <<= 1
		if i&k != 0 {
			out |= 1
		}
	}

	return out
}
