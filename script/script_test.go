package script

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/dsp"
)

// The tests print numbers and assert on them; nothing here opens a device. A
// scripted effect is checked the way a built-in one is: run a known signal,
// measure what came out, and state the relationship that has to hold.

const testRate = 48000

// loadEffect loads a testdata script and builds a configured effect.
func loadEffect(t *testing.T, name string, values dsp.Values) *Effect {
	t.Helper()

	f, err := Load(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	e, err := f.New(values)
	if err != nil {
		t.Fatalf("New(%s): %v", name, err)
	}
	ef, ok := e.(*Effect)
	if !ok {
		t.Fatalf("New(%s) returned %T, want *Effect", name, e)
	}
	if _, err := ef.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = ef.Close() })

	return ef
}

// sine fills an interleaved stereo buffer with a tone of amplitude a.
func sine(frames int, freq, a float64) []float32 {
	buf := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		v := float32(a * math.Sin(2*math.Pi*freq*float64(i)/testRate))
		buf[i*2] = v
		buf[i*2+1] = v
	}

	return buf
}

// rms measures one channel of an interleaved stereo buffer.
func rms(buf []float32, ch int) float64 {
	n := len(buf) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(buf[i*2+ch])
		sum += v * v
	}

	return math.Sqrt(sum / float64(n))
}

// peak measures the largest absolute sample across both channels.
func peak(buf []float32) float64 {
	var p float64
	for _, s := range buf {
		if v := math.Abs(float64(s)); v > p {
			p = v
		}
	}

	return p
}

// tone measures one frequency in one channel with a single-bin DFT.
func tone(buf []float32, channel int, freq float64) float64 {
	n := len(buf) / 2
	if n == 0 {
		return 0
	}
	w := 2 * math.Pi * freq / testRate
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for i := 0; i < n; i++ {
		s0 := float64(buf[i*2+channel]) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	if power < 0 {
		power = 0
	}

	return 2 * math.Sqrt(power) / float64(n)
}

// runBlocks processes frames in fixed blocks and returns a snapshot per block
// of the tail of each block, so a test can see a change over time.
func runBlocks(t *testing.T, e *Effect, frames, blockFrames int) [][]float32 {
	t.Helper()

	var blocks [][]float32
	for start := 0; start < frames; start += blockFrames {
		end := min(start+blockFrames, frames)
		buf := sine(end-start, 1000, 0.5)
		if err := e.Process(buf, end-start); err != nil {
			t.Fatalf("Process: %v", err)
		}
		blocks = append(blocks, buf)
	}

	return blocks
}

func TestLoadExampleScripts(t *testing.T) {
	for _, name := range []string{"tremolo.lua", "delay.lua", "lowpass.lua", "compressor.lua"} {
		f, err := Load(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
		if f.Kind() != "script" {
			t.Fatalf("%s Kind = %q, want script", name, f.Kind())
		}
		if f.Impl() == "" {
			t.Fatalf("%s has an empty Impl", name)
		}
		e, err := f.New(nil)
		if err != nil {
			t.Fatalf("New(%s): %v", name, err)
		}
		if _, err := e.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
			t.Fatalf("Configure(%s): %v", name, err)
		}
	}
}

// TestEq20BandsAreInFrequencyOrder pins the graphic EQ's schema: twenty bands
// plus the standard three, and the band sliders in frequency order. The order
// is what the zero-padded keys buy, and it is the one thing the loader has to
// get right for a 20-band layout to read as a spectrum rather than a shuffle.
func TestEq20BandsAreInFrequencyOrder(t *testing.T) {
	f, err := Load(filepath.Join("testdata", "eq20.lua"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	schema := f.Schema()
	if len(schema) != 23 {
		t.Fatalf("schema has %d params, want 23 (3 standard + 20 bands)", len(schema))
	}
	for i := 1; i <= 20; i++ {
		want := fmt.Sprintf("gain%02d", i)
		if got := schema[2+i].Key; got != want {
			t.Fatalf("band %d key = %q, want %q", i, got, want)
		}
	}
	// The band labels are the centre frequencies, and they must ascend.
	prev := 0.0
	for _, p := range schema[3:] {
		var hz float64
		if _, err := fmt.Sscanf(p.Label, "%f Hz", &hz); err != nil {
			t.Fatalf("band label %q is not a frequency: %v", p.Label, err)
		}
		if hz <= prev {
			t.Fatalf("band labels are not ascending: %v after %v", hz, prev)
		}
		prev = hz
	}
	t.Logf("20-band EQ: %d params, bands %s .. %s", len(schema), schema[3].Label, schema[22].Label)
}

// TestParamOrderIsStableAcrossLoads pins the loader's sort. gopher-lua keeps a
// table's string keys in a Go map, whose iteration order changes per run, so
// without the sort a schema's parameter order would differ between loads.
func TestParamOrderIsStableAcrossLoads(t *testing.T) {
	first := ""
	for i := 0; i < 50; i++ {
		f, err := Load(filepath.Join("testdata", "eq20.lua"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got := strings.Join(schemaKeys(f.Schema()), ",")
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("load %d parameter order changed:\n got  %s\n want %s", i, got, first)
		}
	}
	t.Logf("parameter order stable over 50 loads: %s", first)
}

// TestEq20IsTransparentWhenFlat checks the claim the script rests on: a
// peaking biquad at 0 dB is unity, so twenty of them in series pass a tone
// through untouched.
func TestEq20IsTransparentWhenFlat(t *testing.T) {
	e := loadEffect(t, "eq20.lua", nil)
	buf := sine(4800, 1000, 0.5)
	want := append([]float32(nil), buf...)

	if err := e.Process(buf, 4800); err != nil {
		t.Fatalf("Process: %v", err)
	}
	var maxDiff float64
	for i := range buf {
		if d := math.Abs(float64(buf[i] - want[i])); d > maxDiff {
			maxDiff = d
		}
	}
	t.Logf("flat 20-band EQ: max deviation from input %g", maxDiff)
	if maxDiff > 1e-5 {
		t.Fatalf("a flat 20-band EQ is not transparent: max deviation %g", maxDiff)
	}
}

// TestEq20BandMovesItsFrequency checks that one band's gain changes the level
// near its centre and leaves a distant frequency alone.
func TestEq20BandMovesItsFrequency(t *testing.T) {
	const center = 533.6 // band 10
	const far = 3000.0

	measure := func(values dsp.Values, freq float64) float64 {
		e := loadEffect(t, "eq20.lua", values)
		buf := sine(4800, freq, 0.5)
		if err := e.Process(buf, 4800); err != nil {
			t.Fatalf("Process: %v", err)
		}

		return tone(buf, 0, freq)
	}

	flatCenter := measure(nil, center)
	boostCenter := measure(dsp.Values{"gain10": 12.0}, center)
	flatFar := measure(nil, far)
	boostFar := measure(dsp.Values{"gain10": 12.0}, far)

	centerDB := 20 * math.Log10(boostCenter/flatCenter)
	farDB := 20 * math.Log10(boostFar/flatFar)
	t.Logf("band 10 +12 dB: %.1f Hz moved %+.2f dB, %.0f Hz moved %+.2f dB", center, centerDB, far, farDB)

	if centerDB < 9 {
		t.Fatalf("a +12 dB boost moved its own band only %+.2f dB", centerDB)
	}
	if math.Abs(farDB) > 1 {
		t.Fatalf("a +12 dB boost leaked %+.2f dB into a distant frequency", farDB)
	}

	cutCenter := measure(dsp.Values{"gain10": -12.0}, center)
	cutDB := 20 * math.Log10(cutCenter/flatCenter)
	t.Logf("band 10 -12 dB: %.1f Hz moved %+.2f dB", center, cutDB)
	if cutDB > -9 {
		t.Fatalf("a -12 dB cut moved its own band only %+.2f dB", cutDB)
	}
}

func TestSchemaHasStandardAndScriptParams(t *testing.T) {
	f, err := Load(filepath.Join("testdata", "lowpass.lua"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	schema := f.Schema()
	keys := make(map[string]dsp.Param, len(schema))
	for _, p := range schema {
		keys[p.Key] = p
	}

	for _, want := range []string{dsp.ParamBypass, dsp.ParamInputGain, dsp.ParamOutputGain, "freq", "q"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("schema is missing %q; has %v", want, schemaKeys(schema))
		}
	}
	if keys[dsp.ParamBypass].Kind != dsp.Bool {
		t.Fatalf("bypass kind = %v, want Bool", keys[dsp.ParamBypass].Kind)
	}
	if keys[dsp.ParamInputGain].Group != dsp.GroupGain {
		t.Fatalf("input gain group = %q, want %q", keys[dsp.ParamInputGain].Group, dsp.GroupGain)
	}
	if keys["freq"].Unit != "Hz" || keys["freq"].Widget != dsp.WidgetKnob || keys["freq"].Label != "Cutoff" {
		t.Fatalf("freq metadata lost: %+v", keys["freq"])
	}
	if keys["freq"].Default != 1000.0 {
		t.Fatalf("freq default = %v, want 1000", keys["freq"].Default)
	}
	t.Logf("lowpass schema: %v", schemaKeys(schema))
}

func schemaKeys(schema []dsp.Param) []string {
	out := make([]string, len(schema))
	for i, p := range schema {
		out[i] = p.Key
	}

	return out
}

func TestTremoloVariesLevelOverTime(t *testing.T) {
	e := loadEffect(t, "tremolo.lua", dsp.Values{"rate": 4.0, "depth": 1.0})
	blocks := runBlocks(t, e, testRate, 1200)

	minRMS, maxRMS := math.Inf(1), 0.0
	for i, b := range blocks {
		r := rms(b, 0)
		t.Logf("tremolo block %2d rms=%.6f peak=%.6f", i, r, peak(b))
		minRMS = math.Min(minRMS, r)
		maxRMS = math.Max(maxRMS, r)
	}
	if minRMS == maxRMS {
		t.Fatalf("tremolo did not vary the level: rms stayed at %.6f", minRMS)
	}
	if maxRMS == 0 {
		t.Fatal("tremolo produced silence")
	}
	t.Logf("tremolo rms range: %.6f .. %.6f", minRMS, maxRMS)
}

func TestDelayProducesDelayedCopy(t *testing.T) {
	// mix 1.0 means the output is the delayed tap only, so a burst appears in
	// the silence one delay time after the input. That is the cleanest proof
	// the line actually holds audio.
	e := loadEffect(t, "delay.lua", dsp.Values{"time": 0.05, "feedback": 0.0, "mix": 1.0})

	const burst = 2400 // 50 ms of tone
	const tail = 4800  // 100 ms after it
	const delayFrames = 2400
	dry := sine(burst, 1000, 0.5)
	buf := make([]float32, (burst+tail)*2)
	copy(buf, dry)

	inRMS := rms(dry, 0)
	if err := e.Process(buf, burst+tail); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// The first delay length of output is the empty line, then the burst, then
	// silence again once the line has passed it.
	early := rms(buf[(delayFrames+120)*2:(delayFrames+burst-120)*2], 0)
	late := rms(buf[(burst+delayFrames+240)*2:], 0)
	t.Logf("delay: dry rms=%.6f delayed burst rms=%.6f after tail rms=%.6f", inRMS, early, late)

	if inRMS < 0.3 {
		t.Fatalf("input burst is too quiet: %.6f", inRMS)
	}
	if early < 0.1 {
		t.Fatalf("delayed copy did not appear: rms %.6f", early)
	}
	if late > early*0.25 {
		t.Fatalf("delayed copy did not end: late %.6f vs early %.6f", late, early)
	}
}

func TestDelayBlockTrace(t *testing.T) {
	// A per-block trace of the delayed output, so a run with -v shows the line
	// filling and emptying rather than only the aggregate assertion.
	e := loadEffect(t, "delay.lua", dsp.Values{"time": 0.05, "feedback": 0.3, "mix": 0.5})
	blocks := runBlocks(t, e, testRate/4, 1200)
	for i, b := range blocks {
		t.Logf("delay block %2d rms=%.6f peak=%.6f", i, rms(b, 0), peak(b))
	}
	if len(blocks) < 10 {
		t.Fatalf("only %d blocks ran", len(blocks))
	}
}

func TestDelayFeedbackIsStable(t *testing.T) {
	// A 0.5 tone through a 0.9-feedback delay has a sustained tail, so the
	// output is not just the dry tone: the loop adds to it. The assertion is
	// that the added energy stays finite and bounded, which was vacuous when
	// the delay ignored its wired input and produced only the dry tone.
	e := loadEffect(t, "delay.lua", dsp.Values{"time": 0.02, "feedback": 0.9, "mix": 0.5})
	buf := sine(testRate, 1000, 0.5)

	if err := e.Process(buf, testRate); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i, s := range buf {
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
			t.Fatalf("sample %d is not finite: %v", i, s)
		}
	}
	// The dry tone alone would give an rms near 0.5; feedback raises it.
	outRMS := rms(buf, 0)
	t.Logf("delay feedback 0.9 peak=%.6f rms=%.6f (dry rms=%.6f)", peak(buf), outRMS, rms(sine(testRate, 1000, 0.5), 0))
	if peak(buf) > 8 {
		t.Fatalf("feedback loop grew unbounded: peak %.6f", peak(buf))
	}
	if outRMS <= 0.5 {
		t.Fatalf("feedback did not add energy to the tone: rms %.6f", outRMS)
	}
}

func TestLowpassAttenuatesHighMoreThanLow(t *testing.T) {
	// A cutoff of 500 Hz: a 200 Hz tone passes, an 8 kHz tone is cut.
	low := loadEffect(t, "lowpass.lua", dsp.Values{"freq": 500.0, "q": 0.707})
	high := loadEffect(t, "lowpass.lua", dsp.Values{"freq": 500.0, "q": 0.707})

	lowBuf := sine(testRate/2, 200, 0.5)
	highBuf := sine(testRate/2, 8000, 0.5)
	if err := low.Process(lowBuf, testRate/2); err != nil {
		t.Fatalf("Process low: %v", err)
	}
	if err := high.Process(highBuf, testRate/2); err != nil {
		t.Fatalf("Process high: %v", err)
	}

	lowOut := tone(lowBuf[testRate/4*2:], 0, 200)
	highOut := tone(highBuf[testRate/4*2:], 0, 8000)
	t.Logf("lowpass 500 Hz: 200 Hz out=%.6f, 8000 Hz out=%.6f", lowOut, highOut)

	if !(lowOut > 0.35) {
		t.Fatalf("low tone was attenuated too much: %.6f", lowOut)
	}
	if !(highOut < lowOut*0.2) {
		t.Fatalf("high tone was not attenuated relative to low: %.6f vs %.6f", highOut, lowOut)
	}
}

func TestLowpassBlockTrace(t *testing.T) {
	// The per-block trace for a filtered 1 kHz tone, so a -v run shows the
	// output settling as the biquad's memory fills.
	e := loadEffect(t, "lowpass.lua", dsp.Values{"freq": 1000.0, "q": 0.707})
	blocks := runBlocks(t, e, 48000, 1200)
	for i, b := range blocks {
		t.Logf("lowpass block %2d rms=%.6f peak=%.6f tone1k=%.6f", i, rms(b, 0), peak(b), tone(b, 0, 1000))
	}
	if len(blocks) < 10 {
		t.Fatalf("only %d blocks ran", len(blocks))
	}
}

func TestProcessAllocatesNothing(t *testing.T) {
	for _, name := range []string{"tremolo.lua", "delay.lua", "lowpass.lua", "compressor.lua"} {
		e := loadEffect(t, name, nil)
		buf := sine(960, 1000, 0.5)
		n := testing.AllocsPerRun(1000, func() {
			if err := e.Process(buf, 480); err != nil {
				t.Fatalf("Process: %v", err)
			}
		})
		t.Logf("%s Process allocations per run: %v", name, n)
		if n != 0 {
			t.Fatalf("%s Process allocated %v times per run, want 0", name, n)
		}
	}
}

func TestBypassPassesAudioUnchanged(t *testing.T) {
	e := loadEffect(t, "lowpass.lua", dsp.Values{dsp.ParamBypass: true})
	buf := sine(4096, 1000, 0.5)
	want := append([]float32(nil), buf...)

	if err := e.Process(buf, 2048); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range buf {
		if buf[i] != want[i] {
			t.Fatalf("bypassed sample %d changed: %v vs %v", i, buf[i], want[i])
		}
	}
}

func TestMetersReportLevels(t *testing.T) {
	e := loadEffect(t, "tremolo.lua", dsp.Values{"depth": 0.0})
	buf := sine(4800, 1000, 0.5)
	if err := e.Process(buf, 4800); err != nil {
		t.Fatalf("Process: %v", err)
	}
	m := e.Meters()
	in, ok := m[dsp.MeterIn]
	if !ok {
		t.Fatalf("Meters has no %q key: %v", dsp.MeterIn, m)
	}
	out := m[dsp.MeterOut]
	t.Logf("meters: in=%.2f dBFS out=%.2f dBFS", in, out)
	if !(in < 0) {
		t.Fatalf("input meter reads %.2f dBFS for a 0.5 tone, want negative", in)
	}
}

func TestStandardGainsApply(t *testing.T) {
	// +6.02 dB in and -6.02 dB out must cancel, so the effect's own output is
	// unchanged; the point is that both trims are live.
	base := loadEffect(t, "lowpass.lua", dsp.Values{"freq": 2000.0})
	gained := loadEffect(t, "lowpass.lua", dsp.Values{
		"freq":              2000.0,
		dsp.ParamInputGain:  6.0206,
		dsp.ParamOutputGain: -6.0206,
	})
	a := sine(4800, 1000, 0.5)
	b := append([]float32(nil), a...)
	if err := base.Process(a, 4800); err != nil {
		t.Fatalf("Process base: %v", err)
	}
	if err := gained.Process(b, 4800); err != nil {
		t.Fatalf("Process gained: %v", err)
	}
	for i := range a {
		if d := math.Abs(float64(a[i] - b[i])); d > 1e-5 {
			t.Fatalf("sample %d: unity %v, gained %v", i, a[i], b[i])
		}
	}
}

func TestSetValidatesAndClamps(t *testing.T) {
	e := loadEffect(t, "lowpass.lua", nil)

	if err := e.Set("nope", 1.0); !errors.Is(err, dsp.ErrUnknownParam) {
		t.Fatalf("Set(unknown) = %v, want ErrUnknownParam", err)
	}
	if err := e.Set("freq", 999999.0); err != nil {
		t.Fatalf("Set(clamped): %v", err)
	}
	if got, _ := e.Get("freq"); got != 20000.0 {
		t.Fatalf("clamped freq = %v, want 20000", got)
	}
	if err := e.Set("freq", "loud"); !errors.Is(err, dsp.ErrUnknownParam) {
		t.Fatalf("Set(wrong type) = %v, want ErrUnknownParam", err)
	}
}

func TestSetPreservesNodeState(t *testing.T) {
	// Moving a slider must not reset a delay line. Feed a burst, then change the
	// time while the line still holds it; the tail must survive the change.
	e := loadEffect(t, "delay.lua", dsp.Values{"time": 0.5, "feedback": 0.6, "mix": 1.0})

	burst := sine(2400, 1000, 0.5)
	if err := e.Process(burst, 2400); err != nil {
		t.Fatalf("Process burst: %v", err)
	}
	before := e.state.Load().plan

	if err := e.Set("time", 0.05); err != nil {
		t.Fatalf("Set: %v", err)
	}
	after := e.state.Load().plan
	if before != after {
		t.Fatal("Set recompiled the plan; node state would be lost")
	}

	// The feedback loop keeps ringing after the burst, so the follow-on is not
	// silent unless Set wiped the line.
	follow := make([]float32, 24000*2)
	if err := e.Process(follow, 24000); err != nil {
		t.Fatalf("Process follow: %v", err)
	}
	got := rms(follow, 0)
	t.Logf("after Set time=0.05, follow-on rms=%.6f", got)
	if got == 0 {
		t.Fatal("Set wiped the delay line")
	}
}

func TestSetDoesNotResetFilterMemory(t *testing.T) {
	// The biquad's memory must survive a cutoff move, which is what keeps a
	// slider from clicking.
	e := loadEffect(t, "lowpass.lua", dsp.Values{"freq": 800.0})
	buf := sine(24000, 1000, 0.5)
	if err := e.Process(buf, 24000); err != nil {
		t.Fatalf("Process: %v", err)
	}
	magnitude := tone(buf[12000*2:], 0, 1000)
	if err := e.Set("freq", 4000.0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	follow := sine(4096, 1000, 0.5)
	if err := e.Process(follow, 4096); err != nil {
		t.Fatalf("Process: %v", err)
	}
	t.Logf("before Set 1 kHz through 800 Hz = %.6f, after Set through 4 kHz = %.6f", magnitude, tone(follow, 0, 1000))
	if magnitude < 0.1 {
		t.Fatalf("filter did not pass the tone before Set: %.6f", magnitude)
	}
}

func TestFactoryRegistersAndBuilds(t *testing.T) {
	f, err := Load(filepath.Join("testdata", "tremolo.lua"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	reg := dsp.NewRegistry()
	reg.Register(f)

	// The registered pair resolves by kind and by impl, which is how a preset
	// names a scripted effect.
	if _, err := reg.New("script", "", nil); err != nil {
		t.Fatalf("New by kind: %v", err)
	}
	if _, err := reg.New("script", f.Impl(), nil); err != nil {
		t.Fatalf("New by impl: %v", err)
	}
	schema, err := reg.Schema("script")
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if len(schema) != 5 {
		t.Fatalf("schema has %d params, want 5 (3 standard + rate + depth)", len(schema))
	}
}

func TestLoadDir(t *testing.T) {
	fs, err := LoadDir("testdata")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(fs) < 3 {
		t.Fatalf("LoadDir returned %d factories, want at least 3", len(fs))
	}
	for _, f := range fs {
		if f.Kind() != "script" {
			t.Fatalf("%s has kind %q", f.Impl(), f.Kind())
		}
	}
}

// writeScript writes a temporary Lua file for a test.
func writeScript(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.lua")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	return path
}

func TestSandboxRejectsHostLibraries(t *testing.T) {
	cases := map[string]string{
		"os.execute": `os.execute("true") return effect{name="x", build=function(ctx) ctx.out(ctx.input()) end}`,
		"io.open":    `io.open("/etc/passwd") return effect{name="x", build=function(ctx) ctx.out(ctx.input()) end}`,
		"require":    `local m = require("os") return effect{name="x", build=function(ctx) ctx.out(ctx.input()) end}`,
		"dofile":     `dofile("/etc/passwd") return effect{name="x", build=function(ctx) ctx.out(ctx.input()) end}`,
	}
	for name, body := range cases {
		path := writeScript(t, body)
		f, err := Load(path)
		if err == nil {
			t.Fatalf("%s did not fail to load; got factory %v", name, f)
		}
		t.Logf("%s rejected: %v", name, err)
	}
}

func TestCombinatorialCycleRejected(t *testing.T) {
	// An add whose output feeds itself through a multiply, with no delay in the
	// loop, has no run-loop fixed point and must fail to compile.
	path := writeScript(t, `return effect {
  name = "Cycle",
  build = function(ctx)
    local a = ctx.add(ctx.input())
    local m = ctx.scale(a, 0.5)
    a.b = m
    ctx.out(a)
  end,
}`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := f.New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err == nil {
		t.Fatal("Configure accepted a combinatorial cycle")
	} else {
		t.Logf("cycle rejected: %v", err)
	}
}

func TestFeedbackThroughDelayAccepted(t *testing.T) {
	// The same shape, but the loop reads a delay's tap, which carries one
	// sample of memory and makes the loop legal.
	path := writeScript(t, `return effect {
  name = "Feedback",
  params = { fb = { float, min = 0, max = 0.9, default = 0.8 } },
  build = function(ctx)
    local d = ctx.delay { max = 1.0, time = 0.01 }
    d.input = ctx.input() + d.tap * ctx.param("fb")
    ctx.out(d.tap)
  end,
}`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := f.New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ef := e.(*Effect)
	if _, err := ef.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { _ = ef.Close() })
	buf := sine(testRate/2, 1000, 0.5)
	if err := ef.Process(buf, testRate/2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i, s := range buf {
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
			t.Fatalf("sample %d not finite: %v", i, s)
		}
	}
	t.Logf("delay feedback 0.8 stable: peak=%.6f rms=%.6f", peak(buf), rms(buf, 0))
	if peak(buf) > 8 {
		t.Fatalf("feedback loop ran away: peak %.6f", peak(buf))
	}
}

func TestWatchdogBypassesSlowControl(t *testing.T) {
	// The callback spins for well over the budget, so the watchdog must pull
	// the effect and record why. The audio path then passes the input through.
	path := writeScript(t, `return effect {
  name = "Slow",
  params = { freq = { float, min = 20, max = 20000, default = 1000 } },
  build = function(ctx)
    local lp = ctx.biquad { type = "lowpass", freq = ctx.param("freq") }
    ctx.control(function(p)
      local t = 0
      for i = 1, 2000000 do t = t + i end
      lp:set { freq = p.freq, q = 0.707 }
    end)
    ctx.out(lp(ctx.input()))
  end,
}`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := f.New(dsp.Values{"freq": 8000.0})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ef := e.(*Effect)
	if _, err := ef.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { _ = ef.Close() })

	// Give the control goroutine time to run and trip the watchdog. The loop is
	// deliberately over the budget but small enough to finish; the deadline is
	// generous so a race-instrumented run still gets there.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && ef.Watchdog() == "" {
		time.Sleep(10 * time.Millisecond)
	}
	if ef.Watchdog() == "" {
		t.Fatal("watchdog never tripped on a slow callback")
	}
	t.Logf("watchdog tripped: %s", ef.Watchdog())

	if !ef.Bypassed() {
		t.Fatal("effect is not bypassed after a watchdog fault")
	}
	// Bypassed means unchanged audio, and Process must still be allocation-free
	// and never call back into Lua.
	buf := sine(960, 1000, 0.5)
	want := append([]float32(nil), buf...)
	if err := ef.Process(buf, 480); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range buf {
		if buf[i] != want[i] {
			t.Fatalf("sample %d changed while watch-dogged: %v vs %v", i, buf[i], want[i])
		}
	}
}

func TestTier2ControlUpdatesCoefficients(t *testing.T) {
	// The callback runs on its own goroutine and calls node:set, which publishes
	// new coefficients for the audio thread to pick up. The assertion is that
	// the effect's frequency response actually moved.
	path := writeScript(t, `return effect {
  name = "Scan",
  params = { freq = { float, min = 20, max = 20000, default = 1000 } },
  build = function(ctx)
    local lp = ctx.biquad { type = "lowpass", freq = ctx.param("freq") }
    ctx.control(function(p)
      lp:set { freq = p.freq, q = 0.707 }
    end)
    ctx.out(lp(ctx.input()))
  end,
}`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	measureAt := func(freq float64) float64 {
		e, err := f.New(dsp.Values{"freq": freq})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ef := e.(*Effect)
		if _, err := ef.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
			t.Fatalf("Configure: %v", err)
		}
		t.Cleanup(func() { _ = ef.Close() })
		// Wait for one control tick so the callback publishes the coefficient.
		time.Sleep(150 * time.Millisecond)

		buf := sine(testRate/2, 2000, 0.5)
		if err := ef.Process(buf, testRate/2); err != nil {
			t.Fatalf("Process: %v", err)
		}

		return tone(buf[testRate/4*2:], 0, 2000)
	}

	open := measureAt(12000)
	closed := measureAt(300)
	t.Logf("tier 2 control: 2 kHz through 12 kHz lowpass=%.6f, through 300 Hz lowpass=%.6f", open, closed)
	if !(open > closed*2) {
		t.Fatalf("control callback did not move the filter: open %.6f, closed %.6f", open, closed)
	}
}

func TestUnknownParamReferenceFailsLoad(t *testing.T) {
	path := writeScript(t, `return effect {
  name = "Bad",
  params = { freq = { float, min = 20, max = 20000, default = 1000 } },
  build = function(ctx)
    ctx.out(ctx.input() * ctx.param("nope"))
  end,
}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a build that references an undeclared parameter")
	} else {
		t.Logf("undeclared reference rejected: %v", err)
	}
}

func TestMissingOutputFailsLoad(t *testing.T) {
	path := writeScript(t, `return effect {
  name = "NoOut",
  build = function(ctx) end,
}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a build that never calls ctx.out")
	} else {
		t.Logf("missing ctx.out rejected: %v", err)
	}
}

func TestScriptErrorIsNotAPanic(t *testing.T) {
	path := writeScript(t, `error("boom")`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a script that errors")
	} else {
		t.Logf("runtime error returned as error: %v", err)
	}
}

func TestUnknownNodeFieldFailsLoad(t *testing.T) {
	path := writeScript(t, `return effect {
  name = "Typo",
  build = function(ctx)
    local lp = ctx.biquad { type = "lowpass", freqeuncy = 1000 }
    ctx.out(lp(ctx.input()))
  end,
}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a misspelled node field")
	} else {
		t.Logf("unknown field rejected: %v", err)
	}
}

func TestSetDuringProcessIsRaceClean(t *testing.T) {
	// Process and Set run on different goroutines in production. The race
	// detector is the assertion: every field Set republishes must be disjoint
	// from what Process reads, and the output must stay finite.
	e := loadEffect(t, "lowpass.lua", dsp.Values{"freq": 1000.0, "q": 0.707})
	buf := sine(960, 1000, 0.5)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for cutoff := 200.0; ; cutoff += 50 {
			select {
			case <-stop:
				return
			default:
			}
			if cutoff > 18000 {
				cutoff = 200
			}
			if err := e.Set("freq", cutoff); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 2000; i++ {
		if err := e.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	for i, s := range buf {
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
			t.Fatalf("sample %d = %v after concurrent Set", i, s)
		}
	}
}

func TestTier2SetDuringProcessIsRaceClean(t *testing.T) {
	// The same, but with a control goroutine also running: three sides touch
	// the effect and only the published snapshot may be shared.
	path := writeScript(t, `return effect {
  name = "Scan",
  params = { freq = { float, min = 20, max = 20000, default = 1000 } },
  build = function(ctx)
    local lp = ctx.biquad { type = "lowpass", freq = ctx.param("freq") }
    ctx.control(function(p) lp:set { freq = p.freq, q = 0.707 } end)
    ctx.out(lp(ctx.input()))
  end,
}`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := f.New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ef := e.(*Effect)
	if _, err := ef.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { _ = ef.Close() })

	buf := sine(960, 1000, 0.5)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for cutoff := 200.0; ; cutoff += 500 {
			select {
			case <-stop:
				return
			default:
			}
			if cutoff > 18000 {
				cutoff = 200
			}
			if err := ef.Set("freq", cutoff); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		if err := ef.Process(buf, 480); err != nil {
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

func TestEnumParamMapsOptions(t *testing.T) {
	path := writeScript(t, `return effect {
  name = "Modes",
  params = {
    mode = { enum, options = { "peak", "rms" }, default = "rms", label = "Mode" },
  },
  build = function(ctx)
    local e = ctx.env { attack = 0.01, release = 0.1, mode = "rms", input = ctx.input() }
    ctx.out(ctx.input() * e)
  end,
}`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var mode dsp.Param
	for _, p := range f.Schema() {
		if p.Key == "mode" {
			mode = p
		}
	}
	if mode.Kind != dsp.Enum {
		t.Fatalf("mode kind = %v, want Enum", mode.Kind)
	}
	if mode.Widget != dsp.WidgetSelect {
		t.Fatalf("mode widget = %q, want select", mode.Widget)
	}
	if mode.Default != "rms" {
		t.Fatalf("mode default = %v, want rms", mode.Default)
	}
	e, err := f.New(dsp.Values{"mode": "peak"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if _, err := f.New(dsp.Values{"mode": "bogus"}); err == nil {
		t.Fatal("New accepted an enum value outside its options")
	}
	t.Logf("enum param: %+v", mode)
}

// TestSchemaMatchesDSPStandardParams is the coupling test for the one place a
// scripted effect must be indistinguishable from a built-in: the three standard
// parameters. buildSchema prepends dsp.StandardParams, and this pins the two
// together field by field, so a change to the standard shape cannot silently
// diverge from what a scripted effect emits.
func TestSchemaMatchesDSPStandardParams(t *testing.T) {
	f, err := Load(filepath.Join("testdata", "tremolo.lua"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	schema := f.Schema()
	standard := dsp.StandardParams()
	if len(schema) < len(standard) {
		t.Fatalf("schema has %d params, fewer than the %d standard ones", len(schema), len(standard))
	}

	for i, want := range standard {
		got := schema[i]
		if !paramsEqual(got, want) {
			t.Errorf("standard param %d:\n got  %+v\n want %+v", i, got, want)
		}
		if !dsp.CommonParam(got.Key) {
			t.Errorf("dsp.CommonParam(%q) = false, but the schema emits it", got.Key)
		}
	}
}

// paramsEqual compares two parameters field by field. Param carries an Options
// slice, so it is not comparable with ==.
func paramsEqual(a, b dsp.Param) bool {
	if a.Key != b.Key || a.Kind != b.Kind || a.Min != b.Min || a.Max != b.Max ||
		a.Step != b.Step || a.Unit != b.Unit || a.Default != b.Default ||
		a.Label != b.Label || a.Group != b.Group || a.Widget != b.Widget {
		return false
	}
	if len(a.Options) != len(b.Options) {
		return false
	}
	for i := range a.Options {
		if a.Options[i] != b.Options[i] {
			return false
		}
	}

	return true
}
