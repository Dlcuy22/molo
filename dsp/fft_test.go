package dsp

import (
	"math"
	"math/rand/v2"
	"testing"
)

func fftTestSignal(n int) []float32 {
	sig := make([]float32, n)
	for i := range sig {
		t := float64(i) / float64(n)
		sig[i] = float32(
			0.6*math.Sin(2*math.Pi*7*t) +
				0.3*math.Cos(2*math.Pi*23*t) -
				0.15*math.Sin(2*math.Pi*57*t+0.4),
		)
	}
	return sig
}

func fftNaiveDFT(frame []float32, nfft int) []complex128 {
	half := nfft / 2
	out := make([]complex128, half+1)
	for k := 0; k <= half; k++ {
		var re, im float64
		for n := 0; n < nfft; n++ {
			var x float64
			if n < len(frame) {
				x = float64(frame[n])
			}
			ang := -2 * math.Pi * float64(k*n) / float64(nfft)
			s, c := math.Sincos(ang)
			re += x * c
			im += x * s
		}
		out[k] = complex(re, im)
	}
	return out
}

func fftRelativeL2Error(got []complex64, want []complex128) float64 {
	var diffNormSq, wantNormSq float64
	for i := range got {
		dr := float64(real(got[i])) - real(want[i])
		di := float64(imag(got[i])) - imag(want[i])
		diffNormSq += dr*dr + di*di

		wr := real(want[i])
		wi := imag(want[i])
		wantNormSq += wr*wr + wi*wi
	}
	if wantNormSq == 0 {
		return math.Sqrt(diffNormSq)
	}
	return math.Sqrt(diffNormSq / wantNormSq)
}

func TestFFTForwardAgainstNaiveDFT(t *testing.T) {
	const nfft = 1024
	frame := fftTestSignal(nfft)
	want := fftNaiveDFT(frame, nfft)

	t.Run("default_plan", func(t *testing.T) {
		p, err := newFFT(nfft)
		if err != nil {
			t.Fatalf("newFFT(%d): %v", nfft, err)
		}
		got := make([]complex64, nfft/2+1)
		n := p.forward(got, frame)
		if n != len(got) {
			t.Fatalf("forward wrote %d bins, want %d", n, len(got))
		}

		relErr := fftRelativeL2Error(got, want)
		if relErr >= 1e-4 {
			t.Fatalf("relative error %g >= 1e-4", relErr)
		}
	})

	t.Run("scalar_plan", func(t *testing.T) {
		p, err := newScalarFFTPlan(nfft)
		if err != nil {
			t.Fatalf("newScalarFFTPlan(%d): %v", nfft, err)
		}
		got := make([]complex64, nfft/2+1)
		n := p.forward(got, frame)
		if n != len(got) {
			t.Fatalf("forward wrote %d bins, want %d", n, len(got))
		}

		relErr := fftRelativeL2Error(got, want)
		if relErr >= 1e-4 {
			t.Fatalf("relative error %g >= 1e-4", relErr)
		}
	})
}

func TestFFTRoundTripInverseForward(t *testing.T) {
	sizes := []int{2, 4, 8, 16, 64, 128, 512, 1024, 2048, 4096}

	for _, nfft := range sizes {
		frame := fftTestSignal(nfft)
		spec := make([]complex64, nfft/2+1)
		recon := make([]float32, nfft)

		runRoundTrip := func(t *testing.T, p *fftPlan, label string) {
			p.forward(spec, frame)
			p.inverse(recon, spec)

			const tol = 1e-4
			for i := range frame {
				diff := math.Abs(float64(recon[i] - frame[i]))
				if diff > tol {
					t.Fatalf("%s nfft=%d sample %d: got %g want %g (diff=%g > %g)", label, nfft, i, recon[i], frame[i], diff, tol)
				}
			}
		}

		t.Run("default", func(t *testing.T) {
			p, err := newFFT(nfft)
			if err != nil {
				t.Fatalf("newFFT(%d): %v", nfft, err)
			}
			runRoundTrip(t, p, "default")
		})

		t.Run("scalar", func(t *testing.T) {
			p, err := newScalarFFTPlan(nfft)
			if err != nil {
				t.Fatalf("newScalarFFTPlan(%d): %v", nfft, err)
			}
			runRoundTrip(t, p, "scalar")
		})
	}
}

func TestFFTComplexMultiply(t *testing.T) {
	const n = 513
	rng := rand.New(rand.NewPCG(42, 100))

	a := make([]complex64, n)
	b := make([]complex64, n)
	for i := range n {
		a[i] = complex(rng.Float32()*2-1, rng.Float32()*2-1)
		b[i] = complex(rng.Float32()*2-1, rng.Float32()*2-1)
	}

	wantAdd := make([]complex64, n)
	wantMul := make([]complex64, n)
	wantConj := make([]complex64, n)
	for i := range n {
		ar, ai := real(a[i]), imag(a[i])
		br, bi := real(b[i]), imag(b[i])
		wantAdd[i] = a[i] + b[i]
		wantMul[i] = a[i] * b[i]
		wantConj[i] = complex(ar*br+ai*bi, ai*br-ar*bi)
	}

	t.Run("addComplex", func(t *testing.T) {
		got := make([]complex64, n)
		addComplex(got, a, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantAdd[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantAdd[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("addComplex bin %d: got %v want %v", i, got[i], wantAdd[i])
			}
		}

		// In-place aliasing dst == a
		copy(got, a)
		addComplex(got, got, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantAdd[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantAdd[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("addComplex alias bin %d: got %v want %v", i, got[i], wantAdd[i])
			}
		}
	})

	t.Run("addComplexScalar", func(t *testing.T) {
		got := make([]complex64, n)
		addComplexScalar(got, a, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantAdd[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantAdd[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("addComplexScalar bin %d: got %v want %v", i, got[i], wantAdd[i])
			}
		}

		copy(got, a)
		addComplexScalar(got, got, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantAdd[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantAdd[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("addComplexScalar alias bin %d: got %v want %v", i, got[i], wantAdd[i])
			}
		}
	})

	t.Run("mulComplex", func(t *testing.T) {
		got := make([]complex64, n)
		mulComplex(got, a, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantMul[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantMul[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulComplex bin %d: got %v want %v", i, got[i], wantMul[i])
			}
		}
	})

	t.Run("mulComplexScalar", func(t *testing.T) {
		got := make([]complex64, n)
		mulComplexScalar(got, a, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantMul[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantMul[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulComplexScalar bin %d: got %v want %v", i, got[i], wantMul[i])
			}
		}
	})

	t.Run("mulComplex_inplace", func(t *testing.T) {
		got := make([]complex64, n)
		copy(got, a)
		mulComplex(got, got, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantMul[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantMul[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulComplex inplace bin %d: got %v want %v", i, got[i], wantMul[i])
			}
		}

		copy(got, a)
		mulComplexScalar(got, got, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantMul[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantMul[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulComplexScalar inplace bin %d: got %v want %v", i, got[i], wantMul[i])
			}
		}
	})

	t.Run("mulConjComplex", func(t *testing.T) {
		got := make([]complex64, n)
		mulConjComplex(got, a, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantConj[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantConj[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulConjComplex bin %d: got %v want %v", i, got[i], wantConj[i])
			}
		}
	})

	t.Run("mulConjComplexScalar", func(t *testing.T) {
		got := make([]complex64, n)
		mulConjComplexScalar(got, a, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantConj[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantConj[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulConjComplexScalar bin %d: got %v want %v", i, got[i], wantConj[i])
			}
		}
	})

	t.Run("mulConjComplex_inplace", func(t *testing.T) {
		got := make([]complex64, n)
		copy(got, a)
		mulConjComplex(got, got, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantConj[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantConj[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulConjComplex inplace bin %d: got %v want %v", i, got[i], wantConj[i])
			}
		}

		copy(got, a)
		mulConjComplexScalar(got, got, b)
		for i := range n {
			dr := math.Abs(float64(real(got[i]) - real(wantConj[i])))
			di := math.Abs(float64(imag(got[i]) - imag(wantConj[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulConjComplexScalar inplace bin %d: got %v want %v", i, got[i], wantConj[i])
			}
		}
	})

	t.Run("split_variants", func(t *testing.T) {
		aRe := make([]float32, n)
		aIm := make([]float32, n)
		bRe := make([]float32, n)
		bIm := make([]float32, n)
		dstRe := make([]float32, n)
		dstIm := make([]float32, n)
		for i := range n {
			aRe[i] = real(a[i])
			aIm[i] = imag(a[i])
			bRe[i] = real(b[i])
			bIm[i] = imag(b[i])
		}

		mulComplexSplit(dstRe, dstIm, aRe, aIm, bRe, bIm)
		for i := range n {
			dr := math.Abs(float64(dstRe[i] - real(wantMul[i])))
			di := math.Abs(float64(dstIm[i] - imag(wantMul[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulComplexSplit bin %d: got (%g,%g) want %v", i, dstRe[i], dstIm[i], wantMul[i])
			}
		}

		mulComplexSplitScalar(dstRe, dstIm, aRe, aIm, bRe, bIm)
		for i := range n {
			dr := math.Abs(float64(dstRe[i] - real(wantMul[i])))
			di := math.Abs(float64(dstIm[i] - imag(wantMul[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulComplexSplitScalar bin %d: got (%g,%g) want %v", i, dstRe[i], dstIm[i], wantMul[i])
			}
		}

		// In-place aliasing for split multiply: dstRe==aRe && dstIm==aIm
		copy(dstRe, aRe)
		copy(dstIm, aIm)
		mulComplexSplit(dstRe, dstIm, dstRe, dstIm, bRe, bIm)
		for i := range n {
			dr := math.Abs(float64(dstRe[i] - real(wantMul[i])))
			di := math.Abs(float64(dstIm[i] - imag(wantMul[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulComplexSplit aliased bin %d: got (%g,%g) want %v", i, dstRe[i], dstIm[i], wantMul[i])
			}
		}

		copy(dstRe, aRe)
		copy(dstIm, aIm)
		mulComplexSplitScalar(dstRe, dstIm, dstRe, dstIm, bRe, bIm)
		for i := range n {
			dr := math.Abs(float64(dstRe[i] - real(wantMul[i])))
			di := math.Abs(float64(dstIm[i] - imag(wantMul[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulComplexSplitScalar aliased bin %d: got (%g,%g) want %v", i, dstRe[i], dstIm[i], wantMul[i])
			}
		}

		mulConjComplexSplit(dstRe, dstIm, aRe, aIm, bRe, bIm)
		for i := range n {
			dr := math.Abs(float64(dstRe[i] - real(wantConj[i])))
			di := math.Abs(float64(dstIm[i] - imag(wantConj[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulConjComplexSplit bin %d: got (%g,%g) want %v", i, dstRe[i], dstIm[i], wantConj[i])
			}
		}

		mulConjComplexSplitScalar(dstRe, dstIm, aRe, aIm, bRe, bIm)
		for i := range n {
			dr := math.Abs(float64(dstRe[i] - real(wantConj[i])))
			di := math.Abs(float64(dstIm[i] - imag(wantConj[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulConjComplexSplitScalar bin %d: got (%g,%g) want %v", i, dstRe[i], dstIm[i], wantConj[i])
			}
		}

		// In-place aliasing for split conj multiply: dstRe==aRe && dstIm==aIm
		copy(dstRe, aRe)
		copy(dstIm, aIm)
		mulConjComplexSplit(dstRe, dstIm, dstRe, dstIm, bRe, bIm)
		for i := range n {
			dr := math.Abs(float64(dstRe[i] - real(wantConj[i])))
			di := math.Abs(float64(dstIm[i] - imag(wantConj[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulConjComplexSplit aliased bin %d: got (%g,%g) want %v", i, dstRe[i], dstIm[i], wantConj[i])
			}
		}

		copy(dstRe, aRe)
		copy(dstIm, aIm)
		mulConjComplexSplitScalar(dstRe, dstIm, dstRe, dstIm, bRe, bIm)
		for i := range n {
			dr := math.Abs(float64(dstRe[i] - real(wantConj[i])))
			di := math.Abs(float64(dstIm[i] - imag(wantConj[i])))
			if dr > 1e-6 || di > 1e-6 {
				t.Fatalf("mulConjComplexSplitScalar aliased bin %d: got (%g,%g) want %v", i, dstRe[i], dstIm[i], wantConj[i])
			}
		}
	})
}

func TestFFTZeroAllocs(t *testing.T) {
	for _, nfft := range []int{64, 256, 1024, 2048} {
		frame := fftTestSignal(nfft)
		spec := make([]complex64, nfft/2+1)
		out := make([]float32, nfft)
		nb := nfft/2 + 1

		aRe := make([]float32, nb)
		aIm := make([]float32, nb)
		bRe := make([]float32, nb)
		bIm := make([]float32, nb)
		dstRe := make([]float32, nb)
		dstIm := make([]float32, nb)

		pDefault, err := newFFT(nfft)
		if err != nil {
			t.Fatalf("newFFT(%d): %v", nfft, err)
		}
		pDefault.forward(spec, frame)
		pDefault.inverse(out, spec)

		allocsDefault := testing.AllocsPerRun(50, func() {
			pDefault.forward(spec, frame)
			pDefault.inverse(out, spec)
		})
		if allocsDefault != 0 {
			t.Errorf("default plan nfft=%d: allocated %g per run, want 0", nfft, allocsDefault)
		}

		pScalar, err := newScalarFFTPlan(nfft)
		if err != nil {
			t.Fatalf("newScalarFFTPlan(%d): %v", nfft, err)
		}
		pScalar.forward(spec, frame)
		pScalar.inverse(out, spec)

		allocsScalar := testing.AllocsPerRun(50, func() {
			pScalar.forward(spec, frame)
			pScalar.inverse(out, spec)
		})
		if allocsScalar != 0 {
			t.Errorf("scalar plan nfft=%d: allocated %g per run, want 0", nfft, allocsScalar)
		}

		allocsComplexOps := testing.AllocsPerRun(50, func() {
			addComplex(spec, spec, spec)
			addComplexScalar(spec, spec, spec)
			mulComplex(spec, spec, spec)
			mulComplexScalar(spec, spec, spec)
			mulConjComplex(spec, spec, spec)
			mulConjComplexScalar(spec, spec, spec)
			mulComplexSplit(dstRe, dstIm, aRe, aIm, bRe, bIm)
			mulComplexSplitScalar(dstRe, dstIm, aRe, aIm, bRe, bIm)
			mulConjComplexSplit(dstRe, dstIm, aRe, aIm, bRe, bIm)
			mulConjComplexSplitScalar(dstRe, dstIm, aRe, aIm, bRe, bIm)
		})
		if allocsComplexOps != 0 {
			t.Errorf("complex arithmetic operations allocated %g per run, want 0", allocsComplexOps)
		}
	}
}

func TestFFTSizeValidation(t *testing.T) {
	badSizes := []int{-8, -1, 0, 1, 3, 5, 7, 1000, 1023, 1025}
	for _, size := range badSizes {
		if _, err := newFFT(size); err == nil {
			t.Errorf("newFFT(%d) expected error, got nil", size)
		}
		if _, err := newScalarFFTPlan(size); err == nil {
			t.Errorf("newScalarFFTPlan(%d) expected error, got nil", size)
		}
	}

	p, err := newFFT(512)
	if err != nil {
		t.Fatalf("newFFT(512): %v", err)
	}
	if p.size() != 512 {
		t.Errorf("p.size() = %d, want 512", p.size())
	}
	if p.bins() != 257 {
		t.Errorf("p.bins() = %d, want 257", p.bins())
	}

	ps, err := newScalarFFTPlan(512)
	if err != nil {
		t.Fatalf("newScalarFFTPlan(512): %v", err)
	}
	if ps.size() != 512 {
		t.Errorf("ps.size() = %d, want 512", ps.size())
	}
	if ps.bins() != 257 {
		t.Errorf("ps.bins() = %d, want 257", ps.bins())
	}
}

func TestFFTEdgeCases(t *testing.T) {
	const nfft = 128
	plans := []*fftPlan{}
	if p, err := newFFT(nfft); err == nil {
		plans = append(plans, p)
	}
	if p, err := newScalarFFTPlan(nfft); err == nil {
		plans = append(plans, p)
	}

	for _, p := range plans {
		// Short frame input (zero-padded).
		shortFrame := []float32{1.0, 2.0, 3.0}
		spec := make([]complex64, p.bins())
		n := p.forward(spec, shortFrame)
		if n != len(spec) {
			t.Errorf("forward shortFrame: wrote %d, want %d", n, len(spec))
		}

		// Nil / empty destination.
		if n := p.forward(nil, shortFrame); n != 0 {
			t.Errorf("forward to nil dst: wrote %d, want 0", n)
		}
		if n := p.inverse(nil, spec); n != 0 {
			t.Errorf("inverse to nil dst: wrote %d, want 0", n)
		}

		// inverse with nil spec: assert no panic and finite zero output.
		outNil := make([]float32, nfft)
		if n := p.inverse(outNil, nil); n != nfft {
			t.Errorf("inverse with nil spec wrote %d, want %d", n, nfft)
		}
		for i, v := range outNil {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || v != 0 {
				t.Fatalf("inverse sample %d with nil spec: got %g, want 0", i, v)
			}
		}

		// inverse with shortSpec (len < bins): assert no panic and finite output.
		shortSpec := []complex64{complex(1.0, 0), complex(0.5, -0.2)}
		outShort := make([]float32, nfft)
		if n := p.inverse(outShort, shortSpec); n != nfft {
			t.Errorf("inverse with shortSpec wrote %d, want %d", n, nfft)
		}
		for i, v := range outShort {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("inverse sample %d with shortSpec is non-finite: %g", i, v)
			}
		}

		// Partial destination slice.
		partialSpec := make([]complex64, 10)
		if n := p.forward(partialSpec, shortFrame); n != 10 {
			t.Errorf("forward partial dst: wrote %d, want 10", n)
		}

		partialOut := make([]float32, 10)
		if n := p.inverse(partialOut, spec); n != 10 {
			t.Errorf("inverse partial dst: wrote %d, want 10", n)
		}

		// DC and Nyquist imaginary components must be dropped.
		noisySpec := make([]complex64, p.bins())
		copy(noisySpec, spec)
		noisySpec[0] = complex(real(noisySpec[0]), 999.0)
		noisySpec[nfft/2] = complex(real(noisySpec[nfft/2]), 999.0)

		outClean := make([]float32, nfft)
		outNoisy := make([]float32, nfft)
		p.inverse(outClean, spec)
		p.inverse(outNoisy, noisySpec)

		for i := range nfft {
			if math.Abs(float64(outClean[i]-outNoisy[i])) > 1e-6 {
				t.Fatalf("inverse sample %d: imaginary DC/Nyquist not ignored (clean=%g, noisy=%g)", i, outClean[i], outNoisy[i])
			}
		}
	}
}
