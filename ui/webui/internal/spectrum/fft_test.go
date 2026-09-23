package spectrum

import (
	"math"
	"testing"
)

// TestNewRejectsNonPowerOfTwo pins the one precondition radix-2 cannot meet.
func TestNewRejectsNonPowerOfTwo(t *testing.T) {
	for _, size := range []int{0, 1, 3, 6, 100, 1000} {
		if _, err := NewAnalyzer(size); err == nil {
			t.Errorf("NewAnalyzer(%d) = nil error, want ErrFFTSize", size)
		}
	}
	for _, size := range []int{2, 4, 8, 1024, 2048} {
		if _, err := NewAnalyzer(size); err != nil {
			t.Errorf("NewAnalyzer(%d) = %v, want nil", size, err)
		}
	}
}

// TestSinePeaksInItsBin is the load-bearing correctness check: a pure tone must
// land in the bin nearest its frequency, with the rest of the spectrum far
// below it. A wrong twiddle sign or a missing bit-reversal sends the energy
// somewhere else, so this fails loudly on the usual FFT bugs.
func TestSinePeaksInItsBin(t *testing.T) {
	const (
		size = 2048
		rate = 48000
		freq = 6000
	)
	a, err := NewAnalyzer(size)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	samples := make([]float32, size)
	for i := range samples {
		samples[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/rate))
	}

	out := make([]float64, a.Bins())
	bins := a.Magnitudes(samples, out)

	want := freq * size / rate
	peak := 0
	for k := 1; k < bins; k++ {
		if out[k] > out[peak] {
			peak = k
		}
	}
	if peak != want {
		t.Fatalf("peak bin = %d, want %d (bin %d is %.6f, peak is %.6f)", peak, want, want, out[want], out[peak])
	}
	// The neighbours of a Hann-windowed tone are roughly half the peak; a leaky
	// transform would spread energy far from the bin instead.
	if out[want] < 0.4 {
		t.Errorf("peak magnitude = %.4f, want about 0.5", out[want])
	}
	if out[want+8] > out[want]/20 {
		t.Errorf("far bin %d = %.6f, want leakage well below the peak %.6f", want+8, out[want+8], out[want])
	}
}

// TestDCOffsetLandsInBinZero checks the other end of the spectrum. A constant
// signal windowed by Hann has its energy in bins 0 and 1 only (the window's own
// spectrum is N/2, N/4, 0), so bin 2 and beyond must be essentially empty.
func TestDCOffsetLandsInBinZero(t *testing.T) {
	a, err := NewAnalyzer(1024)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	samples := make([]float32, a.Size())
	for i := range samples {
		samples[i] = 0.25
	}

	out := make([]float64, a.Bins())
	a.Magnitudes(samples, out)

	if out[0] <= out[1] {
		t.Errorf("bin0 = %.6f, bin1 = %.6f, want DC above its Hann sidebin", out[0], out[1])
	}
	if out[2] > out[0]/1000 {
		t.Errorf("bin2 = %.6f, want empty next to DC %.6f", out[2], out[0])
	}
}

// TestShortFrameDoesNotPanic covers the early calls before a whole frame is
// buffered: a partial input is zero-padded, not an error.
func TestShortFrameDoesNotPanic(t *testing.T) {
	a, err := NewAnalyzer(64)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := make([]float64, a.Bins())
	if got := a.Magnitudes([]float32{0.1, -0.1, 0.2}, out); got != a.Bins() {
		t.Errorf("bins = %d, want %d", got, a.Bins())
	}
	if got := a.Magnitudes(nil, out); got != a.Bins() {
		t.Errorf("bins from nil = %d, want %d", got, a.Bins())
	}
}

// TestShortOutLeaveRoomIsRespected checks the partial-output path, which the
// visualizer uses when it shows fewer bins than the frame produces.
func TestShortOutLeaveRoomIsRespected(t *testing.T) {
	a, err := NewAnalyzer(256)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := make([]float64, 10)
	if got := a.Magnitudes(make([]float32, a.Size()), out); got != 10 {
		t.Errorf("bins = %d, want 10", got)
	}
}

// TestAnalyzerReuseIsStable guards against scratch state leaking between
// frames: the same input analyzed twice must give the same answer.
func TestAnalyzerReuseIsStable(t *testing.T) {
	a, err := NewAnalyzer(512)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	samples := make([]float32, a.Size())
	for i := range samples {
		samples[i] = float32(math.Sin(float64(i) * 0.3))
	}

	first := make([]float64, a.Bins())
	second := make([]float64, a.Bins())
	a.Magnitudes(samples, first)
	a.Magnitudes(samples, second)

	for k := range first {
		if math.Abs(first[k]-second[k]) > 1e-12 {
			t.Fatalf("bin %d differs across runs: %.12f vs %.12f", k, first[k], second[k])
		}
	}
}

// TestMagnitudesAreNonNegative is the property the visualizer relies on when it
// maps a bin to a bar height.
func TestMagnitudesAreNonNegative(t *testing.T) {
	a, err := NewAnalyzer(1024)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	samples := make([]float32, a.Size())
	for i := range samples {
		samples[i] = float32(math.Sin(float64(i)*0.07) + 0.5*math.Cos(float64(i)*0.31))
	}
	out := make([]float64, a.Bins())
	a.Magnitudes(samples, out)
	for k, v := range out {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("bin %d = %v, want a finite non-negative magnitude", k, v)
		}
	}
}
