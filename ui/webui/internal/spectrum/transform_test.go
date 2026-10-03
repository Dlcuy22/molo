package spectrum

import (
	"math"
	"testing"
)

// TestTransformSelectsSIMDWhenAvailable pins that the fast path is actually
// chosen on a CPU that has it. On a machine with neither AVX nor NEON the
// scalar analyzer is the right answer, so the test accepts that too.
func TestTransformSelectsSIMDWhenAvailable(t *testing.T) {
	tr, err := NewTransform(8192)
	if err != nil {
		t.Fatalf("NewTransform: %v", err)
	}
	_, isSIMD := tr.(*simdAnalyzer)
	if useSIMD() && !isSIMD {
		t.Fatalf("NewTransform returned %T on a SIMD-capable CPU, want *simdAnalyzer", tr)
	}
	if !useSIMD() && isSIMD {
		t.Fatalf("NewTransform returned *simdAnalyzer on a CPU without SIMD")
	}
}

// TestSIMDMatchesScalarBins checks the vectorized transform against the scalar
// reference on a multi-tone frame. The two use different arithmetic (float32
// radix-4 vs float64 radix-2), so they are not bit-identical, but the bins must
// agree far below anything the display can show.
func TestSIMDMatchesScalarBins(t *testing.T) {
	const size = 8192

	ref, err := NewAnalyzer(size)
	if err != nil {
		t.Fatal(err)
	}
	sim, err := newSIMDAnalyzer(size)
	if err != nil {
		t.Fatal(err)
	}

	in := make([]float32, size)
	for i := range in {
		tv := float64(i) / 48000
		in[i] = float32(0.5*math.Sin(2*math.Pi*440*tv) +
			0.25*math.Sin(2*math.Pi*3000*tv) +
			0.1*math.Sin(2*math.Pi*9000*tv))
	}

	want := make([]float64, ref.Bins())
	got := make([]float64, sim.Bins())
	ref.Magnitudes(in, want)
	sim.Magnitudes(in, got)

	peak := 0.0
	for _, v := range want {
		peak = math.Max(peak, v)
	}
	var worst float64
	for i := range want {
		worst = math.Max(worst, math.Abs(got[i]-want[i]))
	}
	if worst/peak > 1e-4 {
		t.Fatalf("SIMD vs scalar max rel err %.3e, want < 1e-4", worst/peak)
	}
}

// TestRunnerSIMDMatchesScalarBars runs the whole pipeline over both transforms
// and checks the display bars agree. This is the precision that actually
// matters: the bar a user sees.
func TestRunnerSIMDMatchesScalarBars(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFT = 8192

	scalarBars := runBarsWith(t, cfg, func(size int) (Transform, error) { return NewAnalyzer(size) })
	simdBars := runBarsWith(t, cfg, func(size int) (Transform, error) { return newSIMDAnalyzer(size) })

	var worst float64
	for i := range scalarBars {
		worst = math.Max(worst, math.Abs(scalarBars[i]-simdBars[i]))
	}
	// A bar is a [0,1] level drawn a few hundred pixels tall; one pixel is
	// ~1/300. The tolerance is well under that.
	if worst > 1e-3 {
		t.Fatalf("SIMD vs scalar bars max err %.3e, want < 1e-3", worst)
	}
}

// runBarsWith drives a Runner over a steady tone with an injectable transform.
func runBarsWith(t *testing.T, cfg Config, build func(int) (Transform, error)) []float64 {
	t.Helper()

	tap := &toneTap{}
	r, err := New(tap, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.an = mustTransform(t, build, cfg.FFT)

	// Fill the window and settle the peak, then read the steady bars.
	step := cfg.SampleRate / 60
	for i := 0; i < 300; i++ {
		tap.publish(step)
		r.Frame()
	}
	out := make([]float64, len(r.Bands()))
	copy(out, r.Bands())

	return out
}

func mustTransform(t *testing.T, build func(int) (Transform, error), size int) Transform {
	t.Helper()
	tr, err := build(size)
	if err != nil {
		t.Fatalf("build transform: %v", err)
	}

	return tr
}

// toneTap is a looping multi-tone source for the parity test.
type toneTap struct {
	pos   int
	avail int
}

func (t *toneTap) publish(n int) { t.avail += n }

func (t *toneTap) Read(dst []float32) int {
	if t.avail <= 0 {
		return 0
	}
	n := min(len(dst), t.avail)
	for i := 0; i < n; i++ {
		tv := float64(t.pos+i) / 48000
		dst[i] = float32(0.5*math.Sin(2*math.Pi*440*tv) +
			0.25*math.Sin(2*math.Pi*3000*tv) +
			0.1*math.Sin(2*math.Pi*9000*tv))
	}
	t.pos += n
	t.avail -= n

	return n
}
