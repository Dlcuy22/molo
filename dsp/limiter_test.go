package dsp

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
)

func limTestNew(t *testing.T, rate int, values Values) *Limiter {
	t.Helper()

	e, err := NewLimiterFactory().New(values)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lim := e.(*Limiter)
	if _, err := lim.Configure(core.FrameFormat{Rate: rate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	return lim
}

func limTestSineStereoScaled(frames, rate int, hz, amp float64) []float32 {
	out := make([]float32, frames*2)
	omega := 2 * math.Pi * hz / float64(rate)
	for i := 0; i < frames; i++ {
		v := float32(amp * math.Sin(omega*float64(i)))
		out[2*i] = v
		out[2*i+1] = v
	}
	return out
}

func TestLimiterBrickwallTransientBurst(t *testing.T) {
	thresholds := []float64{0.0, -6.0, -12.0, -20.0}
	for _, threshDB := range thresholds {
		lim := limTestNew(t, 48000, Values{
			LimiterThreshold: threshDB,
			LimiterAttack:    1.0,
			LimiterRelease:   10.0,
			LimiterLookahead: 5.0,
		})

		threshLin := float32(dbToLinear(threshDB))
		frames := 4800
		buf := limTestSineStereoScaled(frames, 48000, 1000, 4.0)

		// Inject sudden high-amplitude transient spikes.
		buf[200] = 10.0
		buf[201] = 10.0
		buf[500] = -8.0
		buf[501] = 9.0

		if err := lim.Process(buf, frames); err != nil {
			t.Fatalf("thresh %v dB: Process: %v", threshDB, err)
		}

		tolerance := float32(1e-6)
		maxAllowed := threshLin + tolerance
		for i, s := range buf {
			if s > maxAllowed || s < -maxAllowed {
				t.Fatalf("sample %d at %v dB exceeded threshold: got %v, max %v", i, threshDB, s, maxAllowed)
			}
		}
	}
}

func TestLimiterLookaheadAlignment(t *testing.T) {
	const rate = 48000
	const lookaheadMs = 5.0
	lookaheadSamples := int(math.Round(lookaheadMs * float64(rate) / 1000.0))
	const threshDB = -6.0
	threshLin := float32(dbToLinear(threshDB))

	lim := limTestNew(t, rate, Values{
		LimiterThreshold: threshDB,
		LimiterAttack:    0.5,
		LimiterRelease:   50.0,
		LimiterLookahead: lookaheadMs,
	})

	const quietFrames = 500
	const burstFrames = 500
	const totalFrames = quietFrames + burstFrames
	buf := make([]float32, totalFrames*2)

	// Quiet stretch below threshold.
	const quietAmp = 0.1
	for i := 0; i < quietFrames*2; i++ {
		buf[i] = quietAmp
	}

	// Loud burst well above threshold.
	const burstAmp = 2.5
	for i := quietFrames * 2; i < totalFrames*2; i++ {
		buf[i] = burstAmp
	}

	if err := lim.Process(buf, totalFrames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Verify quiet samples before the burst are not attenuated once emerged.
	for i := lookaheadSamples; i < quietFrames; i++ {
		sampleL := buf[2*i]
		sampleR := buf[2*i+1]
		if math.Abs(float64(sampleL-quietAmp)) > 1e-5 || math.Abs(float64(sampleR-quietAmp)) > 1e-5 {
			t.Fatalf("frame %d quiet sample attenuated before burst: L=%v R=%v", i, sampleL, sampleR)
		}
	}

	// Verify loud burst is limited to threshold with tolerance.
	tolerance := float32(1e-6)
	maxAllowed := threshLin + tolerance
	for i := quietFrames; i < totalFrames; i++ {
		if buf[2*i] > maxAllowed || buf[2*i+1] > maxAllowed {
			t.Fatalf("frame %d in burst exceeded threshold: L=%v R=%v, max %v", i, buf[2*i], buf[2*i+1], maxAllowed)
		}
	}
}

func TestLimiterBelowThresholdPassesUnchanged(t *testing.T) {
	const rate = 48000
	const frames = 4800

	// Zero lookahead passes through with no latency delay.
	limZero := limTestNew(t, rate, Values{
		LimiterThreshold: 0.0,
		LimiterLookahead: 0.0,
	})
	buf := limTestSineStereoScaled(frames, rate, 440, 0.25)
	orig := make([]float32, len(buf))
	copy(orig, buf)

	if err := limZero.Process(buf, frames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if nullDB := parityNullDB(orig, buf); nullDB > -100.0 {
		t.Fatalf("null dB above threshold: got %f dB, want < -100 dB", nullDB)
	}

	// With lookahead, the steady-state output delayed by lookahead frames matches input.
	lookaheadMs := 5.0
	limLook := limTestNew(t, rate, Values{
		LimiterThreshold: 0.0,
		LimiterLookahead: lookaheadMs,
	})
	bufLong := limTestSineStereoScaled(rate, rate, 440, 0.25)
	origLong := make([]float32, len(bufLong))
	copy(origLong, bufLong)

	if err := limLook.Process(bufLong, rate); err != nil {
		t.Fatalf("Process: %v", err)
	}

	lookaheadSamples := int(math.Round(lookaheadMs * float64(rate) / 1000.0))
	start := lookaheadSamples * 2
	refSlice := origLong[:len(bufLong)-start]
	outSlice := bufLong[start:]

	if nullDB := parityNullDB(refSlice, outSlice); nullDB > -100.0 {
		t.Fatalf("lookahead steady-state null dB: got %f dB, want < -100 dB", nullDB)
	}
}

func TestLimiterStereoLinkAsymmetricSignal(t *testing.T) {
	const rate = 48000
	const frames = 4800

	limLinked := limTestNew(t, rate, Values{
		LimiterThreshold:  -6.0,
		LimiterStereoLink: 100.0,
		LimiterAttack:     0.5,
		LimiterRelease:    50.0,
		LimiterLookahead:  0.0,
	})

	// Left channel is loud while right channel is quiet.
	buf := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		buf[2*i] = 2.0
		buf[2*i+1] = 0.1
	}

	if err := limLinked.Process(buf, frames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if limLinked.channelGains[0] != limLinked.channelGains[1] {
		t.Fatalf("gains not equal with 100%% link: L=%v, R=%v", limLinked.channelGains[0], limLinked.channelGains[1])
	}

	// Verify right channel is attenuated by the same gain factor as left.
	lastR := buf[len(buf)-1]
	expectedR := float32(0.1 * limLinked.delayedGains[1])
	if math.Abs(float64(lastR-expectedR)) > 1e-5 {
		t.Fatalf("R output mismatch: got %v, want %v", lastR, expectedR)
	}

	// Compare with unlinked where right channel remains untouched.
	limUnlinked := limTestNew(t, rate, Values{
		LimiterThreshold:  -6.0,
		LimiterStereoLink: 0.0,
		LimiterAttack:     0.5,
		LimiterRelease:    50.0,
		LimiterLookahead:  0.0,
	})
	bufUnlinked := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		bufUnlinked[2*i] = 2.0
		bufUnlinked[2*i+1] = 0.1
	}

	if err := limUnlinked.Process(bufUnlinked, frames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if limUnlinked.delayedGains[1] != 1.0 {
		t.Fatalf("unlinked quiet channel modified: got gain %v, want 1.0", limUnlinked.delayedGains[1])
	}
}

func TestLimiterLatency(t *testing.T) {
	rates := []int{48000, 44100, 96000}
	lookaheads := []float64{0.0, 2.5, 5.0, 10.0, 20.0}

	for _, rate := range rates {
		for _, lookMs := range lookaheads {
			lim := limTestNew(t, rate, Values{LimiterLookahead: lookMs})
			expectedSamples := int(math.Round(lookMs * float64(rate) / 1000.0))
			expectedLatency := time.Duration(expectedSamples) * time.Second / time.Duration(rate)

			if got := lim.Latency(); got != expectedLatency {
				t.Fatalf("rate %d, lookahead %f: Latency() = %v, want %v", rate, lookMs, got, expectedLatency)
			}
		}
	}
}

func TestLimiterProcessDoesNotAllocate(t *testing.T) {
	lim := limTestNew(t, 48000, nil)
	buf := make([]float32, 960)
	for i := range buf {
		buf[i] = 0.5
	}

	if n := testing.AllocsPerRun(100, func() {
		if err := lim.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}); n != 0 {
		t.Fatalf("Process allocated %v times per run, want 0", n)
	}
}

func TestLimiterBypassBitIdentical(t *testing.T) {
	lim := limTestNew(t, 48000, Values{
		LimiterThreshold: -6.0,
		ParamBypass:      true,
	})

	frames := 960
	buf := limTestSineStereoScaled(frames, 48000, 440, 2.0)
	orig := make([]float32, len(buf))
	copy(orig, buf)

	if err := lim.Process(buf, frames); err != nil {
		t.Fatalf("Process: %v", err)
	}

	for i := range buf {
		if buf[i] != orig[i] {
			t.Fatalf("sample %d altered while bypassed: got %v, want %v", i, buf[i], orig[i])
		}
	}

	if nullDB := parityNullDB(orig, buf); !math.IsInf(nullDB, -1) {
		t.Fatalf("null dB not -Inf: got %v", nullDB)
	}
	if !lim.Bypassed() {
		t.Fatal("Bypassed() reported false, want true")
	}
}

func TestLimiterPresetCompatibility(t *testing.T) {
	values := Values{
		LimiterMode:         "Herm Thin",
		LimiterOversampling: "None",
		LimiterThreshold:    -3.0,
	}

	e, err := NewLimiterFactory().New(values)
	if err != nil {
		t.Fatalf("New with preset values failed: %v", err)
	}
	lim := e.(*Limiter)

	if err := lim.Set(LimiterMode, "Herm Duck"); err != nil {
		t.Fatalf("Set mode failed: %v", err)
	}
	if err := lim.Set(LimiterOversampling, "Full x4"); err != nil {
		t.Fatalf("Set oversampling failed: %v", err)
	}

	modeVal, err := lim.Get(LimiterMode)
	if err != nil || modeVal != "Herm Duck" {
		t.Fatalf("Get mode: got %v, err %v", modeVal, err)
	}
	osVal, err := lim.Get(LimiterOversampling)
	if err != nil || osVal != "Full x4" {
		t.Fatalf("Get oversampling: got %v, err %v", osVal, err)
	}

	if err := lim.Set(LimiterMode, "InvalidMode"); err == nil {
		t.Fatal("expected error setting invalid mode, got nil")
	}
}

func TestLimiterNaNLeak(t *testing.T) {
	lim := limTestNew(t, 48000, Values{
		LimiterThreshold: -6.0,
	})

	frames := 480
	buf := make([]float32, frames*2)
	buf[10] = float32(math.NaN())
	buf[11] = float32(math.Inf(1))
	buf[12] = float32(math.Inf(-1))

	if err := lim.Process(buf, frames); err != nil {
		t.Fatalf("Process with NaNs failed: %v", err)
	}

	for i, s := range buf {
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
			t.Fatalf("output sample %d leaked non-finite value: %v", i, s)
		}
	}
}

func TestLimiterSetDuringProcessIsRaceClean(t *testing.T) {
	lim := limTestNew(t, 48000, nil)
	buf := limTestSineStereoScaled(960, 48000, 440, 0.5)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for thresh := -30.0; ; thresh += 1.0 {
			if thresh > 0.0 {
				thresh = -30.0
			}
			select {
			case <-stop:
				return
			default:
				_ = lim.Set(LimiterThreshold, thresh)
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if err := lim.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestLimiterFactoryMetadataAndSchema(t *testing.T) {
	factory := NewLimiterFactory()
	if got := factory.Kind(); got != "limiter" {
		t.Fatalf("Kind: got %q, want %q", got, "limiter")
	}
	if got := factory.Impl(); got != "limiter-lookahead" {
		t.Fatalf("Impl: got %q, want %q", got, "limiter-lookahead")
	}
	if got := factory.FriendlyName(); got != "Limiter" {
		t.Fatalf("FriendlyName: got %q, want %q", got, "Limiter")
	}
	if got := factory.Weight(); got != 50 {
		t.Fatalf("Weight: got %d, want 50", got)
	}
	if got := factory.Placement(); got != Post {
		t.Fatalf("Placement: got %v, want %v", got, Post)
	}

	schema := factory.Schema()
	expectedKeys := []string{
		ParamBypass, ParamInputGain, ParamOutputGain,
		LimiterThreshold, LimiterAttack, LimiterRelease, LimiterLookahead, LimiterStereoLink,
		LimiterMode, LimiterOversampling,
	}
	for _, key := range expectedKeys {
		var found bool
		for _, p := range schema {
			if p.Key == key {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("schema missing expected key %q", key)
		}
	}

	lim := limTestNew(t, 48000, nil)
	if got := lim.Name(); got != "limiter" {
		t.Fatalf("Name: got %q, want %q", got, "limiter")
	}
	if err := lim.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	meters := lim.Meters()
	if _, ok := meters[MeterIn]; !ok {
		t.Fatal("meters missing in reading")
	}
	if _, ok := meters[MeterOut]; !ok {
		t.Fatal("meters missing out reading")
	}
	if _, ok := meters[MeterReduction]; !ok {
		t.Fatal("meters missing reduction reading")
	}
}
