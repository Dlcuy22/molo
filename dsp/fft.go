// Clean-room note: this file implements the FFT kernel as a thin wrapper over
// github.com/tphakala/simd/f32 (radix-4 STFT with SIMD vectorization) with an
// independent scalar Cooley-Tukey radix-2 fallback for architectures without
// AVX/NEON vector extensions. It is not a translation of any GPL/LGPL C code.

package dsp

import (
	"errors"
	"math"
	"runtime"

	"github.com/tphakala/simd/c64"
	"github.com/tphakala/simd/cpu"
	"github.com/tphakala/simd/f32"
)

var errFFTSize = errors.New("fft: size must be a positive power of two")

// useSIMD reports whether SIMD acceleration is available and expected to beat
// scalar Go execution. Matches the webui spectrum backend selector.
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

// fftPlan executes forward (RFFT) and inverse (IRFFT) transforms for a fixed
// frame size. All scratch buffers are preallocated at construction time, so
// forward and inverse operations perform zero heap allocations.
//
// A plan instance is NOT safe for concurrent use across multiple goroutines;
// each concurrent processor or channel must own its own plan instance.
type fftPlan struct {
	nfft   int
	simd   *f32.STFTPlan
	scalar *scalarFFT
}

// newFFT returns a transform plan for size-point frames. size must be a positive
// power of two >= 2. SIMD acceleration is chosen when supported by the CPU;
// otherwise an iterative radix-2 scalar plan is used.
func newFFT(size int) (*fftPlan, error) {
	if size < 2 || size&(size-1) != 0 {
		return nil, errFFTSize
	}
	if useSIMD() {
		if p, err := newSIMDFFTPlan(size); err == nil {
			return p, nil
		}
	}
	return newScalarFFTPlan(size)
}

// newSIMDFFTPlan builds a SIMD-backed plan.
func newSIMDFFTPlan(size int) (*fftPlan, error) {
	if size < 2 || size&(size-1) != 0 {
		return nil, errFFTSize
	}
	plan, err := f32.NewSTFTPlan(size)
	if err != nil {
		return nil, err
	}
	return &fftPlan{
		nfft: size,
		simd: plan,
	}, nil
}

// newScalarFFTPlan builds a scalar-backed plan.
func newScalarFFTPlan(size int) (*fftPlan, error) {
	if size < 2 || size&(size-1) != 0 {
		return nil, errFFTSize
	}
	s, err := newScalarFFT(size)
	if err != nil {
		return nil, err
	}
	return &fftPlan{
		nfft:   size,
		scalar: s,
	}, nil
}

// forward computes the real-input discrete Fourier transform of frame,
// writing up to size/2+1 Hermitian half-spectrum bins to dst. A frame shorter
// than size is zero-padded. Returns the number of bins written.
//
// forward does not allocate memory on the heap.
func (p *fftPlan) forward(dst []complex64, frame []float32) int {
	if p.simd != nil {
		return p.simd.RFFT(dst, frame, nil)
	}
	return p.scalar.forward(dst, frame)
}

// inverse computes the real-output inverse discrete Fourier transform of spec,
// writing up to size real samples to dst, scaled by 1/size so that
// inverse(dst, forward(spec, frame)) reproduces frame within float32 tolerance.
// Returns the number of samples written.
//
// inverse does not allocate memory on the heap.
func (p *fftPlan) inverse(dst []float32, spec []complex64) int {
	if p.simd != nil {
		return p.simd.IRFFT(dst, spec)
	}
	return p.scalar.inverse(dst, spec)
}

// size returns the transform length N.
func (p *fftPlan) size() int {
	return p.nfft
}

// bins returns the number of Hermitian half-spectrum bins, N/2 + 1.
func (p *fftPlan) bins() int {
	return p.nfft/2 + 1
}

// addComplex computes dst[i] = a[i] + b[i]; dst may alias a or b.
func addComplex(dst, a, b []complex64) {
	n := min(len(dst), len(a), len(b))
	if n == 0 {
		return
	}
	if useSIMD() {
		c64.Add(dst[:n], a[:n], b[:n])
		return
	}
	addComplexScalar(dst[:n], a[:n], b[:n])
}

func addComplexScalar(dst, a, b []complex64) {
	n := min(len(dst), len(a), len(b))
	for i := range n {
		dst[i] = a[i] + b[i]
	}
}

// mulComplex computes element-wise multiplication dst[i] = a[i] * b[i].
// dst may alias a or b.
func mulComplex(dst, a, b []complex64) {
	n := min(len(dst), len(a), len(b))
	if n == 0 {
		return
	}
	if useSIMD() {
		c64.Mul(dst[:n], a[:n], b[:n])
		return
	}
	mulComplexScalar(dst[:n], a[:n], b[:n])
}

func mulComplexScalar(dst, a, b []complex64) {
	n := min(len(dst), len(a), len(b))
	for i := range n {
		dst[i] = a[i] * b[i]
	}
}

// mulConjComplex computes element-wise multiplication dst[i] = a[i] * conj(b[i]).
// dst may alias a or b.
func mulConjComplex(dst, a, b []complex64) {
	n := min(len(dst), len(a), len(b))
	if n == 0 {
		return
	}
	if useSIMD() {
		c64.MulConj(dst[:n], a[:n], b[:n])
		return
	}
	mulConjComplexScalar(dst[:n], a[:n], b[:n])
}

func mulConjComplexScalar(dst, a, b []complex64) {
	n := min(len(dst), len(a), len(b))
	for i := range n {
		ar, ai := real(a[i]), imag(a[i])
		br, bi := real(b[i]), imag(b[i])
		dst[i] = complex(ar*br+ai*bi, ai*br-ar*bi)
	}
}

// mulComplexSplit computes element-wise split complex multiplication using
// separate real and imaginary slices.
func mulComplexSplit(dstRe, dstIm, aRe, aIm, bRe, bIm []float32) {
	n := min(len(dstRe), len(dstIm), len(aRe), len(aIm), len(bRe), len(bIm))
	if n == 0 {
		return
	}
	if useSIMD() {
		f32.MulComplex(dstRe[:n], dstIm[:n], aRe[:n], aIm[:n], bRe[:n], bIm[:n])
		return
	}
	mulComplexSplitScalar(dstRe[:n], dstIm[:n], aRe[:n], aIm[:n], bRe[:n], bIm[:n])
}

func mulComplexSplitScalar(dstRe, dstIm, aRe, aIm, bRe, bIm []float32) {
	n := min(len(dstRe), len(dstIm), len(aRe), len(aIm), len(bRe), len(bIm))
	for i := range n {
		ar, ai := aRe[i], aIm[i]
		br, bi := bRe[i], bIm[i]
		dstRe[i] = ar*br - ai*bi
		dstIm[i] = ar*bi + ai*br
	}
}

// mulConjComplexSplit computes element-wise split complex multiplication with
// the conjugate of b: dst = a * conj(b).
func mulConjComplexSplit(dstRe, dstIm, aRe, aIm, bRe, bIm []float32) {
	n := min(len(dstRe), len(dstIm), len(aRe), len(aIm), len(bRe), len(bIm))
	if n == 0 {
		return
	}
	if useSIMD() {
		f32.MulConjComplex(dstRe[:n], dstIm[:n], aRe[:n], aIm[:n], bRe[:n], bIm[:n])
		return
	}
	mulConjComplexSplitScalar(dstRe[:n], dstIm[:n], aRe[:n], aIm[:n], bRe[:n], bIm[:n])
}

func mulConjComplexSplitScalar(dstRe, dstIm, aRe, aIm, bRe, bIm []float32) {
	n := min(len(dstRe), len(dstIm), len(aRe), len(aIm), len(bRe), len(bIm))
	for i := range n {
		ar, ai := aRe[i], aIm[i]
		br, bi := bRe[i], bIm[i]
		dstRe[i] = ar*br + ai*bi
		dstIm[i] = ai*br - ar*bi
	}
}

// scalarFFT implements a power-of-two iterative radix-2 Cooley-Tukey FFT in pure Go.
type scalarFFT struct {
	size int
	re   []float32
	im   []float32
	cos  []float32
	sin  []float32
	rev  []int
}

func newScalarFFT(size int) (*scalarFFT, error) {
	if size < 2 || size&(size-1) != 0 {
		return nil, errFFTSize
	}
	s := &scalarFFT{
		size: size,
		re:   make([]float32, size),
		im:   make([]float32, size),
		cos:  make([]float32, size/2),
		sin:  make([]float32, size/2),
		rev:  make([]int, size),
	}
	for i := range size {
		s.rev[i] = reverseBits(i, size)
	}
	for i := range size / 2 {
		angle := -2 * math.Pi * float64(i) / float64(size)
		s.cos[i] = float32(math.Cos(angle))
		s.sin[i] = float32(math.Sin(angle))
	}
	return s, nil
}

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

func (s *scalarFFT) transform(re, im []float32) {
	n := s.size
	for i := range n {
		if j := s.rev[i]; i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}

	for length := 2; length <= n; length <<= 1 {
		half := length / 2
		step := n / length
		for start := 0; start < n; start += length {
			for k := range half {
				tw := k * step
				wr, wi := s.cos[tw], s.sin[tw]
				even := start + k
				odd := even + half
				ur, ui := re[even], im[even]
				vr := re[odd]*wr - im[odd]*wi
				vi := re[odd]*wi + im[odd]*wr
				re[even] = ur + vr
				im[even] = ui + vi
				re[odd] = ur - vr
				im[odd] = ui - vi
			}
		}
	}
}

func (s *scalarFFT) forward(dst []complex64, frame []float32) int {
	n := s.size
	nb := min(len(dst), n/2+1)
	if nb == 0 {
		return 0
	}
	nf := min(len(frame), n)
	copy(s.re[:nf], frame[:nf])
	clear(s.re[nf:])
	clear(s.im)

	s.transform(s.re, s.im)

	for k := range nb {
		dst[k] = complex(s.re[k], s.im[k])
	}
	return nb
}

func (s *scalarFFT) inverse(dst []float32, spec []complex64) int {
	n := s.size
	ns := min(len(dst), n)
	if ns == 0 {
		return 0
	}
	half := n / 2

	if len(spec) > 0 {
		s.re[0] = real(spec[0])
	} else {
		s.re[0] = 0
	}
	s.im[0] = 0

	if half < len(spec) {
		s.re[half] = real(spec[half])
	} else {
		s.re[half] = 0
	}
	s.im[half] = 0

	for k := 1; k < half; k++ {
		if k < len(spec) {
			r := real(spec[k])
			im := imag(spec[k])
			s.re[k] = r
			s.im[k] = -im
			s.re[n-k] = r
			s.im[n-k] = im
		} else {
			s.re[k] = 0
			s.im[k] = 0
			s.re[n-k] = 0
			s.im[n-k] = 0
		}
	}

	s.transform(s.re, s.im)

	scale := 1 / float32(n)
	for j := range ns {
		dst[j] = s.re[j] * scale
	}
	return ns
}
