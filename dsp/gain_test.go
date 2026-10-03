package dsp

import (
	"math"
	"sync"
	"testing"

	"github.com/dlcuy22/molo/core"
)

func scaleBuffer(n int, start float32) []float32 {
	buf := make([]float32, n)
	for i := range buf {
		buf[i] = start + float32(i)
	}

	return buf
}

func TestGainScalesExactly(t *testing.T) {
	for _, volume := range []float64{0.5, 0.25, 0.125, 2.0 / 3.0} {
		g := NewGain(volume)
		in := scaleBuffer(64, -3)
		want := append([]float32(nil), in...)
		for i := range want {
			want[i] *= float32(volume)
		}

		if err := g.Process(in, 32); err != nil {
			t.Fatalf("Process: %v", err)
		}
		for i := range in {
			if in[i] != want[i] {
				t.Fatalf("volume %v: sample %d = %v, want %v", volume, i, in[i], want[i])
			}
		}
	}
}

func TestGainZeroProducesSilence(t *testing.T) {
	g := NewGain(0)
	in := scaleBuffer(64, 1)

	if err := g.Process(in, 32); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i, s := range in {
		if s != 0 {
			t.Fatalf("sample %d = %v, want 0", i, s)
		}
	}
}

func TestGainOneIsNoOp(t *testing.T) {
	g := NewGain(1)
	in := scaleBuffer(64, -1.25)
	want := append([]float32(nil), in...)

	if err := g.Process(in, 32); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range in {
		if in[i] != want[i] {
			t.Fatalf("sample %d = %v, want unchanged %v", i, in[i], want[i])
		}
	}
}

func TestGainProcessesInPlace(t *testing.T) {
	g := NewGain(0.5)
	in := scaleBuffer(8, 1)
	before := &in[0]

	if err := g.Process(in, 4); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if &in[0] != before {
		t.Fatal("Process replaced the backing array instead of scaling in place")
	}
	if in[0] != 0.5 {
		t.Fatalf("in[0] = %v, want 0.5", in[0])
	}
}

func TestGainOnlyTouchesDeclaredFrames(t *testing.T) {
	// The streamer hands a module a scratch buffer that may be wider than the
	// frames it declared. A gain that walked the whole slice would corrupt the
	// next stage's input.
	g := NewGain(0.5)
	if _, err := g.Configure(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	in := scaleBuffer(8, 1)

	if err := g.Process(in, 3); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := 0; i < 6; i++ {
		if in[i] != float32(i+1)*0.5 {
			t.Fatalf("sample %d = %v, want %v", i, in[i], float32(i+1)*0.5)
		}
	}
	for i := 6; i < len(in); i++ {
		if in[i] != float32(i+1) {
			t.Fatalf("tail sample %d = %v, want untouched %v", i, in[i], float32(i+1))
		}
	}
}

func TestGainConfigureKeepsFormat(t *testing.T) {
	g := NewGain(0.75)
	for _, f := range []core.FrameFormat{
		{Rate: 48000, Ch: 2, Fmt: core.F32},
		{Rate: 44100, Ch: 2, Fmt: core.F32},
		{Rate: 48000, Ch: 1, Fmt: core.F32},
	} {
		out, err := g.Configure(f)
		if err != nil {
			t.Fatalf("Configure(%+v): %v", f, err)
		}
		if !out.Equal(f) {
			t.Fatalf("Configure(%+v) = %+v, want unchanged", f, out)
		}
	}
}

func TestGainResetIsInert(t *testing.T) {
	g := NewGain(0.5)
	if _, err := g.Configure(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if err := g.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := g.Volume(); got != 0.5 {
		t.Fatalf("Volume() = %v after Reset, want 0.5", got)
	}
	in := scaleBuffer(4, 1)
	if err := g.Process(in, 2); err != nil {
		t.Fatalf("Process after Reset: %v", err)
	}
	if in[0] != 0.5 {
		t.Fatalf("in[0] = %v, want 0.5", in[0])
	}
}

func TestGainClampsOutOfRange(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{in: -1, want: MinVolume},
		{in: -0.0001, want: MinVolume},
		{in: 0, want: 0},
		{in: 0.5, want: 0.5},
		{in: 1, want: 1},
		{in: MaxVolume + 0.5, want: MaxVolume},
		{in: math.NaN(), want: MinVolume},
	}
	for _, tc := range cases {
		g := NewGain(tc.in)
		if got := g.Volume(); got != tc.want {
			t.Fatalf("NewGain(%v).Volume() = %v, want %v", tc.in, got, tc.want)
		}
		g.SetVolume(tc.in)
		if got := g.Volume(); got != tc.want {
			t.Fatalf("SetVolume(%v).Volume() = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestGainMaxVolumeIsUnity(t *testing.T) {
	// Values above unity are a limiter's business, not gain's: a post-ring
	// gain cannot stop the result from clipping, so it refuses to amplify.
	if MaxVolume != 1 {
		t.Fatalf("MaxVolume = %v, want 1", MaxVolume)
	}
	if MinVolume != 0 {
		t.Fatalf("MinVolume = %v, want 0", MinVolume)
	}
}

func TestGainProcessEmptyIsInert(t *testing.T) {
	g := NewGain(0.5)
	if err := g.Process(nil, 0); err != nil {
		t.Fatalf("Process(nil, 0): %v", err)
	}
	if err := g.Process([]float32{}, 0); err != nil {
		t.Fatalf("Process(empty, 0): %v", err)
	}
}

func TestGainProcessDoesNotAllocate(t *testing.T) {
	g := NewGain(0.5)
	buf := scaleBuffer(960, 0)

	if n := testing.AllocsPerRun(100, func() {
		if err := g.Process(buf, len(buf)/2); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}); n != 0 {
		t.Fatalf("Process allocated %v times per run, want 0", n)
	}
}

func TestGainSetVolumeDuringProcessIsRaceClean(t *testing.T) {
	g := NewGain(1)
	want := scaleBuffer(960, 0.25)
	buf := append([]float32(nil), want...)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := 0.0; ; v += 0.01 {
			select {
			case <-stop:
				return
			default:
			}
			g.SetVolume(v)
			if v > 1 {
				v = 0
			}
		}
	}()

	for i := 0; i < 2000; i++ {
		if err := g.Process(buf, len(buf)/2); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	// Every sample must still be a finite scale of its input by a value in
	// [0,1], never a torn read of the atomic volume. Comparing to the input
	// alone is enough because Process only ever multiplies.
	for i, s := range buf {
		if math.IsNaN(float64(s)) || s < 0 || s > want[i] {
			t.Fatalf("sample %d = %v after concurrent SetVolume, want within [0, %v]", i, s, want[i])
		}
	}
}

func TestGainSatisfiesModule(t *testing.T) {
	var _ core.Module = (*Gain)(nil)
}
