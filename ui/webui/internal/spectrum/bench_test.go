package spectrum

import (
	"math"
	"testing"
)

// These benchmarks measure the shipped transform in both forms, so the
// before/after of the SIMD optimization is the real code, not a copy. They use
// the visualizer's shipped shape (8192-point frames) and a steady tone.

const benchSize = 8192

func benchTone(n int) []float32 {
	s := make([]float32, n)
	for i := range s {
		t := float64(i) / 48000
		s[i] = float32(0.5*math.Sin(2*math.Pi*440*t) +
			0.25*math.Sin(2*math.Pi*3000*t) +
			0.1*math.Sin(2*math.Pi*9000*t))
	}
	return s
}

// BenchmarkFFTScalar is the pre-optimization transform: pure float64 radix-2.
func BenchmarkFFTScalar(b *testing.B) {
	a, err := NewAnalyzer(benchSize)
	if err != nil {
		b.Fatal(err)
	}
	in := benchTone(benchSize)
	out := make([]float64, a.Bins())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Magnitudes(in, out)
	}
}

// BenchmarkFFTSIMD is the optimized transform: the vectorized f32 plan.
func BenchmarkFFTSIMD(b *testing.B) {
	a, err := newSIMDAnalyzer(benchSize)
	if err != nil {
		b.Fatal(err)
	}
	in := benchTone(benchSize)
	out := make([]float64, a.Bins())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Magnitudes(in, out)
	}
}

// BenchmarkFFTSelected measures whatever NewTransform picks on this CPU, which
// is the number that reflects the shipped path.
func BenchmarkFFTSelected(b *testing.B) {
	a, err := NewTransform(benchSize)
	if err != nil {
		b.Fatal(err)
	}
	in := benchTone(benchSize)
	out := make([]float64, a.Bins())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Magnitudes(in, out)
	}
}

// benchRunner builds a Runner over a tone with the given transform, pre-filled
// and settled, so the frame benchmark measures steady-state work.
func benchRunner(b *testing.B, build func(int) (Transform, error)) *Runner {
	b.Helper()
	cfg := DefaultConfig()
	cfg.FFT = benchSize
	tap := &benchTap{}
	r, err := New(tap, cfg)
	if err != nil {
		b.Fatal(err)
	}
	tr, err := build(cfg.FFT)
	if err != nil {
		b.Fatal(err)
	}
	r.an = tr

	step := cfg.SampleRate / 60
	for i := 0; i < 300; i++ {
		tap.publish(step)
		r.Frame()
	}
	return r
}

// BenchmarkRunnerFrameScalar is the whole visualizer frame before the change.
func BenchmarkRunnerFrameScalar(b *testing.B) {
	r := benchRunner(b, func(size int) (Transform, error) { return NewAnalyzer(size) })
	step := r.cfg.SampleRate / 60
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Keep the tap fed so the window actually advances each frame.
		if t, ok := r.tap.(*benchTap); ok {
			t.publish(step)
		}
		r.Frame()
	}
}

// BenchmarkRunnerFrameSIMD is the whole visualizer frame after the change.
func BenchmarkRunnerFrameSIMD(b *testing.B) {
	r := benchRunner(b, func(size int) (Transform, error) { return newSIMDAnalyzer(size) })
	step := r.cfg.SampleRate / 60
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if t, ok := r.tap.(*benchTap); ok {
			t.publish(step)
		}
		r.Frame()
	}
}

// benchTap is a non-blocking looping tone source.
type benchTap struct {
	pos   int
	avail int
}

func (t *benchTap) publish(n int) { t.avail += n }

func (t *benchTap) Read(dst []float32) int {
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
