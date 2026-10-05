package dsp

import (
	"math"
	"sync"
	"testing"

	"github.com/dlcuy22/molo/core"
)

func eqTestMakeEqualizer(t *testing.T, values Values, rate, ch int) Effect {
	t.Helper()
	fac := NewEqualizerFactory()
	eq, err := fac.New(values)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	format := core.FrameFormat{Rate: rate, Ch: ch, Fmt: core.F32}
	if _, err := eq.Configure(format); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	return eq
}

func TestEqualizerAllGainsZeroTransparent(t *testing.T) {
	rate := 48000
	ch := 2
	eq := eqTestMakeEqualizer(t, nil, rate, ch)

	frames := 4800
	samples := sineStereo(frames, rate, 1000)
	ref := interleaveDeep(samples)

	renderEffectChunks(t, eq, samples, ch, 512)

	nullDB := parityNullDB(ref, samples)
	if !math.IsInf(nullDB, -1) && nullDB >= -100 {
		t.Fatalf("expected transparent EQ (null < -100 dB), got %v dB", nullDB)
	}
}

func TestEqualizerSingleBandBoost(t *testing.T) {
	rate := 48000
	ch := 2
	eq := eqTestMakeEqualizer(t, nil, rate, ch)

	if err := eq.Set("band5-gain", 6.0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	freq := 1000.0
	frames := 9600
	samples := sineStereo(frames, rate, freq)

	inMag := goertzel(samples, ch, 0, rate, freq)

	renderEffectChunks(t, eq, samples, ch, 512)

	// Steady state after transient settles in the first 2000 frames.
	steady := samples[2000*ch:]
	outMag := goertzel(steady, ch, 0, rate, freq)

	gainDB := 20 * math.Log10(outMag/inMag)
	if math.Abs(gainDB-6.0) > 0.5 {
		t.Fatalf("expected ~6 dB boost at 1000 Hz, got %.2f dB (in=%.4f out=%.4f)", gainDB, inMag, outMag)
	}
}

func TestEqualizerPresetNonClipping(t *testing.T) {
	rate := 48000
	ch := 2

	presetValues := Values{
		EqualizerNumBands: 10,
		"band0-gain":      4.0,
		"band1-gain":      2.0,
		"band2-gain":      1.0,
		"band3-gain":      0.0,
		"band4-gain":      -1.0,
		"band5-gain":      -2.0,
		"band6-gain":      0.0,
		"band7-gain":      2.0,
		"band8-gain":      3.0,
		"band9-gain":      3.0,
	}

	eq := eqTestMakeEqualizer(t, presetValues, rate, ch)

	frames := 9600
	samples := make([]float32, frames*ch)
	freqs := []float64{32, 64, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}
	scale := 0.25 / float64(len(freqs))

	for i := 0; i < frames; i++ {
		var mix float64
		for _, f := range freqs {
			mix += math.Sin(2 * math.Pi * f * float64(i) / float64(rate))
		}
		v := float32(mix * scale)
		samples[2*i] = v
		samples[2*i+1] = v
	}

	renderEffectChunks(t, eq, samples, ch, 512)

	var maxPeak float32
	for i, s := range samples {
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
			t.Fatalf("sample %d is non-finite: %v", i, s)
		}
		abs := float32(math.Abs(float64(s)))
		if abs > maxPeak {
			maxPeak = abs
		}
	}

	if maxPeak > 1.0 {
		t.Fatalf("preset output exceeded full scale: peak=%v", maxPeak)
	}
}

func TestEqualizerBandGainChangePreservesOtherState(t *testing.T) {
	rate := 48000
	ch := 2
	raw := eqTestMakeEqualizer(t, nil, rate, ch)
	eq := raw.(*Equalizer)

	samples := sineStereo(512, rate, 1000)
	if err := eq.Process(samples, 512); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Capture delay history of untouched band 0 and target band 5.
	b0x1 := eq.bands[0].x1[0]
	b0y1 := eq.bands[0].y1[0]
	b5x1 := eq.bands[5].x1[0]
	b5y1 := eq.bands[5].y1[0]

	if b0x1 == 0 || b0y1 == 0 || b5x1 == 0 || b5y1 == 0 {
		t.Fatal("expected non-zero filter memory after processing audio")
	}

	if err := eq.Set("band5-gain", 4.0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if eq.bands[0].x1[0] != b0x1 || eq.bands[0].y1[0] != b0y1 {
		t.Fatal("Set on band 5 corrupted band 0 filter memory")
	}
	if eq.bands[5].x1[0] != b5x1 || eq.bands[5].y1[0] != b5y1 {
		t.Fatal("Set on band 5 reset its own filter memory")
	}

	// Step a continuous tone across parameter update to verify no waveform jump.
	chunk1 := sineStereo(256, rate, 1000)
	if err := eq.Process(chunk1, 256); err != nil {
		t.Fatalf("Process: %v", err)
	}
	lastSample := chunk1[len(chunk1)-2]

	if err := eq.Set("band5-gain", 2.0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	chunk2 := sineStereo(256, rate, 1000)
	if err := eq.Process(chunk2, 256); err != nil {
		t.Fatalf("Process: %v", err)
	}
	firstSample := chunk2[0]

	if diff := math.Abs(float64(firstSample - lastSample)); diff > 0.5 {
		t.Fatalf("discontinuity at chunk seam: jump=%v", diff)
	}
}

func TestEqualizerProcessZeroAllocs(t *testing.T) {
	rate := 48000
	ch := 2
	eq := eqTestMakeEqualizer(t, nil, rate, ch)

	buf := sineStereo(480, rate, 1000)

	allocs := testing.AllocsPerRun(100, func() {
		if err := eq.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("Process allocated %v times per run, want 0", allocs)
	}
}

func TestEqualizerBypassBitIdentical(t *testing.T) {
	rate := 48000
	ch := 2
	eq := eqTestMakeEqualizer(t, Values{
		ParamBypass:  true,
		"band5-gain": 6.0,
	}, rate, ch)

	samples := sineStereo(4800, rate, 1000)
	ref := interleaveDeep(samples)

	renderEffectChunks(t, eq, samples, ch, 512)

	nullDB := parityNullDB(ref, samples)
	if !math.IsInf(nullDB, -1) {
		t.Fatalf("expected bit-identical bypass (null -Inf), got %v dB", nullDB)
	}
}

func TestEqualizerSetDuringProcessIsRaceClean(t *testing.T) {
	rate := 48000
	ch := 2
	eq := eqTestMakeEqualizer(t, nil, rate, ch)
	buf := sineStereo(480, rate, 1000)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		gain := -12.0
		for {
			select {
			case <-stop:
				return
			default:
			}
			gain += 1.0
			if gain > 12.0 {
				gain = -12.0
			}
			if err := eq.Set("band5-gain", gain); err != nil {
				return
			}
			if err := eq.Set("band2-frequency", 125.0+gain*2.0); err != nil {
				return
			}
			if err := eq.Set(EqualizerNumBands, 5+int(math.Abs(gain))%5); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 1000; i++ {
		if err := eq.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	for i, s := range buf {
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
			t.Fatalf("sample %d is non-finite: %v", i, s)
		}
	}
}

func TestEqualizerFactoryAndSchema(t *testing.T) {
	fac := NewEqualizerFactory()
	if fac.Kind() != "equalizer" {
		t.Fatalf("unexpected Kind: %s", fac.Kind())
	}
	if fac.Impl() != "equalizer-rbj" {
		t.Fatalf("unexpected Impl: %s", fac.Impl())
	}
	if fac.FriendlyName() != "Equalizer" {
		t.Fatalf("unexpected FriendlyName: %s", fac.FriendlyName())
	}
	if fac.Weight() != 50 {
		t.Fatalf("unexpected Weight: %d", fac.Weight())
	}
	if fac.Placement() != Post {
		t.Fatalf("unexpected Placement: %v", fac.Placement())
	}

	schema := fac.Schema()
	if len(schema) != 34 {
		t.Fatalf("expected 34 schema params, got %d", len(schema))
	}

	var numBandsParam *Param
	for i := range schema {
		if schema[i].Key == EqualizerNumBands {
			numBandsParam = &schema[i]
			break
		}
	}
	if numBandsParam == nil || numBandsParam.Min != 1 || numBandsParam.Max != 10 {
		t.Fatalf("expected num-bands range 1..10, got %+v", numBandsParam)
	}

	eq, err := fac.New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if eq.Name() != "equalizer" {
		t.Fatalf("unexpected Name: %s", eq.Name())
	}
}

func BenchmarkEqualizerProcess(b *testing.B) {
	fac := NewEqualizerFactory()
	eq, err := fac.New(nil)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	format := core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}
	if _, err := eq.Configure(format); err != nil {
		b.Fatalf("Configure: %v", err)
	}

	buf := sineStereo(512, 48000, 1000)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = eq.Process(buf, 512)
	}
}
