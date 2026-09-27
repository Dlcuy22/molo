package script

// Tests for script-declared latency and the soft-knee shaper. The shaper law is
// pinned against the same quadratic the frontend draws, so the curve and the
// audio cannot drift.

import (
	"math"
	"strconv"
	"testing"
	"time"
)

// TestLatencyLiteralAndParam pins the three ways ctx.latency resolves: a
// literal, no declaration, and a parameter that follows Set.
func TestLatencyLiteralAndParam(t *testing.T) {
	literal := buildConfigured(t, `return effect {
  name = "Literal",
  build = function(ctx)
    ctx.out(ctx.input())
    ctx.latency(0.005)
  end,
}`, nil)
	if got, want := literal.Latency(), 5*time.Millisecond; got != want {
		t.Fatalf("literal latency = %v, want %v", got, want)
	}

	none := buildConfigured(t, `return effect {
  name = "None",
  build = function(ctx)
    ctx.out(ctx.input())
  end,
}`, nil)
	if got := none.Latency(); got != 0 {
		t.Fatalf("undeclared latency = %v, want 0", got)
	}

	param := buildConfigured(t, `return effect {
  name = "Param",
  params = { look = { float, min = 0, max = 0.02, default = 0.01 } },
  build = function(ctx)
    ctx.out(ctx.input())
    ctx.latency(ctx.param("look"))
  end,
}`, nil)
	if got, want := param.Latency(), 10*time.Millisecond; got != want {
		t.Fatalf("param latency = %v, want %v", got, want)
	}
	if err := param.Set("look", 0.02); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, want := param.Latency(), 20*time.Millisecond; got != want {
		t.Fatalf("latency after Set = %v, want %v", got, want)
	}
}

// TestLatencyRejectsBadInput pins the load errors: a non-number, a negative
// literal, a duplicate declaration, an undeclared param, and a param of the
// wrong kind are all mistakes.
func TestLatencyRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		params string
		call   string
	}{
		"string":     {call: `ctx.latency("nope")`},
		"negative":   {call: `ctx.latency(-0.1)`},
		"duplicate":  {call: `ctx.latency(0.001) ctx.latency(0.002)`},
		"undeclared": {call: `ctx.latency(ctx.param("missing"))`},
		"bool param": {
			params: `params = { on = { bool, default = false } },`,
			call:   `ctx.latency(ctx.param("on"))`,
		},
	}
	for name, c := range cases {
		_, err := Load(writeScript(t, `return effect {
  name = "BadLatency",
  `+c.params+`
  build = function(ctx)
    ctx.out(ctx.input())
    `+c.call+`
  end,
}`))
		t.Logf("%s: %v", name, err)
		if err == nil {
			t.Fatalf("%s latency declaration loaded", name)
		}
	}
}

// TestDelayShiftsOutputByFrames drives an impulse through a delay and finds the
// delayed sample, which pins that a delay of n frames returns the input from n
// frames ago. A delay of zero is a pass-through, so the impulse stays put.
func TestDelayShiftsOutputByFrames(t *testing.T) {
	for _, frames := range []int{0, 1, 8, 64} {
		sec := float64(frames) / float64(testRate)
		e := buildConfigured(t, `return effect {
  name = "DelayShift",
  build = function(ctx)
    local d = ctx.delay { max = 0.1, time = `+formatFloat(sec)+` }
    d.input = ctx.input()
    ctx.out(d)
  end,
}`, nil)

		const n = 256
		buf := make([]float32, n*2)
		// The impulse sits at frame 4 so the delay's line has a clean history.
		buf[4*2] = 1
		buf[4*2+1] = 1
		if err := e.Process(buf, n); err != nil {
			t.Fatalf("Process: %v", err)
		}
		want := 4 + frames
		if buf[want*2] != 1 {
			t.Fatalf("delay of %d frames: impulse landed at %d, want %d (buf=%v)", frames, impulseIndex(buf, n), want, buf[:40])
		}
	}
}

// impulseIndex finds the first non-zero frame, so a failure reports where the
// impulse actually landed.
func impulseIndex(buf []float32, frames int) int {
	for i := 0; i < frames; i++ {
		if buf[i*2] != 0 {
			return i
		}
	}

	return -1
}

// formatFloat renders a float with full precision, so the Lua literal carries
// the exact value the test asked for.
func formatFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// frontendReduction mirrors ui/webui/frontend/.../transfer-curve.ts: the gain
// reduction is transferSample(x) - (x + makeup), with makeup dropped as 0. It
// exists so the Go law is checked against the curve the UI draws, not a copy of
// the Go branch.
func frontendReduction(x, threshold, ratio, knee float64) float64 {
	if ratio < 1 {
		ratio = 1
	}
	if knee < 0 {
		knee = 0
	}
	transfer := func(x float64) float64 {
		if knee <= 0 {
			if x <= threshold {
				return x
			}

			return threshold + (x-threshold)/ratio
		}
		half := knee / 2
		d := x - threshold
		if d <= -half {
			return x
		}
		if d >= half {
			return threshold + d/ratio
		}

		return x + (1/ratio-1)*((d+half)*(d+half))/(2*knee)
	}

	return transfer(x) - x
}

// TestSoftKneeShaperLaw pins the shaper against the quadratic the frontend
// draws. It routes a set of constant levels through the shape and reads the
// reduction the meter reports.
func TestSoftKneeShaperLaw(t *testing.T) {
	const threshold, ratio, knee = -20.0, 4.0, 8.0
	shape := func(level float64) float64 {
		e := buildConfigured(t, `return effect {
  name = "KneeProbe",
  build = function(ctx)
    local red = ctx.shaper { shape = "softknee", threshold = `+formatFloat(threshold)+`,
                             ratio = `+formatFloat(ratio)+`, knee = `+formatFloat(knee)+` }(ctx.const(`+formatFloat(level)+`))
    local m = ctx.meter("red", { label = "Red", unit = "dB", min = -30, max = 0, kind = "gain-reduction" })
    m(red)
    ctx.out(red)
  end,
}`, nil)
		buf := make([]float32, 64*2)
		if err := e.Process(buf, 64); err != nil {
			t.Fatalf("Process: %v", err)
		}

		return float64(e.Meters()["red"])
	}

	slope := 1 - 1/ratio
	half := knee / 2

	// Below the lower knee edge: no reduction.
	if got := shape(threshold - half - 1); got != 0 {
		t.Fatalf("below the knee reduction = %v, want 0", got)
	}
	// At the threshold: half of the way across the knee's quadratic.
	if want := slope * knee / 8; math.Abs(shape(threshold)-want) > 1e-4 {
		t.Fatalf("at the threshold reduction = %v, want %v", shape(threshold), want)
	}
	// Every level across the knee matches the frontend's transferSample, where
	// the reduction is transferSample(x) - (x + makeup).
	for x := threshold - half - 2; x <= threshold+half+2; x += 0.5 {
		want := -frontendReduction(x, threshold, ratio, knee)
		if got := shape(x); math.Abs(got-want) > 1e-4 {
			t.Fatalf("level %v: reduction = %v, want %v (frontend law)", x, got, want)
		}
	}
	// Above the upper knee edge: the linear segment, reduction = slope*(x-threshold).
	over := 10.0
	if want := slope * (half + over); math.Abs(shape(threshold+half+over)-want) > 1e-4 {
		t.Fatalf("above the knee reduction = %v, want %v", shape(threshold+half+over), want)
	}
	// A knee of zero is a hard knee: nothing at or below the threshold, and the
	// same linear segment above it.
	hard := buildConfigured(t, `return effect {
  name = "HardProbe",
  build = function(ctx)
    ctx.out(ctx.const(0))
    local red = ctx.shaper { shape = "softknee", threshold = `+formatFloat(threshold)+`,
                             ratio = `+formatFloat(ratio)+`, knee = 0 }(ctx.const(`+formatFloat(threshold+over)+`))
    local m = ctx.meter("red", { label = "Red", unit = "dB", min = -30, max = 0, kind = "gain-reduction" })
    m(red)
  end,
}`, nil)
	hb := make([]float32, 64*2)
	if err := hard.Process(hb, 64); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if want := slope * over; math.Abs(float64(hard.Meters()["red"])-want) > 1e-4 {
		t.Fatalf("hard knee reduction = %v, want %v", hard.Meters()["red"], want)
	}
}
