package dsp

import (
	"math"
	"testing"

	"github.com/dlcuy22/molo/core"
)

var (
	_ Metered = (*Crossfeed)(nil)
	_ Metered = (*Fade)(nil)
)

// sineBuffer fills an interleaved buffer with a sine of the given amplitude.
// The frequency only has to be non-trivial for the RMS to approach the ideal
// amp/sqrt(2), which it does for any whole number of cycles.
func sineBuffer(frames, ch int, amp, freq, rate float64) []float32 {
	buf := make([]float32, frames*ch)
	for i := 0; i < frames; i++ {
		s := float32(amp * math.Sin(2*math.Pi*freq*float64(i)/rate))
		for c := 0; c < ch; c++ {
			buf[i*ch+c] = s
		}
	}

	return buf
}

// TestMeterTracksSine checks a pure tone: RMS settles near amp/sqrt(2) and peak
// approaches amp from below. The tolerance is loose on purpose: these are
// exponentially smoothed accumulators, not exact statistics, and the assertion
// is about the meter being in the right place, not a bit-exact mean.
func TestMeterTracksSine(t *testing.T) {
	const (
		frames = 4800 // 100 ms at 48 kHz, many cycles
		rate   = 48000.0
		amp    = 0.5
	)

	m := NewMeter()
	buf := sineBuffer(frames, 1, amp, 1000, rate)
	// The RMS mean is smoothed across pushes, so let it settle. 101 pushes of
	// the same tone leaves it within a percent of the running mean the signal
	// implies; the odd count lands the read on a fresh peak rather than partway
	// through a release, since a rise and a decay alternate.
	for i := 0; i < 101; i++ {
		m.Push(buf, frames, 1)
	}

	got := m.Read()
	if math.Abs(float64(got.Peak)-amp) > 0.02 {
		t.Fatalf("Peak = %v, want ~%v", got.Peak, amp)
	}

	wantRMS := amp / math.Sqrt2
	if math.Abs(float64(got.RMS)-wantRMS) > 0.02 {
		t.Fatalf("RMS = %v, want ~%v", got.RMS, wantRMS)
	}
}

// TestMeterPeakDecays pins the release policy: after a loud block, feeding
// silence must bring the held peak down rather than leaving it latched forever.
func TestMeterPeakDecays(t *testing.T) {
	m := NewMeter()
	loud := sineBuffer(480, 2, 0.9, 1000, 48000)
	m.Push(loud, 480, 2)

	loudPeak := m.Read().Peak
	if loudPeak < 0.5 {
		t.Fatalf("after a loud block Peak = %v, want a high value", loudPeak)
	}

	silence := make([]float32, 480*2)
	for i := 0; i < 200; i++ {
		m.Push(silence, 480, 2)
	}
	if got := m.Read().Peak; got >= loudPeak {
		t.Fatalf("Peak after silence = %v, want below %v", got, loudPeak)
	}
}

// TestMeterSilenceIsFloor checks that nothing reads as the floor rather than
// -Inf or NaN, which a UI could not render and arithmetic would poison.
func TestMeterSilenceIsFloor(t *testing.T) {
	m := NewMeter()
	peakDB, rmsDB := m.Read().DB()
	if peakDB != dBFloor || rmsDB != dBFloor {
		t.Fatalf("fresh meter DB = (%v, %v), want (%v, %v)", peakDB, rmsDB, dBFloor, dBFloor)
	}
	if math.IsInf(float64(peakDB), 0) || math.IsNaN(float64(peakDB)) {
		t.Fatalf("fresh meter peakDB = %v, want finite", peakDB)
	}

	buf := make([]float32, 960)
	m.Push(buf, 480, 2)
	peakDB, rmsDB = m.Read().DB()
	if peakDB != dBFloor || rmsDB != dBFloor {
		t.Fatalf("silent block DB = (%v, %v), want floor", peakDB, rmsDB)
	}
}

// TestMeterStereoAsymmetry documents the stereo contract: Peak is the maximum
// across channels, so a loud left channel is not hidden by a silent right one,
// while RMS is the mean power across all samples, so it is halved relative to a
// block where both channels carried the same signal.
func TestMeterStereoAsymmetry(t *testing.T) {
	const (
		frames = 4800
		amp    = 0.8
	)

	buf := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		buf[i*2] = float32(amp * math.Sin(2*math.Pi*1000*float64(i)/48000))
		buf[i*2+1] = 0
	}

	m := NewMeter()
	for i := 0; i < 101; i++ {
		m.Push(buf, frames, 2)
	}
	got := m.Read()

	if math.Abs(float64(got.Peak)-amp) > 0.02 {
		t.Fatalf("Peak = %v, want ~%v (max across channels)", got.Peak, amp)
	}
	// Mean power is half the one-channel power, so RMS is the one-channel
	// value over sqrt(2).
	wantRMS := amp / 2
	if math.Abs(float64(got.RMS)-wantRMS) > 0.02 {
		t.Fatalf("RMS = %v, want ~%v (mean power across all samples)", got.RMS, wantRMS)
	}
}

// TestMeterResetClears checks Reset returns the accumulator to its initial
// state, including after a loud block.
func TestMeterResetClears(t *testing.T) {
	m := NewMeter()
	loud := sineBuffer(480, 2, 0.9, 1000, 48000)
	m.Push(loud, 480, 2)
	if m.Read().Peak == 0 {
		t.Fatal("meter did not register a loud block")
	}

	m.Reset()
	got := m.Read()
	if got.Peak != 0 || got.RMS != 0 {
		t.Fatalf("after Reset = %+v, want zero", got)
	}
	peakDB, rmsDB := got.DB()
	if peakDB != dBFloor || rmsDB != dBFloor {
		t.Fatalf("after Reset DB = (%v, %v), want floor", peakDB, rmsDB)
	}
}

// TestMeteredProcessAllocatesNothing asserts the real-time contract: neither
// effect allocates in Process once configured, with the meters active.
func TestMeteredProcessAllocatesNothing(t *testing.T) {
	cf, err := NewCrossfeedFactory().New(nil)
	if err != nil {
		t.Fatalf("build crossfeed: %v", err)
	}
	c := cf.(*Crossfeed)
	if _, err := c.Configure(core.CanonicalFormat); err != nil {
		t.Fatalf("configure crossfeed: %v", err)
	}
	cfBuf := make([]float32, 480*2)
	allocs := testing.AllocsPerRun(1000, func() {
		_ = c.Process(cfBuf, 480)
	})
	if allocs != 0 {
		t.Fatalf("Crossfeed.Process allocations = %v, want 0", allocs)
	}

	fd, err := NewFadeFactory().New(Values{FadeDuration: 0.0, FadeMode: FadeModeIn})
	if err != nil {
		t.Fatalf("build fade: %v", err)
	}
	f := fd.(*Fade)
	if _, err := f.Configure(core.CanonicalFormat); err != nil {
		t.Fatalf("configure fade: %v", err)
	}
	fdBuf := make([]float32, 480*2)
	allocs = testing.AllocsPerRun(1000, func() {
		_ = f.Process(fdBuf, 480)
	})
	if allocs != 0 {
		t.Fatalf("Fade.Process allocations = %v, want 0", allocs)
	}
}

// TestMeteredCrossfeedReportsLevels is the print, do not listen test: it runs a
// sine through crossfeed and logs the in/out dB so the trace is visible under
// -v, then checks the reading is finite and keyed as the UI expects.
func TestMeteredCrossfeedReportsLevels(t *testing.T) {
	c := newTestCrossfeed(t, 48000, nil)
	buf := sineBuffer(480, 2, 0.5, 1000, 48000)
	if err := c.Process(buf, 480); err != nil {
		t.Fatalf("Process: %v", err)
	}

	m := c.Meters()
	inDB, ok := m[MeterIn]
	if !ok {
		t.Fatalf("Meters missing %q: %v", MeterIn, m)
	}
	outDB, ok := m[MeterOut]
	if !ok {
		t.Fatalf("Meters missing %q: %v", MeterOut, m)
	}
	if math.IsInf(float64(inDB), 0) || math.IsNaN(float64(inDB)) {
		t.Fatalf("in dB = %v, want finite", inDB)
	}
	if math.IsInf(float64(outDB), 0) || math.IsNaN(float64(outDB)) {
		t.Fatalf("out dB = %v, want finite", outDB)
	}
	t.Logf("crossfeed meters: in=%.2f dB out=%.2f dB", inDB, outDB)
}

// TestMeterRecoversFromNonFiniteBlocks guards the latch: the update multiplies
// the previous value, so one NaN or Inf would otherwise stay NaN or Inf forever
// and leave MeterReading non-finite, which callers rely on being real numbers.
func TestMeterRecoversFromNonFiniteBlocks(t *testing.T) {
	good := []float32{0.5, 0.5}
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		m := NewMeter()
		m.Push([]float32{bad, bad}, 1, 2)
		for i := 0; i < 100; i++ {
			m.Push(good, 1, 2)
		}

		r := m.Read()
		if math.IsNaN(float64(r.Peak)) || math.IsInf(float64(r.Peak), 0) {
			t.Errorf("Peak after a non-finite block = %v, want finite", r.Peak)
		}
		if math.IsNaN(float64(r.RMS)) || math.IsInf(float64(r.RMS), 0) {
			t.Errorf("RMS after a non-finite block (%v) = %v, want finite", bad, r.RMS)
		}
	}
}

// TestMeterIgnoresPartialTrailingFrame pins the frame alignment: a buffer whose
// length is not a whole number of frames must measure only the samples the
// effect processed, matching the effects' own n -= n % ch.
func TestMeterIgnoresPartialTrailingFrame(t *testing.T) {
	m := NewMeter()
	// Two whole frames (0.1) plus one orphan (0.9). ch=2 drops the orphan.
	m.Push([]float32{0.1, 0.1, 0.9}, 2, 2)
	if got := m.Read().Peak; got != 0.1 {
		t.Fatalf("Peak = %v, want 0.1: the trailing partial frame must not be measured", got)
	}

	// A short buffer (fewer samples than frames*ch) is clamped to whole frames.
	m2 := NewMeter()
	m2.Push([]float32{0.1, 0.1, 0.9}, 5, 2)
	if got := m2.Read().Peak; got != 0.1 {
		t.Fatalf("Peak = %v, want 0.1 for a short buffer", got)
	}
}

// TestStandardParamKeysMatchStandardParams keeps the key list and the schema in
// step: a caller that pins the base controls by key must find exactly the
// parameters StandardParams emits.
func TestStandardParamKeysMatchStandardParams(t *testing.T) {
	keys := StandardParamKeys()
	params := StandardParams()
	if len(keys) != len(params) {
		t.Fatalf("StandardParamKeys has %d keys, StandardParams has %d params", len(keys), len(params))
	}
	for i, p := range params {
		if p.Key != keys[i] {
			t.Errorf("position %d: StandardParams key %q, StandardParamKeys %q", i, p.Key, keys[i])
		}
		if !CommonParam(p.Key) {
			t.Errorf("CommonParam(%q) = false, but StandardParams emits it", p.Key)
		}
	}
}
