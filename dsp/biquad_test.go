package dsp

import (
	"math"
	"testing"
)

func biquadTestApproxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestBiquadPeakingZeroGainIdentity(t *testing.T) {
	freqs := []float64{100, 1000, 10000}
	qs := []float64{0.5, 0.707, 2.0}

	for _, freq := range freqs {
		for _, q := range qs {
			coeffs, err := designBiquad("peaking", 48000, freq, q, 0)
			if err != nil {
				t.Fatalf("designBiquad failed: %v", err)
			}

			b0, b1, b2, a1, a2 := coeffs[0], coeffs[1], coeffs[2], coeffs[3], coeffs[4]
			if !biquadTestApproxEqual(b0, 1.0, 1e-12) {
				t.Fatalf("expected b0 == 1.0, got %v", b0)
			}
			if !biquadTestApproxEqual(b1, a1, 1e-12) {
				t.Fatalf("expected b1 == a1, got b1=%v a1=%v", b1, a1)
			}
			if !biquadTestApproxEqual(b2, a2, 1e-12) {
				t.Fatalf("expected b2 == a2, got b2=%v a2=%v", b2, a2)
			}

			st := newBiquadState(1)
			st.setCoeffs(b0, b1, b2, a1, a2)

			// Step through varied inputs to verify unity transfer.
			for n := 0; n < 100; n++ {
				x := math.Sin(float64(n) * 0.1)
				y := st.step(x, 0)
				if !biquadTestApproxEqual(y, x, 1e-12) {
					t.Fatalf("step %d: expected y == x, got x=%v y=%v", n, x, y)
				}
			}
		}
	}
}

func TestBiquadLowpassDCPassAndNyquistAttenuate(t *testing.T) {
	coeffs, err := designBiquad("lowpass", 48000, 1000, 0.707, 0)
	if err != nil {
		t.Fatalf("designBiquad failed: %v", err)
	}

	stDC := newBiquadState(1)
	stDC.setCoeffs(coeffs[0], coeffs[1], coeffs[2], coeffs[3], coeffs[4])

	var yDC float64
	for n := 0; n < 500; n++ {
		yDC = stDC.step(1.0, 0)
	}
	if !biquadTestApproxEqual(yDC, 1.0, 1e-5) {
		t.Fatalf("expected DC output near 1.0, got %v", yDC)
	}

	stNyq := newBiquadState(1)
	stNyq.setCoeffs(coeffs[0], coeffs[1], coeffs[2], coeffs[3], coeffs[4])

	var yNyq float64
	for n := 0; n < 500; n++ {
		x := 1.0
		if n%2 != 0 {
			x = -1.0
		}
		yNyq = stNyq.step(x, 0)
	}
	if math.Abs(yNyq) > 1e-3 {
		t.Fatalf("expected Nyquist output heavily attenuated, got %v", yNyq)
	}
}

func TestBiquadReferenceCoefficients(t *testing.T) {
	// Analytical reference at quarter sample rate where trigonometry simplifies.
	lowpassCoeffs, err := designBiquad("lowpass", 48000, 12000, 0.5, 0)
	if err != nil {
		t.Fatalf("designBiquad failed: %v", err)
	}
	expectedLP := [5]float64{0.25, 0.5, 0.25, 0.0, 0.0}
	for i := range expectedLP {
		if !biquadTestApproxEqual(lowpassCoeffs[i], expectedLP[i], 1e-12) {
			t.Fatalf("coeff %d: got %v want %v", i, lowpassCoeffs[i], expectedLP[i])
		}
	}

	// Hand-computed reference for peaking filter at 1 kHz, Q 2.0, +6 dB.
	peakCoeffs, err := designBiquad("peaking", 48000, 1000, 2.0, 6.0)
	if err != nil {
		t.Fatalf("designBiquad failed: %v", err)
	}
	expectedPeak := [5]float64{
		1.0224727682198582,
		-1.9381165805572229,
		0.9323677439107332,
		-1.9381165805572229,
		0.9548405121305915,
	}
	for i := range expectedPeak {
		if !biquadTestApproxEqual(peakCoeffs[i], expectedPeak[i], 1e-12) {
			t.Fatalf("peaking coeff %d: got %v want %v", i, peakCoeffs[i], expectedPeak[i])
		}
	}
}

func TestBiquadSetCoeffsPreservesMemory(t *testing.T) {
	coeffs1, err := designBiquad("lowpass", 48000, 1000, 0.707, 0)
	if err != nil {
		t.Fatalf("designBiquad failed: %v", err)
	}
	coeffs2, err := designBiquad("highpass", 48000, 2000, 0.707, 0)
	if err != nil {
		t.Fatalf("designBiquad failed: %v", err)
	}

	st := newBiquadState(2)
	st.setCoeffs(coeffs1[0], coeffs1[1], coeffs1[2], coeffs1[3], coeffs1[4])

	for n := 0; n < 10; n++ {
		st.step(0.5, 0)
		st.step(-0.5, 1)
	}

	x1Ch0, x2Ch0 := st.x1[0], st.x2[0]
	y1Ch0, y2Ch0 := st.y1[0], st.y2[0]
	x1Ch1, x2Ch1 := st.x1[1], st.x2[1]
	y1Ch1, y2Ch1 := st.y1[1], st.y2[1]

	if x1Ch0 == 0 || y1Ch0 == 0 || x1Ch1 == 0 || y1Ch1 == 0 {
		t.Fatal("expected non-zero filter memory before updating coefficients")
	}

	st.setCoeffs(coeffs2[0], coeffs2[1], coeffs2[2], coeffs2[3], coeffs2[4])

	if st.x1[0] != x1Ch0 || st.x2[0] != x2Ch0 || st.y1[0] != y1Ch0 || st.y2[0] != y2Ch0 {
		t.Fatal("setCoeffs modified channel 0 memory")
	}
	if st.x1[1] != x1Ch1 || st.x2[1] != x2Ch1 || st.y1[1] != y1Ch1 || st.y2[1] != y2Ch1 {
		t.Fatal("setCoeffs modified channel 1 memory")
	}
}

func TestBiquadNaNRecovery(t *testing.T) {
	coeffs, err := designBiquad("peaking", 48000, 1000, 1.0, 6.0)
	if err != nil {
		t.Fatalf("designBiquad failed: %v", err)
	}

	st := newBiquadState(2)
	st.setCoeffs(coeffs[0], coeffs[1], coeffs[2], coeffs[3], coeffs[4])

	for n := 0; n < 10; n++ {
		st.step(0.5, 0)
		st.step(0.7, 1)
	}

	// Non-finite sample resets only the targeted channel.
	outNaN := st.step(math.NaN(), 0)
	if outNaN != 0 {
		t.Fatalf("expected 0 output on NaN input, got %v", outNaN)
	}
	if st.x1[0] != 0 || st.x2[0] != 0 || st.y1[0] != 0 || st.y2[0] != 0 {
		t.Fatal("channel 0 memory was not cleared on NaN input")
	}
	if st.x1[1] == 0 || st.y1[1] == 0 {
		t.Fatal("channel 1 memory was erroneously cleared")
	}

	// Normal inputs recover cleanly after NaN reset.
	outNormal := st.step(0.5, 0)
	if math.IsNaN(outNormal) || math.IsInf(outNormal, 0) {
		t.Fatalf("expected finite output after recovery, got %v", outNormal)
	}

	// Inf input resets channel 1.
	outInf := st.step(math.Inf(1), 1)
	if outInf != 0 {
		t.Fatalf("expected 0 output on Inf input, got %v", outInf)
	}
	if st.x1[1] != 0 || st.x2[1] != 0 || st.y1[1] != 0 || st.y2[1] != 0 {
		t.Fatal("channel 1 memory was not cleared on Inf input")
	}
}

func TestBiquadZeroAllocs(t *testing.T) {
	coeffs, err := designBiquad("peaking", 48000, 1000, 1.0, 3.0)
	if err != nil {
		t.Fatalf("designBiquad failed: %v", err)
	}

	st := newBiquadState(2)
	st.setCoeffs(coeffs[0], coeffs[1], coeffs[2], coeffs[3], coeffs[4])

	allocsStep := testing.AllocsPerRun(1000, func() {
		_ = st.step(0.5, 0)
		_ = st.step(-0.5, 1)
	})
	if allocsStep != 0 {
		t.Fatalf("expected 0 allocs per run for step, got %v", allocsStep)
	}

	allocsSet := testing.AllocsPerRun(1000, func() {
		st.setCoeffs(coeffs[0], coeffs[1], coeffs[2], coeffs[3], coeffs[4])
	})
	if allocsSet != 0 {
		t.Fatalf("expected 0 allocs per run for setCoeffs, got %v", allocsSet)
	}
}

func TestBiquadValidationAndDefaults(t *testing.T) {
	kinds := []string{
		"lowpass", "highpass", "bandpass", "peaking",
		"lowshelf", "highshelf", "notch", "allpass",
	}
	for _, kind := range kinds {
		if _, err := designBiquad(kind, 48000, 1000, 1.0, 0); err != nil {
			t.Fatalf("failed to design supported kind %q: %v", kind, err)
		}
	}

	if _, err := designBiquad("invalid_kind", 48000, 1000, 1.0, 0); err == nil {
		t.Fatal("expected error on unknown filter kind")
	}

	invalidFreqs := []float64{-100, 0, 24000, 30000, math.NaN(), math.Inf(1)}
	for _, freq := range invalidFreqs {
		if _, err := designBiquad("lowpass", 48000, freq, 1.0, 0); err == nil {
			t.Fatalf("expected error for out-of-range freq %v", freq)
		}
	}

	// Rate <= 0 defaults to 48000.
	cDefaultRate, err := designBiquad("lowpass", 0, 1000, 0.707, 0)
	if err != nil {
		t.Fatalf("expected default rate 48000 to succeed: %v", err)
	}
	cExplicitRate, _ := designBiquad("lowpass", 48000, 1000, 0.707, 0)
	for i := range cDefaultRate {
		if cDefaultRate[i] != cExplicitRate[i] {
			t.Fatalf("coeff %d mismatch between default rate and 48000", i)
		}
	}

	// Q <= 0 defaults to 0.707.
	cDefaultQ, err := designBiquad("lowpass", 48000, 1000, -1, 0)
	if err != nil {
		t.Fatalf("expected default Q to succeed: %v", err)
	}
	cExplicitQ, _ := designBiquad("lowpass", 48000, 1000, 0.707, 0)
	for i := range cDefaultQ {
		if cDefaultQ[i] != cExplicitQ[i] {
			t.Fatalf("coeff %d mismatch between default Q and 0.707", i)
		}
	}
}

func TestBiquadReset(t *testing.T) {
	coeffs, err := designBiquad("lowpass", 48000, 1000, 0.707, 0)
	if err != nil {
		t.Fatalf("designBiquad failed: %v", err)
	}

	st := newBiquadState(2)
	st.setCoeffs(coeffs[0], coeffs[1], coeffs[2], coeffs[3], coeffs[4])

	for n := 0; n < 10; n++ {
		st.step(0.5, 0)
		st.step(0.8, 1)
	}

	st.reset()

	for ch := 0; ch < 2; ch++ {
		if st.x1[ch] != 0 || st.x2[ch] != 0 || st.y1[ch] != 0 || st.y2[ch] != 0 {
			t.Fatalf("channel %d memory not zeroed after reset", ch)
		}
	}

	if st.b0 != coeffs[0] || st.b1 != coeffs[1] || st.b2 != coeffs[2] || st.a1 != coeffs[3] || st.a2 != coeffs[4] {
		t.Fatal("reset modified filter coefficients")
	}
}

func TestBiquadGainDBValidation(t *testing.T) {
	nonFiniteGains := []float64{math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, gain := range nonFiniteGains {
		if _, err := designBiquad("peaking", 48000, 1000, 1.0, gain); err == nil {
			t.Fatalf("expected error for non-finite gainDB %v", gain)
		}
	}

	kinds := []string{
		"lowpass", "highpass", "bandpass", "peaking",
		"lowshelf", "highshelf", "notch", "allpass",
	}
	for _, kind := range kinds {
		coeffs, err := designBiquad(kind, 48000, 1000, 1.0, 3.0)
		if err != nil {
			t.Fatalf("kind %q failed for normal parameters: %v", kind, err)
		}
		for i, c := range coeffs {
			if math.IsNaN(c) || math.IsInf(c, 0) {
				t.Fatalf("kind %q coeff %d is non-finite: %v", kind, i, c)
			}
		}
	}
}
