package dsp

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/dlcuy22/molo/core"
)

// crossfeedRef is an independent float64 implementation of the bs2b design,
// written directly from the published equations in this test rather than
// sharing code with the effect. It exists so the effect is checked against the
// algorithm, not against itself: if crossfeed.go drifted from the equations,
// this would not follow.
type crossfeedRef struct {
	a0Lo, b1Lo       float64
	a0Hi, a1Hi, b1Hi float64
	gain             float64

	loL, loR     float64
	hiL, hiR     float64
	asisL, asisR float64
}

func newCrossfeedRef(rate int, cutoffHz, feedDB float64) *crossfeedRef {
	level := feedDB

	gbLo := level*-5.0/6.0 - 3.0
	gbHi := level/6.0 - 3.0

	gLo := math.Pow(10, gbLo/20.0)
	gHi := 1.0 - math.Pow(10, gbHi/20.0)
	fcHi := cutoffHz * math.Pow(2, (gbLo-20.0*math.Log10(gHi))/12.0)

	sr := float64(rate)
	xLo := math.Exp(-2.0 * math.Pi * cutoffHz / sr)
	xHi := math.Exp(-2.0 * math.Pi * fcHi / sr)

	return &crossfeedRef{
		a0Lo: gLo * (1.0 - xLo),
		b1Lo: xLo,
		a0Hi: 1.0 - gHi*(1.0-xHi),
		a1Hi: -xHi,
		b1Hi: xHi,
		gain: 1.0 / (1.0 - gHi + gLo),
	}
}

// process applies the reference to interleaved stereo in place.
func (r *crossfeedRef) process(buf []float32) {
	for i := 0; i+1 < len(buf); i += 2 {
		l := float64(buf[i])
		rt := float64(buf[i+1])

		r.loL = r.a0Lo*l + r.b1Lo*r.loL
		r.loR = r.a0Lo*rt + r.b1Lo*r.loR

		r.hiL = r.a0Hi*l + r.a1Hi*r.asisL + r.b1Hi*r.hiL
		r.hiR = r.a0Hi*rt + r.a1Hi*r.asisR + r.b1Hi*r.hiR

		r.asisL = l
		r.asisR = rt

		buf[i] = float32((r.hiL + r.loR) * r.gain)
		buf[i+1] = float32((r.hiR + r.loL) * r.gain)
	}
}

// twoTone builds a deterministic interleaved stereo buffer: 440 Hz on the left,
// 660 Hz on the right, so cross-channel leakage is measurable as the appearance
// of one tone in the other channel.
func twoTone(frames, rate int) []float32 {
	buf := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		t := float64(i) / float64(rate)
		buf[i*2] = float32(0.4 * math.Sin(2*math.Pi*440*t))
		buf[i*2+1] = float32(0.4 * math.Sin(2*math.Pi*660*t))
	}

	return buf
}

func newTestCrossfeed(t *testing.T, rate int, values Values) *Crossfeed {
	t.Helper()

	e, err := NewCrossfeedFactory().New(values)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cf := e.(*Crossfeed)
	if _, err := cf.Configure(core.FrameFormat{Rate: rate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	return cf
}

func TestCrossfeedMatchesIndependentReference(t *testing.T) {
	const (
		rate   = 48000
		frames = 48000 // one second, long enough for the filters to settle
		feed   = 4.5
	)
	for _, cutoff := range []float64{300, 700, 1000, 2000} {
		cf := newTestCrossfeed(t, rate, Values{CrossfeedCutoff: cutoff, CrossfeedFeed: feed})

		got := twoTone(frames, rate)
		want := interleaveDeep(got)
		ref := newCrossfeedRef(rate, cutoff, feed)
		ref.process(want)

		if err := cf.Process(got, frames); err != nil {
			t.Fatalf("cutoff %v: Process: %v", cutoff, err)
		}
		for i := range got {
			// The two implementations share equations and float64 state, so
			// the only difference is operation order. A tight tolerance still
			// catches a real divergence.
			if d := math.Abs(float64(got[i] - want[i])); d > 1e-6 {
				t.Fatalf("cutoff %v: sample %d = %v, reference %v (diff %g)", cutoff, i, got[i], want[i], d)
			}
		}
	}
}

func TestCrossfeedLeakageFallsAsFeedRises(t *testing.T) {
	const (
		rate   = 48000
		frames = 48000
	)
	// The right channel carries 660 Hz only. After crossfeed, 440 Hz appears
	// in it: that is the leakage the effect exists to create.
	//
	// Feed is inverse to strength. The reference presets say so: Jmeier at
	// 9.5 dB is "little change", Cmoy at 6 dB is stronger, and the default
	// 4.5 dB sits between them, so a higher feed must leak less.
	leak := func(feed float64) float64 {
		cf := newTestCrossfeed(t, rate, Values{CrossfeedFeed: feed})
		buf := twoTone(frames, rate)
		if err := cf.Process(buf, frames); err != nil {
			t.Fatalf("Process: %v", err)
		}

		return goertzel(buf, 2, 1, rate, 440)
	}

	strong := leak(1.0)
	mid := leak(4.5)
	weak := leak(15.0)
	t.Logf("right-channel 440 Hz leak: feed 1.0 = %.6f, 4.5 = %.6f, 15.0 = %.6f", strong, mid, weak)

	if !(strong > mid && mid > weak) {
		t.Fatalf("leakage does not fall as feed rises: %.6f, %.6f, %.6f", strong, mid, weak)
	}
}

func TestCrossfeedLowFrequencyCrossesMoreThanHigh(t *testing.T) {
	const (
		rate   = 48000
		frames = 48000
	)
	cf := newTestCrossfeed(t, rate, Values{CrossfeedFeed: 4.5})

	buf := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		// Left gets a low tone, right gets a high tone.
		t := float64(i) / float64(rate)
		buf[i*2] = float32(0.4 * math.Sin(2*math.Pi*80*t))
		buf[i*2+1] = float32(0.4 * math.Sin(2*math.Pi*8000*t))
	}
	if err := cf.Process(buf, frames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// The low tone must leak into the right channel more than the high tone
	// leaks into the left. That is the low-pass shape of the crossed path.
	lowIntoRight := goertzel(buf, 2, 1, rate, 80)
	highIntoLeft := goertzel(buf, 2, 0, rate, 8000)
	t.Logf("80 Hz into right = %.6f, 8000 Hz into left = %.6f", lowIntoRight, highIntoLeft)

	if !(lowIntoRight > highIntoLeft) {
		t.Fatalf("crossfeed is not low-pass: low leak %.6f, high leak %.6f", lowIntoRight, highIntoLeft)
	}
}

func TestCrossfeedBypassPassesAudioUnchanged(t *testing.T) {
	cf := newTestCrossfeed(t, 48000, Values{ParamBypass: true})
	buf := twoTone(4096, 48000)
	want := interleaveDeep(buf)

	if err := cf.Process(buf, 2048); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range buf {
		if buf[i] != want[i] {
			t.Fatalf("sample %d changed while bypassed: %v vs %v", i, buf[i], want[i])
		}
	}
}

func TestCrossfeedResetClearsFilterState(t *testing.T) {
	cf := newTestCrossfeed(t, 48000, nil)
	buf := twoTone(48000, 48000)
	if err := cf.Process(buf, 48000); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := cf.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if cf.loL != 0 || cf.loR != 0 || cf.hiL != 0 || cf.hiR != 0 || cf.asisL != 0 || cf.asisR != 0 {
		t.Fatal("Reset left filter state behind")
	}
}

func TestCrossfeedMonoPassesThrough(t *testing.T) {
	e, err := NewCrossfeedFactory().New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cf := e.(*Crossfeed)
	if _, err := cf.Configure(core.FrameFormat{Rate: 48000, Ch: 1, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	buf := []float32{0.1, -0.2, 0.3, -0.4}
	want := interleaveDeep(buf)
	if err := cf.Process(buf, 4); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range buf {
		if buf[i] != want[i] {
			t.Fatalf("mono sample %d changed: %v vs %v", i, buf[i], want[i])
		}
	}
}

func TestCrossfeedOnlyTouchesDeclaredFrames(t *testing.T) {
	cf := newTestCrossfeed(t, 48000, nil)
	buf := twoTone(64, 48000)
	tail := interleaveDeep(buf[8:])

	if err := cf.Process(buf, 4); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range tail {
		if buf[8+i] != tail[i] {
			t.Fatalf("tail sample %d changed: %v vs %v", i, buf[8+i], tail[i])
		}
	}
}

func TestCrossfeedSetIsAtomicAndValidated(t *testing.T) {
	cf := newTestCrossfeed(t, 48000, nil)

	// A bad key and a bad type are rejected without changing the effect.
	if err := cf.Set("nope", 1.0); !errors.Is(err, ErrUnknownParam) {
		t.Fatalf("Set(unknown) = %v, want ErrUnknownParam", err)
	}
	if err := cf.Set(CrossfeedCutoff, "loud"); !errors.Is(err, ErrUnknownParam) {
		t.Fatalf("Set(non-numeric) = %v, want ErrUnknownParam", err)
	}
	// An out-of-range number clamps rather than erroring, like a slider.
	if err := cf.Set(CrossfeedCutoff, 99999.0); err != nil {
		t.Fatalf("Set(clamped): %v", err)
	}
	if got := cf.store.Float(CrossfeedCutoff); got != crossfeedMaxCutoff {
		t.Fatalf("clamped cutoff = %v, want %v", got, crossfeedMaxCutoff)
	}
	if err := cf.Set(CrossfeedCutoff, -50.0); err != nil {
		t.Fatalf("Set(clamped low): %v", err)
	}
	if got := cf.store.Float(CrossfeedCutoff); got != crossfeedMinCutoff {
		t.Fatalf("clamped cutoff = %v, want %v", got, crossfeedMinCutoff)
	}
}

func TestCrossfeedFeedTenthsRoundTrip(t *testing.T) {
	// The feed is stored in dB and compiled to tenths. A stored value must
	// select the same tenths the reference preset names: 4.5 dB is 45.
	for _, tc := range []struct {
		feed   float64
		tenths int
	}{
		{1.0, 10},
		{4.5, 45},
		{9.5, 95},
		{15.0, 150},
	} {
		cf := newTestCrossfeed(t, 48000, Values{CrossfeedFeed: tc.feed})
		if got := cf.state.Load().feedTenths; got != tc.tenths {
			t.Fatalf("feed %v compiled to %d tenths, want %d", tc.feed, got, tc.tenths)
		}
	}
}

func TestCrossfeedConfigureRejectsBadFormat(t *testing.T) {
	e, err := NewCrossfeedFactory().New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cf := e.(*Crossfeed)
	if _, err := cf.Configure(core.FrameFormat{Rate: 0, Ch: 2, Fmt: core.F32}); err == nil {
		t.Fatal("Configure accepted a zero rate")
	}
	if _, err := cf.Configure(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.S16}); err == nil {
		t.Fatal("Configure accepted an integer sample format")
	}
}

func TestCrossfeedProcessDoesNotAllocate(t *testing.T) {
	cf := newTestCrossfeed(t, 48000, nil)
	buf := twoTone(960, 48000)

	if n := testing.AllocsPerRun(100, func() {
		if err := cf.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}); n != 0 {
		t.Fatalf("Process allocated %v times per run, want 0", n)
	}
}

func TestCrossfeedSetDuringProcessIsRaceClean(t *testing.T) {
	// Set rebuilds the audio state while Process is running. The race detector
	// is the assertion: a lock or a shared field on the audio path would show
	// up here. Every output must stay finite and bounded, never a torn read.
	cf := newTestCrossfeed(t, 48000, nil)
	buf := twoTone(960, 48000)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for cutoff := 300.0; ; cutoff += 25 {
			select {
			case <-stop:
				return
			default:
			}
			if cutoff > crossfeedMaxCutoff {
				cutoff = 300
			}
			if err := cf.Set(CrossfeedCutoff, cutoff); err != nil {
				return
			}
			if err := cf.Set(CrossfeedFeed, 1.0+(cutoff-300)/100); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 2000; i++ {
		if err := cf.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	for i, s := range buf {
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) || math.Abs(float64(s)) > 4 {
			t.Fatalf("sample %d = %v after concurrent Set", i, s)
		}
	}
}

func TestCrossfeedInputOutputGainApply(t *testing.T) {
	// The standard gains wrap the effect's own work: input before it, output
	// after it. A +6.02 dB input gain must double the level a unity setting
	// produces, and an output gain must scale the result.
	base := newTestCrossfeed(t, 48000, Values{CrossfeedFeed: 4.5})
	on := newTestCrossfeed(t, 48000, Values{
		CrossfeedFeed:   4.5,
		ParamInputGain:  6.0206, // ~x2
		ParamOutputGain: -6.0206,
	})

	a := twoTone(48000, 48000)
	b := interleaveDeep(a)
	if err := base.Process(a, 48000); err != nil {
		t.Fatalf("Process base: %v", err)
	}
	if err := on.Process(b, 48000); err != nil {
		t.Fatalf("Process on: %v", err)
	}

	// Input x2 then output /2 cancels, so the two runs must agree closely.
	for i := range a {
		if d := math.Abs(float64(a[i] - b[i])); d > 1e-5 {
			t.Fatalf("sample %d: unity %v, gained %v (diff %g)", i, a[i], b[i], d)
		}
	}
}

func TestCrossfeedGainsAreNoOpAtUnity(t *testing.T) {
	// Zero dB must not introduce a rounding step that a null effect would show.
	without := newTestCrossfeed(t, 48000, Values{CrossfeedFeed: 4.5})
	with := newTestCrossfeed(t, 48000, Values{
		CrossfeedFeed:   4.5,
		ParamInputGain:  0.0,
		ParamOutputGain: 0.0,
	})

	a := twoTone(4096, 48000)
	b := interleaveDeep(a)
	if err := without.Process(a, 2048); err != nil {
		t.Fatalf("Process without: %v", err)
	}
	if err := with.Process(b, 2048); err != nil {
		t.Fatalf("Process with: %v", err)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sample %d differs: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestCrossfeedSatisfiesEffect(t *testing.T) {
	var _ Effect = (*Crossfeed)(nil)
	var _ Bypassable = (*Crossfeed)(nil)
}
