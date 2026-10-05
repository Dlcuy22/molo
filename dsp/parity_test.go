package dsp

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/dlcuy22/molo/core"
)

// parityNullDB returns 20*log10(rms(a-b)/rms(a)), -Inf when identical or empty, and +Inf when silent reference sees signal.
func parityNullDB(a, b []float32) float64 {
	if len(a) == 0 {
		return math.Inf(-1)
	}
	n := max(len(a), len(b))
	var sumDiff, sumA float64
	for i := 0; i < n; i++ {
		var va, vb float64
		if i < len(a) {
			va = float64(a[i])
			sumA += va * va
		}
		if i < len(b) {
			vb = float64(b[i])
		}
		d := va - vb
		sumDiff += d * d
	}
	if sumDiff == 0 {
		return math.Inf(-1)
	}
	if sumA == 0 {
		return math.Inf(1)
	}
	rmsDiff := math.Sqrt(sumDiff / float64(n))
	rmsA := math.Sqrt(sumA / float64(n))
	return 20 * math.Log10(rmsDiff/rmsA)
}

// sineStereo generates an interleaved two-channel sine wave with amplitude 0.5 and matched phase.
func sineStereo(frames, rate int, hz float64) []float32 {
	if frames <= 0 || rate <= 0 {
		return nil
	}
	out := make([]float32, frames*2)
	omega := 2 * math.Pi * hz / float64(rate)
	for i := 0; i < frames; i++ {
		v := float32(0.5 * math.Sin(omega*float64(i)))
		out[2*i] = v
		out[2*i+1] = v
	}
	return out
}

// renderEffectChunks processes samples in place one chunk at a time with a single effect.
func renderEffectChunks(t *testing.T, e Effect, samples []float32, ch, chunkFrames int) {
	t.Helper()

	if ch <= 0 || chunkFrames <= 0 {
		t.Fatalf("invalid chunk parameters: ch=%d chunkFrames=%d", ch, chunkFrames)
	}
	for start := 0; start < len(samples); start += chunkFrames * ch {
		end := min(start+chunkFrames*ch, len(samples))
		frames := (end - start) / ch
		if err := e.Process(samples[start:end], frames); err != nil {
			t.Fatalf("Process at sample %d: %v", start, err)
		}
	}
}

type parityPassEffect struct {
	calls int
}

func (p *parityPassEffect) Name() string                                            { return "parity-pass" }
func (p *parityPassEffect) Schema() []Param                                         { return nil }
func (p *parityPassEffect) Get(string) (any, error)                                 { return nil, ErrUnknownParam }
func (p *parityPassEffect) Set(string, any) error                                   { return nil }
func (p *parityPassEffect) Configure(in core.FrameFormat) (core.FrameFormat, error) { return in, nil }
func (p *parityPassEffect) Process(_ []float32, _ int) error {
	p.calls++
	return nil
}
func (p *parityPassEffect) Reset() error { return nil }

type parityErrorEffect struct {
	err error
}

func (p *parityErrorEffect) Name() string                                            { return "parity-error" }
func (p *parityErrorEffect) Schema() []Param                                         { return nil }
func (p *parityErrorEffect) Get(string) (any, error)                                 { return nil, ErrUnknownParam }
func (p *parityErrorEffect) Set(string, any) error                                   { return nil }
func (p *parityErrorEffect) Configure(in core.FrameFormat) (core.FrameFormat, error) { return in, nil }
func (p *parityErrorEffect) Process(_ []float32, _ int) error {
	if p.err != nil {
		return p.err
	}
	return errors.New("parity process failure")
}
func (p *parityErrorEffect) Reset() error { return nil }

func TestParityNullDB(t *testing.T) {
	x := sineStereo(480, 48000, 1000)
	if got := parityNullDB(x, x); !math.IsInf(got, -1) {
		t.Fatalf("identical self: want -Inf, got %f", got)
	}

	xCopy := interleaveDeep(x)
	if got := parityNullDB(x, xCopy); !math.IsInf(got, -1) {
		t.Fatalf("identical copy: want -Inf, got %f", got)
	}

	scales := []float64{0.9, 0.5, 0.999, 1.1, 2.0}
	for _, s := range scales {
		scaled := make([]float32, len(x))
		for i, v := range x {
			scaled[i] = float32(float64(v) * s)
		}
		got := parityNullDB(x, scaled)
		want := 20 * math.Log10(math.Abs(1.0-s))
		if math.IsInf(got, 0) || math.IsNaN(got) {
			t.Fatalf("scale %f: want finite, got %f", s, got)
		}
		if math.Abs(got-want) > 0.01 {
			t.Fatalf("scale %f: got %f dB, want %f dB", s, got, want)
		}
	}

	if got := parityNullDB(nil, x); !math.IsInf(got, -1) {
		t.Fatalf("nil reference: want -Inf, got %f", got)
	}

	zeros := make([]float32, len(x))
	if got := parityNullDB(zeros, zeros); !math.IsInf(got, -1) {
		t.Fatalf("zeros reference and input: want -Inf, got %f", got)
	}
	if got := parityNullDB(zeros, x); !math.IsInf(got, 1) {
		t.Fatalf("zero reference with signal: want +Inf, got %f", got)
	}
}

func TestParitySineStereo(t *testing.T) {
	frames := 480
	rate := 48000
	hz := 1000.0
	samples := sineStereo(frames, rate, hz)

	if len(samples) != frames*2 {
		t.Fatalf("got length %d, want %d", len(samples), frames*2)
	}

	for i := 0; i < frames; i++ {
		left := samples[2*i]
		right := samples[2*i+1]
		if left != right {
			t.Fatalf("frame %d: left %f != right %f", i, left, right)
		}
	}

	magL := goertzel(samples, 2, 0, rate, hz)
	magR := goertzel(samples, 2, 1, rate, hz)
	if math.Abs(magL-0.5) > 0.01 {
		t.Fatalf("left magnitude: got %f, want ~0.5", magL)
	}
	if math.Abs(magR-0.5) > 0.01 {
		t.Fatalf("right magnitude: got %f, want ~0.5", magR)
	}

	if got := sineStereo(0, rate, hz); got != nil {
		t.Fatalf("expected nil for 0 frames, got %v", got)
	}
	if got := sineStereo(-10, rate, hz); got != nil {
		t.Fatalf("expected nil for negative frames, got %v", got)
	}
}

func TestParityRenderEffectChunks(t *testing.T) {
	samples := sineStereo(480, 48000, 1000)
	orig := interleaveDeep(samples)
	pass := &parityPassEffect{}

	renderEffectChunks(t, pass, samples, 2, 128)
	if pass.calls != 4 {
		t.Fatalf("got %d calls, want 4", pass.calls)
	}
	if got := parityNullDB(orig, samples); !math.IsInf(got, -1) {
		t.Fatalf("pass-through altered samples: null = %f dB", got)
	}

	errEffect := &parityErrorEffect{err: errors.New("simulated process error")}
	fakeT := new(testing.T)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		renderEffectChunks(fakeT, errEffect, samples, 2, 128)
	}()
	wg.Wait()

	if !fakeT.Failed() {
		t.Fatalf("expected renderEffectChunks to fail on Process error")
	}
}
