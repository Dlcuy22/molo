package dsp

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/dlcuy22/molo/core"
)

func bassTestNewEnhancer(t *testing.T, rate int, values Values) *BassEnhancer {
	t.Helper()

	e, err := NewBassEnhancerFactory().New(values)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	be := e.(*BassEnhancer)
	if _, err := be.Configure(core.FrameFormat{Rate: rate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	return be
}

func bassTestSine(frames, rate int, hz float64, amp float64) []float32 {
	buf := make([]float32, frames*2)
	omega := 2 * math.Pi * hz / float64(rate)
	for i := 0; i < frames; i++ {
		v := float32(amp * math.Sin(omega*float64(i)))
		buf[i*2] = v
		buf[i*2+1] = v
	}

	return buf
}

func TestBassEnhancerRegistration(t *testing.T) {
	fact, err := Default.Winner("bass-enhancer")
	if err != nil {
		t.Fatalf("Winner(bass-enhancer): %v", err)
	}
	if fact.Kind() != "bass-enhancer" {
		t.Fatalf("Kind = %q, want bass-enhancer", fact.Kind())
	}
	if fact.Impl() != "bass-enhancer-harmonic" {
		t.Fatalf("Impl = %q, want bass-enhancer-harmonic", fact.Impl())
	}
	if fact.FriendlyName() != "Bass Enhancer" {
		t.Fatalf("FriendlyName = %q, want Bass Enhancer", fact.FriendlyName())
	}
	if fact.Weight() != 50 {
		t.Fatalf("Weight = %d, want 50", fact.Weight())
	}
	if fact.Placement() != Post {
		t.Fatalf("Placement = %v, want Post", fact.Placement())
	}
}

func TestBassEnhancerHarmonicEnergy(t *testing.T) {
	rate := 48000
	frames := 4800
	f0 := 60.0

	// Minimum amount (-30 dB) with listen off leaves signal close to input.
	beMin := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount: bassEnhancerMinAmount,
		BassEnhancerListen: false,
	})
	bufMin := bassTestSine(frames, rate, f0, 0.5)
	origMin := interleaveDeep(bufMin)
	if err := beMin.Process(bufMin, frames); err != nil {
		t.Fatalf("Process minimum amount: %v", err)
	}
	nullDB := parityNullDB(origMin, bufMin)
	if nullDB > -20.0 {
		t.Fatalf("minimum amount altered signal: null = %v dB, want < -20 dB", nullDB)
	}

	// Active amount gains harmonic energy at 2x and 3x the fundamental.
	beActive := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount:    10.0,
		BassEnhancerHarmonics: 12.0,
		BassEnhancerScope:     100.0,
		BassEnhancerListen:    false,
	})
	bufActive := bassTestSine(frames, rate, f0, 0.5)
	if err := beActive.Process(bufActive, frames); err != nil {
		t.Fatalf("Process active: %v", err)
	}

	h2Input := goertzel(origMin, 2, 0, rate, 2*f0)
	h3Input := goertzel(origMin, 2, 0, rate, 3*f0)
	h2Out := goertzel(bufActive, 2, 0, rate, 2*f0)
	h3Out := goertzel(bufActive, 2, 0, rate, 3*f0)

	t.Logf("2x: in=%v out=%v; 3x: in=%v out=%v", h2Input, h2Out, h3Input, h3Out)
	if h2Out <= h2Input+0.01 {
		t.Fatalf("2x harmonic did not gain energy: in=%v, out=%v", h2Input, h2Out)
	}
	if h3Out <= h3Input+0.01 {
		t.Fatalf("3x harmonic did not gain energy: in=%v, out=%v", h3Input, h3Out)
	}
}

func TestBassEnhancerListenMutesFundamental(t *testing.T) {
	rate := 48000
	frames := 4800
	f0 := 60.0

	beNormal := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount: 5.0,
		BassEnhancerListen: false,
	})
	bufNormal := bassTestSine(frames, rate, f0, 0.5)
	if err := beNormal.Process(bufNormal, frames); err != nil {
		t.Fatalf("Process normal: %v", err)
	}
	fundNormal := goertzel(bufNormal, 2, 0, rate, f0)

	beListen := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount: 5.0,
		BassEnhancerListen: true,
	})
	bufListen := bassTestSine(frames, rate, f0, 0.5)
	if err := beListen.Process(bufListen, frames); err != nil {
		t.Fatalf("Process listen: %v", err)
	}
	fundListen := goertzel(bufListen, 2, 0, rate, f0)

	t.Logf("fundamental: normal=%v, listen=%v", fundNormal, fundListen)
	if fundListen >= fundNormal*0.5 {
		t.Fatalf("listen mode did not mute fundamental: normal=%v, listen=%v", fundNormal, fundListen)
	}
}

func TestBassEnhancerFloorBandLimits(t *testing.T) {
	rate := 48000
	frames := 4800
	f0 := 30.0

	// Compare floor-active true vs false with floor set above f0.
	beNoFloor := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount:      10.0,
		BassEnhancerScope:       150.0,
		BassEnhancerFloor:       100.0,
		BassEnhancerFloorActive: false,
		BassEnhancerListen:      true,
	})
	bufNoFloor := bassTestSine(frames, rate, f0, 0.5)
	if err := beNoFloor.Process(bufNoFloor, frames); err != nil {
		t.Fatalf("Process no floor: %v", err)
	}
	energyNoFloor := goertzel(bufNoFloor, 2, 0, rate, 2*f0)

	beFloor := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount:      10.0,
		BassEnhancerScope:       150.0,
		BassEnhancerFloor:       100.0,
		BassEnhancerFloorActive: true,
		BassEnhancerListen:      true,
	})
	bufFloor := bassTestSine(frames, rate, f0, 0.5)
	if err := beFloor.Process(bufFloor, frames); err != nil {
		t.Fatalf("Process floor: %v", err)
	}
	energyFloor := goertzel(bufFloor, 2, 0, rate, 2*f0)

	t.Logf("energy at 60 Hz below 100 Hz floor: noFloor=%v, floor=%v", energyNoFloor, energyFloor)
	if energyFloor >= energyNoFloor*0.7 {
		t.Fatalf("floor-active did not band-limit harmonics below floor: noFloor=%v, floor=%v", energyNoFloor, energyFloor)
	}
}

func TestBassEnhancerBypassBitIdentical(t *testing.T) {
	rate := 48000
	frames := 1024
	be := bassTestNewEnhancer(t, rate, Values{
		ParamBypass:        true,
		BassEnhancerAmount: 15.0,
		BassEnhancerListen: true,
	})
	buf := bassTestSine(frames, rate, 60.0, 0.5)
	want := interleaveDeep(buf)

	if err := be.Process(buf, frames); err != nil {
		t.Fatalf("Process bypassed: %v", err)
	}
	for i := range buf {
		if buf[i] != want[i] {
			t.Fatalf("sample %d differed when bypassed: got %v, want %v", i, buf[i], want[i])
		}
	}
}

func TestBassEnhancerProcessDoesNotAllocate(t *testing.T) {
	rate := 48000
	frames := 480
	be := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount:      8.0,
		BassEnhancerHarmonics:   10.0,
		BassEnhancerFloorActive: true,
	})
	buf := bassTestSine(frames, rate, 60.0, 0.5)

	n := testing.AllocsPerRun(100, func() {
		if err := be.Process(buf, frames); err != nil {
			t.Fatalf("Process: %v", err)
		}
	})
	if n != 0 {
		t.Fatalf("Process allocated %v times per run, want 0", n)
	}
}

func TestBassEnhancerResetClearsFilterState(t *testing.T) {
	be := bassTestNewEnhancer(t, 48000, nil)
	buf := bassTestSine(1024, 48000, 60.0, 0.5)
	if err := be.Process(buf, 1024); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := be.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for ch := 0; ch < be.ch; ch++ {
		if be.lpFilter.x1[ch] != 0 || be.lpFilter.y1[ch] != 0 {
			t.Fatalf("Reset did not clear lpFilter state on channel %d", ch)
		}
		if be.hpFilter.x1[ch] != 0 || be.hpFilter.y1[ch] != 0 {
			t.Fatalf("Reset did not clear hpFilter state on channel %d", ch)
		}
	}
}

func TestBassEnhancerOnlyTouchesDeclaredFrames(t *testing.T) {
	be := bassTestNewEnhancer(t, 48000, nil)
	buf := bassTestSine(64, 48000, 60.0, 0.5)
	tail := interleaveDeep(buf[16:])

	if err := be.Process(buf, 8); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range tail {
		if buf[16+i] != tail[i] {
			t.Fatalf("sample past frames altered: got %v, want %v", buf[16+i], tail[i])
		}
	}
}

func TestBassEnhancerSetIsAtomicAndValidated(t *testing.T) {
	be := bassTestNewEnhancer(t, 48000, nil)

	if err := be.Set("unknown-key", 1.0); !errors.Is(err, ErrUnknownParam) {
		t.Fatalf("Set(unknown) = %v, want ErrUnknownParam", err)
	}
	if err := be.Set(BassEnhancerAmount, "not-a-number"); !errors.Is(err, ErrUnknownParam) {
		t.Fatalf("Set(non-numeric) = %v, want ErrUnknownParam", err)
	}

	// Setting above the ceiling clamps to the maximum.
	if err := be.Set(BassEnhancerAmount, 999.0); err != nil {
		t.Fatalf("Set(clamp max): %v", err)
	}
	if got := be.store.Float(BassEnhancerAmount); got != bassEnhancerMaxAmount {
		t.Fatalf("amount clamped = %v, want %v", got, bassEnhancerMaxAmount)
	}

	// Setting below the floor clamps to the minimum.
	if err := be.Set(BassEnhancerAmount, -50.0); err != nil {
		t.Fatalf("Set(clamp min): %v", err)
	}
	if got := be.store.Float(BassEnhancerAmount); got != bassEnhancerMinAmount {
		t.Fatalf("amount clamped = %v, want %v", got, bassEnhancerMinAmount)
	}
}

func TestBassEnhancerConfigureRejectsBadFormat(t *testing.T) {
	e, err := NewBassEnhancerFactory().New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	be := e.(*BassEnhancer)

	if _, err := be.Configure(core.FrameFormat{Rate: 0, Ch: 2, Fmt: core.F32}); err == nil {
		t.Fatal("Configure accepted zero rate")
	}
	if _, err := be.Configure(core.FrameFormat{Rate: 48000, Ch: 0, Fmt: core.F32}); err == nil {
		t.Fatal("Configure accepted zero channel count")
	}
	if _, err := be.Configure(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.S16}); err == nil {
		t.Fatal("Configure accepted non-float32 format")
	}
}

func TestBassEnhancerMonoSupport(t *testing.T) {
	e, err := NewBassEnhancerFactory().New(Values{
		BassEnhancerAmount: 5.0,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	be := e.(*BassEnhancer)
	if _, err := be.Configure(core.FrameFormat{Rate: 48000, Ch: 1, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure mono: %v", err)
	}

	buf := []float32{0.1, 0.2, 0.3, 0.4}
	if err := be.Process(buf, 4); err != nil {
		t.Fatalf("Process mono: %v", err)
	}
	if len(buf) != 4 {
		t.Fatalf("buffer size changed")
	}
}

func TestBassEnhancerSetDuringProcessIsRaceClean(t *testing.T) {
	be := bassTestNewEnhancer(t, 48000, nil)
	buf := bassTestSine(960, 48000, 60.0, 0.5)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for scope := 50.0; ; scope += 10.0 {
			select {
			case <-stop:
				return
			default:
			}
			if scope > bassEnhancerMaxScope {
				scope = 50.0
			}
			if err := be.Set(BassEnhancerScope, scope); err != nil {
				return
			}
			if err := be.Set(BassEnhancerAmount, 5.0+(scope-50)/50); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		if err := be.Process(buf, 480); err != nil {
			t.Fatalf("Process during Set: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestBassEnhancerAmountSmoothness(t *testing.T) {
	rate := 48000
	frames := 4800
	f0 := 60.0

	be0 := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount: 0.0,
		BassEnhancerListen: true,
	})
	buf0 := bassTestSine(frames, rate, f0, 0.5)
	if err := be0.Process(buf0, frames); err != nil {
		t.Fatalf("Process amount 0.0: %v", err)
	}
	e2_0 := goertzel(buf0, 2, 0, rate, 2*f0)

	be1 := bassTestNewEnhancer(t, rate, Values{
		BassEnhancerAmount: 0.1,
		BassEnhancerListen: true,
	})
	buf1 := bassTestSine(frames, rate, f0, 0.5)
	if err := be1.Process(buf1, frames); err != nil {
		t.Fatalf("Process amount 0.1: %v", err)
	}
	e2_1 := goertzel(buf1, 2, 0, rate, 2*f0)

	// A step of 0.1 dB represents a continuous linear factor increase of ~1.16%.
	step := math.Abs(e2_1 - e2_0)
	relStep := step / e2_0
	t.Logf("amount 0.0: %v, amount 0.1: %v, relStep: %v", e2_0, e2_1, relStep)
	if relStep > 0.05 {
		t.Fatalf("amount 0.0 to 0.1 produced discontinuous step: relStep=%v, want <= 0.05", relStep)
	}
}
