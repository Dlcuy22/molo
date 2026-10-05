package dsp

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
)

func maxTestNewMaximizer(t *testing.T, rate, ch int, values Values) *Maximizer {
	t.Helper()

	e, err := NewMaximizerFactory().New(values)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := e.(*Maximizer)
	if !ok {
		t.Fatalf("expected *Maximizer, got %T", e)
	}
	if _, err := m.Configure(core.FrameFormat{Rate: rate, Ch: ch, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	return m
}

func TestMaximizerFactoryRegistration(t *testing.T) {
	f := NewMaximizerFactory()
	if got := f.Kind(); got != "maximizer" {
		t.Fatalf("Kind: got %q, want maximizer", got)
	}
	if got := f.Impl(); got != "maximizer-dafx" {
		t.Fatalf("Impl: got %q, want maximizer-dafx", got)
	}
	if got := f.FriendlyName(); got != "Maximizer" {
		t.Fatalf("FriendlyName: got %q, want Maximizer", got)
	}
	if got := f.Weight(); got != 50 {
		t.Fatalf("Weight: got %d, want 50", got)
	}
	if got := f.Placement(); got != Post {
		t.Fatalf("Placement: got %v, want Post", got)
	}

	winner, err := Default.Winner("maximizer")
	if err != nil {
		t.Fatalf("Winner(maximizer): %v", err)
	}
	if winner.Impl() != "maximizer-dafx" {
		t.Fatalf("Default winner: got %q, want maximizer-dafx", winner.Impl())
	}
}

func TestMaximizerTransientBurstCeiling(t *testing.T) {
	const (
		rate        = 48000
		burstFrames = 480
		totalFrames = 1440
		ch          = 2
		ceilingDB   = -1.5
		thresholdDB = -6.0
	)
	m := maxTestNewMaximizer(t, rate, ch, Values{
		MaximizerThreshold: thresholdDB,
		MaximizerCeiling:   ceilingDB,
		MaximizerRelease:   10.0,
	})

	burst := sineStereo(burstFrames, rate, 1000)
	buf := make([]float32, totalFrames*ch)
	// Place an amplified transient burst starting at frame 240.
	start := 240 * ch
	for i := range burst {
		buf[start+i] = burst[i] * 4.0
	}

	if err := m.Process(buf, totalFrames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	ceilingLinear := dbToLinear(ceilingDB)
	var maxPeak float64
	for i, s := range buf {
		v := math.Abs(float64(s))
		if v > maxPeak {
			maxPeak = v
		}
		if v > ceilingLinear+1e-5 {
			t.Fatalf("sample %d exceeded ceiling: got %v, ceiling %v", i, v, ceilingLinear)
		}
	}

	if maxPeak < 0.5 {
		t.Fatalf("output peak unexpectedly low: %v", maxPeak)
	}

	meters := m.Meters()
	red, ok := meters[MeterReduction]
	if !ok {
		t.Fatalf("missing %q in Meters: %v", MeterReduction, meters)
	}
	if red >= 0 {
		t.Fatalf("expected negative gain reduction in dB, got %v", red)
	}
}

func TestMaximizerImpulseAlignmentNoPreDip(t *testing.T) {
	const (
		rate        = 48000
		ch          = 2
		totalFrames = 1000
		impulsePos  = 500
		thresholdDB = -6.0
		ceilingDB   = -1.0
		quietLevel  = float32(0.2)
	)
	m := maxTestNewMaximizer(t, rate, ch, Values{
		MaximizerThreshold: thresholdDB,
		MaximizerCeiling:   ceilingDB,
		MaximizerRelease:   10.0,
	})

	st := m.state.Load()
	lookahead := st.lookahead

	buf := make([]float32, totalFrames*ch)
	for i := 0; i < totalFrames*ch; i++ {
		buf[i] = quietLevel
	}
	// Loud impulse above threshold in both channels.
	buf[impulsePos*ch] = 2.0
	buf[impulsePos*ch+1] = 2.0

	if err := m.Process(buf, totalFrames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	ceilingLinear := dbToLinear(ceilingDB)

	// Quiet samples emerging before the delayed impulse must not suffer pre-dip attenuation.
	for frame := lookahead; frame < lookahead+impulsePos; frame++ {
		for c := 0; c < ch; c++ {
			s := buf[frame*ch+c]
			if math.Abs(float64(s-quietLevel)) > 1e-5 {
				t.Fatalf("pre-dip at frame %d ch %d: got %v, want %v", frame, c, s, quietLevel)
			}
		}
	}

	// Output at delayed impulse position must not exceed ceiling.
	delayedImpulseFrame := lookahead + impulsePos
	for c := 0; c < ch; c++ {
		s := math.Abs(float64(buf[delayedImpulseFrame*ch+c]))
		if s > ceilingLinear+1e-5 {
			t.Fatalf("impulse at frame %d ch %d exceeded ceiling: got %v, ceiling %v", delayedImpulseFrame, c, s, ceilingLinear)
		}
	}

	// Peak over the entire buffer must not exceed ceiling.
	for i, s := range buf {
		if v := math.Abs(float64(s)); v > ceilingLinear+1e-5 {
			t.Fatalf("sample %d exceeded ceiling: got %v, ceiling %v", i, v, ceilingLinear)
		}
	}
}

func TestMaximizerSubThresholdPassThrough(t *testing.T) {
	const (
		rate        = 48000
		frames      = 480
		ch          = 2
		thresholdDB = -3.0
	)
	m := maxTestNewMaximizer(t, rate, ch, Values{
		MaximizerThreshold: thresholdDB,
		MaximizerCeiling:   0.0,
	})

	st := m.state.Load()
	lookahead := st.lookahead
	totalFrames := frames + lookahead

	raw := sineStereo(frames, rate, 1000)
	// Amplitude 0.5 * 0.4 = 0.2 (-14 dBFS), safely below -3 dBFS threshold.
	for i := range raw {
		raw[i] *= 0.4
	}
	orig := interleaveDeep(raw)

	buf := make([]float32, totalFrames*ch)
	copy(buf, raw)

	if err := m.Process(buf, totalFrames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Compare original input against delayed output.
	delayedOutput := buf[lookahead*ch : (lookahead+frames)*ch]
	nullDB := parityNullDB(orig, delayedOutput)
	if !math.IsInf(nullDB, -1) && nullDB > -100.0 {
		t.Fatalf("sub-threshold signal altered: null = %f dB", nullDB)
	}
}

func TestMaximizerLatency(t *testing.T) {
	rates := []int{44100, 48000, 96000}
	for _, rate := range rates {
		m := maxTestNewMaximizer(t, rate, 2, nil)
		latent, ok := any(m).(Latent)
		if !ok {
			t.Fatalf("expected Maximizer to implement Latent")
		}

		st := m.state.Load()
		want := time.Duration(st.lookahead) * time.Second / time.Duration(st.rate)
		if got := latent.Latency(); got != want {
			t.Fatalf("rate %d: Latency() = %v, want %v", rate, got, want)
		}
	}
}

func TestMaximizerProcessAllocations(t *testing.T) {
	m := maxTestNewMaximizer(t, 48000, 2, nil)
	buf := sineStereo(480, 48000, 1000)

	// Warm up filters.
	if err := m.Process(buf, 480); err != nil {
		t.Fatalf("Process warmup: %v", err)
	}

	allocs := testing.AllocsPerRun(100, func() {
		if err := m.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	})
	if allocs > 0 {
		t.Fatalf("Process allocated %v times per run", allocs)
	}
}

func TestMaximizerBypassBitIdentical(t *testing.T) {
	m := maxTestNewMaximizer(t, 48000, 2, Values{ParamBypass: true})
	if !m.Bypassed() {
		t.Fatal("expected Bypassed() to be true")
	}

	buf := sineStereo(480, 48000, 1000)
	for i := range buf {
		buf[i] *= 4.0
	}
	orig := interleaveDeep(buf)

	if err := m.Process(buf, 480); err != nil {
		t.Fatalf("Process: %v", err)
	}

	for i := range buf {
		if buf[i] != orig[i] {
			t.Fatalf("bypassed sample %d changed: %v vs %v", i, buf[i], orig[i])
		}
	}

	meters := m.Meters()
	if red := meters[MeterReduction]; red != 0 {
		t.Fatalf("bypassed reduction = %v, want 0", red)
	}
}

func TestMaximizerParameterValidationAndClamping(t *testing.T) {
	m := maxTestNewMaximizer(t, 48000, 2, nil)

	if err := m.Set("nonexistent", 1.0); !errors.Is(err, ErrUnknownParam) {
		t.Fatalf("Set(nonexistent) = %v, want ErrUnknownParam", err)
	}
	if err := m.Set(MaximizerThreshold, "low"); !errors.Is(err, ErrUnknownParam) {
		t.Fatalf("Set(string) = %v, want ErrUnknownParam", err)
	}

	if err := m.Set(MaximizerThreshold, -50.0); err != nil {
		t.Fatalf("Set(clamp low): %v", err)
	}
	if got := m.store.Float(MaximizerThreshold); got != maximizerMinThreshold {
		t.Fatalf("clamped threshold = %v, want %v", got, maximizerMinThreshold)
	}

	if err := m.Set(MaximizerThreshold, 10.0); err != nil {
		t.Fatalf("Set(clamp high): %v", err)
	}
	if got := m.store.Float(MaximizerThreshold); got != maximizerMaxThreshold {
		t.Fatalf("clamped threshold = %v, want %v", got, maximizerMaxThreshold)
	}
}

func TestMaximizerConfigureValidation(t *testing.T) {
	e, err := NewMaximizerFactory().New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := e.(*Maximizer)

	if _, err := m.Configure(core.FrameFormat{Rate: 0, Ch: 2, Fmt: core.F32}); err == nil {
		t.Fatal("Configure accepted zero rate")
	}
	if _, err := m.Configure(core.FrameFormat{Rate: 48000, Ch: 0, Fmt: core.F32}); err == nil {
		t.Fatal("Configure accepted zero channels")
	}
	if _, err := m.Configure(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.S16}); err == nil {
		t.Fatal("Configure accepted non-float32 format")
	}
}

func TestMaximizerResetClearsState(t *testing.T) {
	m := maxTestNewMaximizer(t, 48000, 2, Values{
		MaximizerThreshold: -6.0,
		MaximizerCeiling:   -1.0,
	})

	loud := sineStereo(480, 48000, 1000)
	for i := range loud {
		loud[i] *= 4.0
	}
	if err := m.Process(loud, 480); err != nil {
		t.Fatalf("Process loud: %v", err)
	}

	if err := m.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	silent := make([]float32, 480*2)
	if err := m.Process(silent, 480); err != nil {
		t.Fatalf("Process silent: %v", err)
	}
	for i, s := range silent {
		if s != 0 {
			t.Fatalf("post-reset sample %d non-zero: %v", i, s)
		}
	}
}
