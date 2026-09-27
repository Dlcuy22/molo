package script

// Regression tests for the adversarial review of the script package. Each test
// pins one finding with a printed number, so a future refactor that undoes a
// fix fails here rather than in the field.

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/dsp"
)

// buildConfigured loads a script body and configures the effect.
func buildConfigured(t *testing.T, body string, values dsp.Values) *Effect {
	t.Helper()

	path := writeScript(t, body)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := f.New(values)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ef := e.(*Effect)
	if _, err := ef.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { _ = ef.Close() })

	return ef
}

// B1: the delay must read the input it was wired, so a feedback path is live.

func TestDelayReadsItsWiredInput(t *testing.T) {
	// The source multiplies the input by zero, so a delay that reads its wired
	// input emits silence. Before the fix it read the effect's dry frame and
	// emitted the 0.5 DC behind the tap.
	e := buildConfigured(t, `return effect {
  name = "WiredDelay",
  build = function(ctx)
    local d = ctx.delay { max = 0.1, time = 0.01 }
    d.input = ctx.input() * ctx.const(0.0)
    ctx.out(d.tap)
  end,
}`, nil)

	buf := make([]float32, 4800*2)
	for i := range buf {
		buf[i] = 0.5
	}
	if err := e.Process(buf, 4800); err != nil {
		t.Fatalf("Process: %v", err)
	}
	t.Logf("delay tap with a zero-wired input: peak=%.6f (want 0)", peak(buf))
	if math.Abs(peak(buf)) > 1e-6 {
		t.Fatalf("delay ignored its wired input: tap peak %.6f", peak(buf))
	}
}

func TestDelayFeedbackFeedsBack(t *testing.T) {
	// A feedback loop holds the input in the line past the burst. With the wire
	// ignored there is no feedback and the tail is silent.
	e := buildConfigured(t, `return effect {
  name = "FeedbackWire",
  build = function(ctx)
    local d = ctx.delay { max = 0.05, time = 0.005 }
    d.input = ctx.input() + d.tap * ctx.const(0.7)
    ctx.out(d.tap)
  end,
}`, nil)

	burst := sine(240, 1000, 0.5)
	buf := make([]float32, (240+2400)*2)
	copy(buf, burst)
	if err := e.Process(buf, 240+2400); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// After the burst and several delay periods the line should still ring.
	tail := rms(buf[1600*2:], 0)
	t.Logf("feedback tail rms after the burst: %.6f (want > 0)", tail)
	if tail < 1e-4 {
		t.Fatalf("feedback did not feed back: tail rms %.6f", tail)
	}
}

// B2: `/` divides.

func TestDivisionDivides(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Div",
  build = function(ctx) ctx.out(ctx.const(8) / ctx.const(2)) end,
}`, nil)
	buf := make([]float32, 8)
	if err := e.Process(buf, 4); err != nil {
		t.Fatalf("Process: %v", err)
	}
	t.Logf("8 / 2 = %v (want 4)", buf[0])
	if math.Abs(float64(buf[0])-4) > 1e-6 {
		t.Fatalf("8 / 2 = %v, want 4", buf[0])
	}
}

func TestDivisionByZeroIsZero(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "DivZero",
  build = function(ctx) ctx.out(ctx.const(1) / ctx.const(0)) end,
}`, nil)
	buf := make([]float32, 8)
	if err := e.Process(buf, 4); err != nil {
		t.Fatalf("Process: %v", err)
	}
	t.Logf("1 / 0 = %v (want 0, never Inf)", buf[0])
	if buf[0] != 0 {
		t.Fatalf("1 / 0 = %v, want 0", buf[0])
	}
}

// B3: ctx.db and ctx.db2lin build and convert.

func TestDbHelpersBuildAndConvert(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Db",
  params = { threshold = { float, min = -60, max = 0, default = -12 }, ratio = { float, min = 1, max = 20, default = 4 } },
  build = function(ctx)
    local env  = ctx.env { attack = 0.01, release = 0.1, mode = "rms" }
    env.input = ctx.input()
    local db   = ctx.db(env)
    local over = ctx.max(db - ctx.param("threshold"), ctx.const(0))
    local gr   = ctx.db2lin(-over * (1 - 1 / ctx.param("ratio")))
    ctx.out(ctx.input() * gr)
  end,
}`, nil)

	buf := sine(4800, 1000, 0.5)
	if err := e.Process(buf, 4800); err != nil {
		t.Fatalf("Process: %v", err)
	}
	out := rms(buf, 0)
	t.Logf("compressor idiom output rms=%.6f (input rms=%.6f)", out, rms(sine(4800, 1000, 0.5), 0))
	if out == 0 {
		t.Fatal("the compressor idiom produced silence")
	}
}

// B4: Reset must not race Process. The race detector is the assertion; the
// value check only proves the run was real.

func TestResetDuringProcessIsRaceClean(t *testing.T) {
	e := loadEffect(t, "delay.lua", dsp.Values{"time": 0.05, "feedback": 0.5, "mix": 0.5})
	buf := sine(960, 1000, 0.5)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			if err := e.Reset(); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 500; i++ {
		if err := e.Process(buf, 480); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	<-done
	t.Logf("500 Reset/Process interleavings, last peak=%.6f", peak(buf))
}

// B5: an infinite control callback is interrupted and Close returns.

func TestInfiniteControlCallbackIsInterrupted(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Spin",
  build = function(ctx)
    ctx.control(function(p) while true do end end)
    ctx.out(ctx.input())
  end,
}`, nil)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && e.Watchdog() == "" {
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("watchdog on an infinite callback: %q", e.Watchdog())
	if e.Watchdog() == "" {
		t.Fatal("an infinite control callback was never caught")
	}
	done := make(chan struct{})
	go func() {
		_ = e.Close()
		close(done)
	}()
	select {
	case <-done:
		t.Log("Close returned after the fault")
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung on an infinite control callback")
	}
}

func TestSlowButReturningCallbackIsFaulted(t *testing.T) {
	// This loop returns, but takes a few ms, over the 2 ms budget. It must be
	// caught as a fault, and it must not take 100x the budget to notice.
	e := buildConfigured(t, `return effect {
  name = "SlowReturn",
  build = function(ctx)
    ctx.control(function(p)
      local t = 0
      for i = 1, 120000 do t = t + i end
    end)
    ctx.out(ctx.input())
  end,
}`, nil)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.Watchdog() == "" {
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("slow-but-returning callback: %q", e.Watchdog())
	if e.Watchdog() == "" {
		t.Fatal("a callback over the budget was not faulted")
	}
	if strings.Contains(e.Watchdog(), "exceeded") {
		t.Fatalf("the callback returned, so the budget check should have caught it, not the deadline: %q", e.Watchdog())
	}
}

func TestHealthyControlCallbackIsNotFaulted(t *testing.T) {
	// The watchdog must not trip on a callback that stays under the budget.
	e := buildConfigured(t, `return effect {
  name = "Healthy",
  params = { freq = { float, min = 20, max = 20000, default = 1000 } },
  build = function(ctx)
    local lp = ctx.biquad { type = "lowpass", freq = ctx.param("freq") }
    ctx.control(function(p) lp:set { freq = p.freq, q = 0.707 } end)
    ctx.out(lp(ctx.input()))
  end,
}`, nil)
	time.Sleep(500 * time.Millisecond)
	t.Logf("healthy callback watchdog: %q", e.Watchdog())
	if e.Watchdog() != "" {
		t.Fatalf("a healthy callback was faulted: %q", e.Watchdog())
	}
}

// B6: an infinite loop in the load chunk or build() fails Load.

func TestInfiniteLoadChunkTimesOut(t *testing.T) {
	started := time.Now()
	_, err := Load(writeScript(t, `while true do end`))
	elapsed := time.Since(started)
	t.Logf("infinite chunk returned err=%v after %s", err, elapsed.Round(time.Millisecond))
	if err == nil {
		t.Fatal("an infinite load chunk was accepted")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("load took %s, the deadline did not fire", elapsed)
	}
}

func TestInfiniteBuildTimesOut(t *testing.T) {
	started := time.Now()
	_, err := Load(writeScript(t, `return effect {
  name = "SpinBuild",
  build = function(ctx) while true do end end,
}`))
	elapsed := time.Since(started)
	t.Logf("infinite build returned err=%v after %s", err, elapsed.Round(time.Millisecond))
	if err == nil {
		t.Fatal("an infinite build was accepted")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("build took %s, the deadline did not fire", elapsed)
	}
}

// M1: a signal in a scalar field is a load error.

func TestSignalInScalarFieldFailsLoad(t *testing.T) {
	_, err := Load(writeScript(t, `return effect {
  name = "Hijack",
  build = function(ctx)
    local lp = ctx.biquad { type = "lowpass", freq = ctx.input() }
    ctx.out(lp(ctx.input()))
  end,
}`))
	t.Logf("signal in a scalar field: %v", err)
	if err == nil {
		t.Fatal("a signal in the freq field loaded silently")
	}
	if !strings.Contains(err.Error(), "freq") {
		t.Fatalf("error does not name the field: %v", err)
	}
}

// M2: a parameter-backed biquad gain moves the coefficients.

func TestBiquadGainParamMovesCoefficients(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Peak",
  params = { gain = { float, min = -24, max = 24, default = 12 }, freq = { float, min = 20, max = 20000, default = 1000 } },
  build = function(ctx)
    local peq = ctx.biquad { type = "peaking", freq = ctx.param("freq"), q = 1, gain = ctx.param("gain") }
    ctx.out(peq(ctx.input()))
  end,
}`, dsp.Values{"gain": 12.0, "freq": 1000.0})

	coeffs := func() [5]float64 {
		st := e.state.Load()
		for _, n := range st.plan.ctrl {
			if n != nil && n.kind == kindBiquad {
				s := n.state.(*biquadState)

				return [5]float64{s.b0, s.b1, s.b2, s.a1, s.a2}
			}
		}
		t.Fatal("no biquad in the plan")

		return [5]float64{}
	}

	// The coefficients are applied on the audio path, so run one block first.
	if err := e.Process(sine(960, 1000, 0.5), 480); err != nil {
		t.Fatalf("Process: %v", err)
	}
	before := coeffs()
	literal := func() [5]float64 {
		le := buildConfigured(t, `return effect {
  name = "PeakLit",
  build = function(ctx)
    local peq = ctx.biquad { type = "peaking", freq = 1000, q = 1, gain = 12 }
    ctx.out(peq(ctx.input()))
  end,
}`, nil)
		if err := le.Process(sine(960, 1000, 0.5), 480); err != nil {
			t.Fatalf("Process literal: %v", err)
		}
		st := le.state.Load()
		for _, n := range st.plan.ctrl {
			if n != nil && n.kind == kindBiquad {
				s := n.state.(*biquadState)

				return [5]float64{s.b0, s.b1, s.b2, s.a1, s.a2}
			}
		}

		return [5]float64{}
	}()
	t.Logf("param gain=12 b0=%.6f, literal gain=12 b0=%.6f", before[0], literal[0])
	if math.Abs(before[0]-literal[0]) > 1e-6 {
		t.Fatalf("param gain did not reach the coefficients: %v vs %v", before, literal)
	}

	if err := e.Set("gain", -24.0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := e.Process(sine(960, 1000, 0.5), 480); err != nil {
		t.Fatalf("Process after Set: %v", err)
	}
	after := coeffs()
	t.Logf("after Set(gain,-24) b0=%.6f (want ~0.251)", after[0])
	if math.Abs(after[0]-before[0]) < 1e-6 {
		t.Fatalf("Set(gain) did not move b0: %.6f", after[0])
	}
}

// M3: a parameter-only delay time requires an explicit max.

func TestParamOnlyDelayNeedsMax(t *testing.T) {
	path := writeScript(t, `return effect {
  name = "ParamDelay",
  params = { time = { float, min = 0.01, max = 1.5, default = 1.0 } },
  build = function(ctx)
    local d = ctx.delay { time = ctx.param("time") }
    d.input = ctx.input()
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
	_, err = e.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32})
	t.Logf("param-only delay Configure: %v", err)
	if err == nil {
		t.Fatal("a parameterised delay time with no max configured a 1-frame line")
	}
	if !strings.Contains(err.Error(), "max") {
		t.Fatalf("error does not mention max: %v", err)
	}

	// With max, the line is sized from it.
	ok := buildConfigured(t, `return effect {
  name = "ParamDelayOk",
  params = { time = { float, min = 0.01, max = 1.5, default = 1.0 } },
  build = function(ctx)
    local d = ctx.delay { max = 1.0, time = ctx.param("time") }
    d.input = ctx.input()
    ctx.out(d.tap)
  end,
}`, nil)
	for _, n := range ok.state.Load().plan.ops {
		if n.kind == kindDelay {
			frames := n.state.(*delayState).frames
			t.Logf("paramed delay with max=1.0: frames=%d (want %d)", frames, testRate)
			if frames != testRate {
				t.Fatalf("line sized %d, want %d", frames, testRate)
			}
		}
	}
}

// M4: LFO phase and shape take effect.

func TestLFOPhaseAndShape(t *testing.T) {
	peakOf := func(body string) float64 {
		e := buildConfigured(t, body, nil)
		buf := make([]float32, 4800*2)
		if err := e.Process(buf, 4800); err != nil {
			t.Fatalf("Process: %v", err)
		}

		return peak(buf)
	}

	square := buildConfigured(t, `return effect {
  name = "Square",
  build = function(ctx) ctx.out(ctx.lfo{ rate = 10, shape = "square" }) end,
}`, nil)
	buf := make([]float32, 4800*2)
	if err := square.Process(buf, 4800); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// A square oscillator is only ever at its two levels.
	offLevel := 0
	for i := 0; i < 4800; i++ {
		if v := math.Abs(float64(buf[i*2])); math.Abs(v-1) > 1e-6 {
			offLevel++
		}
	}
	t.Logf("square lfo samples off the +/-1 levels: %d", offLevel)
	if offLevel != 0 {
		t.Fatalf("square shape was not applied: %d samples off level", offLevel)
	}

	saw := buildConfigured(t, `return effect {
  name = "Saw",
  build = function(ctx) ctx.out(ctx.lfo{ rate = 10, shape = "saw" }) end,
}`, nil)
	sawBuf := make([]float32, 4800*2)
	if err := saw.Process(sawBuf, 4800); err != nil {
		t.Fatalf("Process: %v", err)
	}
	downSteps := 0
	for i := 1; i < 4800; i++ {
		if sawBuf[i*2] < sawBuf[(i-1)*2] {
			downSteps++
		}
	}
	t.Logf("saw lfo falling steps over one block: %d", downSteps)
	if downSteps == 0 {
		t.Fatal("saw shape never falls; it is not a saw")
	}

	_ = peakOf
}

func TestLFOPhaseShiftsTheOscillator(t *testing.T) {
	at := func(phase string) float64 {
		e := buildConfigured(t, `return effect {
  name = "Phase",
  build = function(ctx) ctx.out(ctx.lfo{ rate = 5, phase = `+phase+` }) end,
}`, nil)
		buf := make([]float32, 96*2)
		if err := e.Process(buf, 96); err != nil {
			t.Fatalf("Process: %v", err)
		}

		return float64(buf[0])
	}
	zero := at("0.0")
	quarter := at("0.25")
	t.Logf("lfo first sample: phase 0 = %.6f, phase 0.25 = %.6f", zero, quarter)
	if math.Abs(quarter-zero) < 1e-3 {
		t.Fatalf("phase had no effect: %.6f vs %.6f", zero, quarter)
	}
}

// M5: a non-finite sample recovers to silence across stateful nodes.

func TestNonFiniteRecoversForStatefulNodes(t *testing.T) {
	cases := map[string]string{
		"onepole": `return effect {
  name = "NF",
  build = function(ctx)
    local n = ctx.onepole { cutoff = 100 }
    n.input = ctx.input()
    ctx.out(n)
  end,
}`,
		"env": `return effect {
  name = "NF",
  build = function(ctx)
    local n = ctx.env { attack = 0.01, release = 0.1 }
    n.input = ctx.input()
    ctx.out(ctx.input() * n)
  end,
}`,
	}
	for name, body := range cases {
		e := buildConfigured(t, body, nil)
		bad := make([]float32, 2*2)
		bad[0], bad[1] = float32(math.NaN()), float32(math.NaN())
		if err := e.Process(bad, 2); err != nil {
			t.Fatalf("%s Process NaN: %v", name, err)
		}
		good := make([]float32, 4800*2)
		for i := range good {
			good[i] = 0.5
		}
		if err := e.Process(good, 4800); err != nil {
			t.Fatalf("%s Process: %v", name, err)
		}
		last := float64(good[len(good)-1])
		t.Logf("%s after NaN then a tone: last=%.6f", name, last)
		if math.IsNaN(last) || math.IsInf(last, 0) {
			t.Fatalf("%s stayed non-finite: %v", name, last)
		}
	}
}

// M6: a feedback gain at or above unity stays finite.

func TestDelayFeedbackGainAboveUnityStaysFinite(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Runaway",
  build = function(ctx)
    local d = ctx.delay { max = 0.1, time = 0.005 }
    d.input = ctx.input() + d.tap * ctx.const(2)
    ctx.out(d.tap)
  end,
}`, nil)

	imp := make([]float32, 2)
	imp[0], imp[1] = 1, 1
	if err := e.Process(imp, 1); err != nil {
		t.Fatalf("Process impulse: %v", err)
	}
	nonFinite := 0
	var lastPeak float64
	for i := 0; i < 20; i++ {
		sil := make([]float32, 4800*2)
		if err := e.Process(sil, 4800); err != nil {
			t.Fatalf("Process: %v", err)
		}
		for _, v := range sil {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				nonFinite++
			}
		}
		lastPeak = peak(sil)
	}
	t.Logf("feedback x2 over 20 silent blocks: nonFinite=%d finalPeak=%.6f", nonFinite, lastPeak)
	if nonFinite != 0 {
		t.Fatalf("a unity-or-greater feedback loop produced %d non-finite samples", nonFinite)
	}
	if lastPeak > 8 {
		t.Fatalf("a unity-or-greater feedback loop grew unbounded: %.6f", lastPeak)
	}
}

// M7: a script parameter colliding with a standard key fails at Load.

func TestStandardParamCollisionFailsLoad(t *testing.T) {
	for _, key := range []string{dsp.ParamBypass, dsp.ParamInputGain, dsp.ParamOutputGain} {
		_, err := Load(writeScript(t, `return effect {
  name = "Collide",
  params = { ["`+key+`"] = { float, min = 0, max = 1, default = 0 } },
  build = function(ctx) ctx.out(ctx.input()) end,
}`))
		t.Logf("collision on %q: %v", key, err)
		if err == nil {
			t.Fatalf("parameter %q collided with a standard key and Load accepted it", key)
		}
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error does not name %q: %v", key, err)
		}
	}
}

// M8: a watchdog fault can be cleared through the documented path.

func TestClearFaultRecoversFromWatchdog(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Slow",
  build = function(ctx)
    ctx.control(function(p)
      local t = 0
      for i = 1, 5000000 do t = t + i end
    end)
    ctx.out(ctx.input())
  end,
}`, nil)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && e.Watchdog() == "" {
		time.Sleep(20 * time.Millisecond)
	}
	if e.Watchdog() == "" {
		t.Fatal("the slow callback never tripped the watchdog")
	}
	if !e.Bypassed() {
		t.Fatal("a faulted effect is not bypassed")
	}
	t.Logf("faulted: %q", e.Watchdog())

	e.ClearFault()
	t.Logf("after ClearFault: watchdog=%q bypassed=%v", e.Watchdog(), e.Bypassed())
	if e.Watchdog() != "" {
		t.Fatal("ClearFault did not clear the verdict")
	}
	if e.Bypassed() {
		t.Fatal("ClearFault did not un-bypass the effect")
	}
}

// MINOR: a `set{}` writing two fields honors both.

func TestControlSetWritesEveryField(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "TwoFields",
  build = function(ctx)
    local lp = ctx.biquad { type = "lowpass" }
    ctx.control(function(p) lp:set { freq = 1000, q = 0.5 } end)
    ctx.out(lp(ctx.input()))
  end,
}`, nil)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		vals := e.control.latest()
		if len(vals) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	vals := e.control.latest()
	if len(vals) == 0 {
		t.Fatal("the control callback published nothing")
	}
	got := vals[0].coeffs
	// A lowpass at 1 kHz with q 0.5 has a different b0 than q 0.707.
	want, err := designBiquad(&node{kind: kindBiquad, fields: map[string]any{"type": "lowpass"}}, 1000, 0.5, 0, testRate)
	if err != nil {
		t.Fatalf("designBiquad: %v", err)
	}
	t.Logf("set{freq,q} b0=%.6f want=%.6f", got[0], want[0])
	if math.Abs(got[0]-want[0]) > 1e-9 {
		t.Fatalf("set honored only the first field: got %v want %v", got, want)
	}
}

// MINOR: `set{}` on a node with parameter-backed fields is refused: the write
// would replace the resolved set with the literals and drop every parameterised
// field. The fault stops the effect rather than silently zeroing the params.
func TestControlSetRefusedOnParamBackedFields(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "ParamShaper",
  params = { threshold = { float, min = -60, max = 0, default = -18 } },
  build = function(ctx)
    local red = ctx.shaper { shape = "softknee", threshold = ctx.param("threshold"),
                             ratio = 4, knee = 8 }
    ctx.control(function(p) red:set { drive = 2 } end)
    ctx.out(red(ctx.input()))
  end,
}`, nil)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.Watchdog() != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	reason := e.Watchdog()
	t.Logf("watchdog: %q", reason)
	if reason == "" {
		t.Fatal("a set on a parameter-backed shaper did not fault the control callback")
	}
	if !strings.Contains(reason, "parameter-backed") {
		t.Fatalf("watchdog reason does not explain the refusal: %q", reason)
	}
}

// MINOR: one-argument min/max/mul/div is a load error, not a silent zero.

func TestOneArgumentCombinatorsFailLoad(t *testing.T) {
	for _, call := range []string{"ctx.max(ctx.input())", "ctx.min(ctx.input())", "ctx.mul(ctx.input())", "ctx.div(ctx.input())"} {
		_, err := Load(writeScript(t, `return effect {
  name = "OneArg",
  build = function(ctx) ctx.out(`+call+`) end,
}`))
		t.Logf("%s: %v", call, err)
		if err == nil {
			t.Fatalf("%s loaded; a missing operand must be an error", call)
		}
	}
}

// MINOR: a chunk error names the file, not <string>.

func TestLoadErrorNamesTheFile(t *testing.T) {
	path := writeScript(t, `return effect {
  name = "Err",
  build = function(ctx) error("boom") end,
}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a script that errors")
	}
	t.Logf("error: %v", err)
	if strings.Contains(err.Error(), "<string>") {
		t.Fatalf("error still uses the <string> chunk name: %v", err)
	}
	if !strings.Contains(err.Error(), "test.lua") {
		t.Fatalf("error does not name the file: %v", err)
	}
}

// STEREO: a filter's memory must be per channel. One shared set of memory
// values advances once per channel, so in stereo a filter runs twice per frame
// and its cutoff lands an octave high. A 1 kHz lowpass at q=0.707 must read
// -3 dB at 1 kHz in both mono and stereo.
func TestFilterCutoffIsChannelIndependent(t *testing.T) {
	measure := func(ch int, freq float64) float64 {
		t.Helper()

		f, err := Load(filepath.Join("testdata", "lowpass.lua"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		e, err := f.New(dsp.Values{"freq": 1000.0, "q": 0.707})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ef := e.(*Effect)
		if _, err := ef.Configure(core.FrameFormat{Rate: testRate, Ch: ch, Fmt: core.F32}); err != nil {
			t.Fatalf("Configure: %v", err)
		}
		t.Cleanup(func() { _ = ef.Close() })

		const frames = 48000
		buf := make([]float32, frames*ch)
		for i := 0; i < frames; i++ {
			v := float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/testRate))
			for c := 0; c < ch; c++ {
				buf[i*ch+c] = v
			}
		}
		if err := ef.Process(buf, frames); err != nil {
			t.Fatalf("Process: %v", err)
		}
		var sum float64
		n := 0
		for i := frames / 2; i < frames; i++ {
			v := float64(buf[i*ch])
			sum += v * v
			n++
		}

		return 20 * math.Log10(math.Sqrt(sum/float64(n))/(0.5/math.Sqrt2))
	}

	mono := measure(1, 1000)
	stereo := measure(2, 1000)
	t.Logf("lowpass 1 kHz at its corner: mono %.2f dB, stereo %.2f dB (want -3)", mono, stereo)

	if math.Abs(mono-(-3.01)) > 0.3 {
		t.Fatalf("mono corner is %.2f dB, want -3.01", mono)
	}
	if math.Abs(stereo-(-3.01)) > 0.3 {
		t.Fatalf("stereo corner is %.2f dB, want -3.01; the filter advanced once per channel, not per frame", stereo)
	}
}
