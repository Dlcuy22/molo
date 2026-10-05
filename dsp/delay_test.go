package dsp

import (
	"math"
	"testing"
)

func delayTestAlmostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestDelayLineClampedConstructor(t *testing.T) {
	dl := newDelayLine(0, -2)
	if dl.maxFrames != 1 {
		t.Fatalf("expected maxFrames clamped to 1, got %d", dl.maxFrames)
	}
	if dl.ch != 1 {
		t.Fatalf("expected ch clamped to 1, got %d", dl.ch)
	}
	if len(dl.buf) != 1 {
		t.Fatalf("expected buf len 1, got %d", len(dl.buf))
	}
}

func TestDelayLineDelayZero(t *testing.T) {
	dl := newDelayLine(4, 2)
	dst := make([]float64, 2)

	frames := [][]float64{
		{1.5, -2.5},
		{3.0, 4.0},
		{-10.2, 0.42},
	}
	for _, f := range frames {
		dl.writeFrame(f)
		dl.readFrame(0, dst)
		if dst[0] != f[0] || dst[1] != f[1] {
			t.Fatalf("delay 0 did not pass through: got %v, want %v", dst, f)
		}
	}
}

func TestDelayLineImpulse(t *testing.T) {
	const capacity = 8
	dl := newDelayLine(capacity, 2)
	dst := make([]float64, 2)
	zero := []float64{0, 0}
	impulse := []float64{1.0, -0.5}

	dl.writeFrame(impulse)

	for step := 0; step < capacity; step++ {
		for d := 0; d < capacity; d++ {
			dl.readFrame(d, dst)
			if d == step {
				if dst[0] != impulse[0] || dst[1] != impulse[1] {
					t.Fatalf("step %d: delay %d got %v, want impulse %v", step, d, dst, impulse)
				}
			} else {
				if dst[0] != 0 || dst[1] != 0 {
					t.Fatalf("step %d: delay %d got %v, want zero", step, d, dst)
				}
			}
		}
		if step < capacity-1 {
			dl.writeFrame(zero)
		}
	}

	dl.writeFrame(zero)
	for d := 0; d < capacity; d++ {
		dl.readFrame(d, dst)
		if dst[0] != 0 || dst[1] != 0 {
			t.Fatalf("post-drain delay %d got %v, want zero", d, dst)
		}
	}
}

func TestDelayLineFractional(t *testing.T) {
	dl := newDelayLine(8, 2)
	dst := make([]float64, 2)
	s0 := make([]float64, 2)
	s1 := make([]float64, 2)

	samples := [][]float64{
		{10.0, -20.0},
		{40.0, 10.0},
		{90.0, -30.0},
		{5.0, 80.0},
	}
	for _, s := range samples {
		dl.writeFrame(s)
	}

	for d := 0; d < 3; d++ {
		dl.readFrame(d, s0)
		dl.readFrame(d+1, s1)

		dl.readFrameFrac(float64(d)+0.5, dst)
		want0 := s0[0] + 0.5*(s1[0]-s0[0])
		want1 := s0[1] + 0.5*(s1[1]-s0[1])

		if dst[0] != want0 || dst[1] != want1 {
			t.Fatalf("delay %d.5: got [%v, %v], want [%v, %v]", d, dst[0], dst[1], want0, want1)
		}

		dl.readFrameFrac(float64(d)+0.25, dst)
		wantQuarter0 := s0[0] + 0.25*(s1[0]-s0[0])
		wantQuarter1 := s0[1] + 0.25*(s1[1]-s0[1])
		if !delayTestAlmostEqual(dst[0], wantQuarter0, 1e-12) || !delayTestAlmostEqual(dst[1], wantQuarter1, 1e-12) {
			t.Fatalf("delay %d.25: got %v, want [%v, %v]", d, dst, wantQuarter0, wantQuarter1)
		}
	}
}

func TestDelayLineOutOfRangeClamp(t *testing.T) {
	dl := newDelayLine(4, 1)
	dst := make([]float64, 1)
	val0 := make([]float64, 1)
	valMax := make([]float64, 1)

	for i := 1; i <= 4; i++ {
		dl.writeFrame([]float64{float64(i * 10)})
	}
	dl.readFrame(0, val0)
	dl.readFrame(3, valMax)

	dl.readFrame(-1, dst)
	if dst[0] != val0[0] {
		t.Fatalf("readFrame(-1) = %v, want %v", dst[0], val0[0])
	}
	dl.readFrame(-99, dst)
	if dst[0] != val0[0] {
		t.Fatalf("readFrame(-99) = %v, want %v", dst[0], val0[0])
	}

	dl.readFrame(4, dst)
	if dst[0] != valMax[0] {
		t.Fatalf("readFrame(4) = %v, want %v", dst[0], valMax[0])
	}
	dl.readFrame(100, dst)
	if dst[0] != valMax[0] {
		t.Fatalf("readFrame(100) = %v, want %v", dst[0], valMax[0])
	}

	dl.readFrameFrac(-0.5, dst)
	if dst[0] != val0[0] {
		t.Fatalf("readFrameFrac(-0.5) = %v, want %v", dst[0], val0[0])
	}
	dl.readFrameFrac(3.5, dst)
	if dst[0] != valMax[0] {
		t.Fatalf("readFrameFrac(3.5) = %v, want %v", dst[0], valMax[0])
	}
	dl.readFrameFrac(99.9, dst)
	if dst[0] != valMax[0] {
		t.Fatalf("readFrameFrac(99.9) = %v, want %v", dst[0], valMax[0])
	}
	dl.readFrameFrac(math.NaN(), dst)
	if dst[0] != val0[0] {
		t.Fatalf("readFrameFrac(NaN) = %v, want %v", dst[0], val0[0])
	}
}

func TestDelayLineReset(t *testing.T) {
	dl := newDelayLine(4, 2)
	dst := make([]float64, 2)

	dl.writeFrame([]float64{1, 2})
	dl.writeFrame([]float64{3, 4})
	dl.reset()

	for d := 0; d < 4; d++ {
		dl.readFrame(d, dst)
		if dst[0] != 0 || dst[1] != 0 {
			t.Fatalf("delay %d after reset got %v, want zero", d, dst)
		}
	}

	dl.writeFrame([]float64{99, 100})
	dl.readFrame(0, dst)
	if dst[0] != 99 || dst[1] != 100 {
		t.Fatalf("post-reset delay 0 got %v, want [99, 100]", dst)
	}
}

func TestDelayLineProcess(t *testing.T) {
	dl := newDelayLine(8, 2)
	in := make([]float64, 2)
	out := make([]float64, 2)

	in[0], in[1] = 10.0, 20.0
	dl.process(in, out, 0)
	if out[0] != in[0] || out[1] != in[1] {
		t.Fatalf("delay 0 did not return just-written frame: got %v, want %v", out, in)
	}

	dl.reset()
	const delayN = 3
	history := [][]float64{
		{100, 101},
		{200, 201},
		{300, 301},
		{400, 401},
		{500, 501},
		{600, 601},
	}
	for step, cur := range history {
		dl.process(cur, out, delayN)
		if step < delayN {
			if out[0] != 0 || out[1] != 0 {
				t.Fatalf("step %d: expected zero before delay filled, got %v", step, out)
			}
		} else {
			want := history[step-delayN]
			if out[0] != want[0] || out[1] != want[1] {
				t.Fatalf("step %d: expected delayed frame %v, got %v", step, want, out)
			}
		}
	}
}

func TestDelayLineAllocs(t *testing.T) {
	dl := newDelayLine(32, 2)
	frame := []float64{1.23, -4.56}
	dst := make([]float64, 2)

	allocs := testing.AllocsPerRun(1000, func() {
		dl.writeFrame(frame)
		dl.readFrame(4, dst)
		dl.readFrameFrac(4.5, dst)
		dl.process(frame, dst, 4)
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs per run, got %f", allocs)
	}
}
