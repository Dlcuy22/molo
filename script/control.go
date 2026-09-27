package script

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Tier 2 is the optional control callback. It runs on its own goroutine, never
// on the audio thread and never per sample, and writes only values the audio
// plan reads. The watchdog exists because a script is untrusted: a callback
// that blocks or loops must not be allowed to starve anything, so a budget is
// measured per invocation and a breach silences the effect.
//
// The bridge is deliberately narrow. The callback receives a table of current
// parameter values and calls `node:set{ field = value }` on handles it kept
// from build. Those writes land in a Go map, which is then converted to
// precomputed coefficients and published atomically. The audio thread only ever
// copies finished numbers out of the published snapshot, so it never runs math
// the control side could have done.

// controlRate is how often the callback runs. About 10 Hz is the plan's figure:
// fast enough to track a signal for a compressor, slow enough that a script
// cannot mistake this for a sample-rate hook.
const controlRate = 10

// watchdogBudget is how long one control callback may run before it is treated
// as a fault. The audio block is about 100 ms, so 2 ms leaves a callback ample
// room while still catching a script that does real work or loops forever.
const watchdogBudget = 2 * time.Millisecond

// watchdogStrikes is how many consecutive over-budget ticks fault the effect.
// The budget is a wall-clock measurement, so one overrun can be a scheduler
// preemption or a GC pause rather than the script's fault; a callback that is
// genuinely too slow overruns every tick, so a small run separates the two
// while still catching a bad script within a few hundred milliseconds.
const watchdogStrikes = 3

// callbackDeadline is the real deadline the interpreter enforces. It is wide
// enough that a callback doing genuine work under the budget is never cut off,
// and finite so an infinite loop is interrupted instead of hanging Close.
const callbackDeadline = 250 * time.Millisecond

// ctrlValue is one precomputed value the control side publishes for a node. The
// audio thread applies it by copying floats, so the expensive part of a
// coefficient update stays off the audio path.
type ctrlValue struct {
	// kind selects what the value writes: a param signal, a node's resolved
	// scalar set, a biquad coefficient set, or a one-pole coefficient.
	kind   ctrlKind
	index  int
	scalar float64
	vals   nodeValues
	coeffs [5]float64 // b0, b1, b2, a1, a2
}

type ctrlKind uint8

const (
	ctrlParam ctrlKind = iota
	ctrlVals
	ctrlBiquad
	ctrlOnePole
)

// controlValues is one published callback result: a flat list of node updates
// plus bypass, all immutable once stored.
type controlValues struct {
	values []ctrlValue
}

// controlBridge is the Lua-side state of a Tier 2 callback. It lives on the
// control goroutine only, so the interpreter is never touched concurrently.
type controlBridge struct {
	state  *lua.LState
	fn     *lua.LFunction
	params *lua.LTable
	// nodes is the graph's declaration-ordered node list, so a dirty write
	// keyed by position resolves without a map.
	nodes []*node
	rate  int
	// dirty holds the writes `node:set{...}` made during one invocation,
	// keyed by node index then field name. Cleared before each tick.
	dirty map[int]map[string]float64
}

// controlRunner owns the Tier 2 loop. A nil runner means a Tier 1 script.
type controlRunner struct {
	bridge *controlBridge
	stop   chan struct{}
	done   chan struct{}

	committed atomic.Pointer[controlValues]

	// overBudget counts consecutive callbacks that ran past the budget. A
	// single overrun is not enough to fault the effect: the budget is measured
	// with a wall clock, so one scheduler preemption or GC pause can push a
	// trivial callback past 2 ms on a loaded machine, and faulting on that
	// would brick a working effect until the host cleared it. A callback that
	// is genuinely too slow, or loops forever, overruns every tick, so
	// requiring a few in a row separates the two without letting a bad script
	// run long.
	overBudget int

	startOnce sync.Once
}

// newControlRunner wires a script's control function to a fresh bridge. Each
// effect gets its own, so two instances built from one script never share
// control state. The `node:set` closures reach their sink through the node, so
// they follow the effect's plan rather than the loader-time graph.
func newControlRunner(ls *luaState) (*controlRunner, error) {
	if ls == nil || ls.state == nil {
		return nil, fmt.Errorf("script: control callback needs a live interpreter")
	}
	fn, ok := ls.state.GetGlobal(controlGlobal).(*lua.LFunction)
	if !ok {
		return nil, fmt.Errorf("script: control callback is missing")
	}

	return &controlRunner{
		bridge: &controlBridge{
			state:  ls.state,
			fn:     fn,
			params: ls.state.NewTable(),
			dirty:  make(map[int]map[string]float64),
		},
	}, nil
}

// controlGlobal is where the loader stashes the control function on the
// interpreter's globals so the runner can recover it without a second return
// value.
const controlGlobal = "__player_control"

// write records a `node:set{...}` value from inside the callback.
func (b *controlBridge) write(n *node, field string, value float64) {
	fields, ok := b.dirty[n.position]
	if !ok {
		fields = make(map[string]float64, 2)
		b.dirty[n.position] = fields
	}
	fields[field] = value
}

// invoke runs one tick: rewrite the params table, run the callback, then turn
// the writes into precomputed node updates. The callback runs under a context
// deadline, so an infinite loop is interrupted rather than hanging the control
// goroutine and Close with it. The deadline has real headroom over the measured
// budget because the interpreter checks it once per Lua instruction, not on a
// wall clock.
func (b *controlBridge) invoke(values map[string]any) ([]ctrlValue, error) {
	for k := range b.dirty {
		delete(b.dirty, k)
	}
	for k, v := range values {
		b.state.SetField(b.params, k, goToLua(v))
	}
	ctx, cancel := context.WithTimeout(context.Background(), callbackDeadline)
	defer cancel()
	b.state.SetContext(ctx)
	defer b.state.RemoveContext()

	b.state.Push(b.fn)
	b.state.Push(b.params)
	if err := b.state.PCall(1, 0, nil); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("control callback exceeded %s and was interrupted", callbackDeadline)
		}

		return nil, err
	}

	return b.collect()
}

// collect converts the dirty writes into ctrlValues, recomputing coefficients
// here so the audio thread never does.
func (b *controlBridge) collect() ([]ctrlValue, error) {
	if len(b.dirty) == 0 {
		return nil, nil
	}
	out := make([]ctrlValue, 0, len(b.dirty))
	for pos, fields := range b.dirty {
		n := b.nodeAt(pos)
		if n == nil {
			continue
		}
		val, err := b.toCtrlValue(n, fields)
		if err != nil {
			return nil, err
		}
		out = append(out, val)
	}

	return out, nil
}

// nodeAt resolves a position to its node; the bridge keeps the graph's node
// list so the lookup is a direct index.
func (b *controlBridge) nodeAt(pos int) *node {
	if pos < 0 || pos >= len(b.nodes) {
		return nil
	}

	return b.nodes[pos]
}

// replaceNodes points the bridge at a fresh plan's node list after a Reset
// rebuilt it, so a `node:set` write from the next tick lands in the plan the
// audio thread is actually running.
func (b *controlBridge) replaceNodes(nodes []*node) {
	b.nodes = nodes
}

// hasParamFields reports whether any of a node's scalar fields is backed by a
// parameter, so a generic `set` would clobber the resolved values.
func hasParamFields(n *node) bool { return len(n.paramFields) > 0 }

// toCtrlValue converts one node's writes to the value the audio side applies.
func (b *controlBridge) toCtrlValue(n *node, fields map[string]float64) (ctrlValue, error) {
	switch n.kind {
	case kindBiquad:
		freq, hasFreq := fields["freq"]
		if !hasFreq {
			return ctrlValue{}, fmt.Errorf("script: biquad control needs a freq")
		}
		q, hasQ := fields["q"]
		if !hasQ {
			q = 0.707
		}
		gain := n.lit.gain
		if g, ok := fields["gain"]; ok {
			gain = g
		}
		v := ctrlValue{kind: ctrlBiquad, index: n.position}
		c, err := designBiquad(n, freq, q, gain, b.rate)
		if err != nil {
			return ctrlValue{}, err
		}
		v.coeffs = c

		return v, nil

	case kindOnePole:
		cutoff, ok := fields["cutoff"]
		if !ok {
			return ctrlValue{}, fmt.Errorf("script: onepole control needs a cutoff")
		}
		v := ctrlValue{kind: ctrlOnePole, index: n.position}
		v.scalar = smoothCoeff(1.0/cutoff, b.rate)

		return v, nil
	default:
		// A generic scalar: the node's resolved scalar set changes, such as an
		// lfo rate or a scale factor. A node with parameter-backed fields
		// resolves them into the same set, so a write here would replace it
		// with the literals and silently drop every parameterised field.
		if hasParamFields(n) {
			return ctrlValue{}, fmt.Errorf("script: %s has parameter-backed fields and cannot be set; drive them through their parameters", n.label())
		}
		v := ctrlValue{kind: ctrlVals, index: n.position, vals: n.lit}
		for _, f := range []string{"rate", "factor", "value", "time", "drive", "phase", "threshold", "ratio", "knee"} {
			x, ok := fields[f]
			if !ok {
				continue
			}
			switch f {
			case "rate":
				v.vals.rate = x / float64(b.rate)
			case "phase":
				v.vals.phase = x
			case "time":
				v.vals.time = x * float64(b.rate)
			case "drive":
				v.vals.drive = x
			case "threshold":
				v.vals.threshold = x
			case "ratio":
				v.vals.ratio = x
			case "knee":
				v.vals.knee = x
			case "factor":
				v.vals.factor = x
			case "value":
				v.scalar = x
				v.kind = ctrlParam
			}
		}
		if v.kind == ctrlVals && v.vals == n.lit {
			return ctrlValue{}, fmt.Errorf("script: %s cannot be controlled", n.label())
		}

		return v, nil
	}
}

// start launches the control loop once. It is idempotent because Configure may
// run more than once for a format change.
func (r *controlRunner) start(e *Effect) {
	if r == nil {
		return
	}
	r.startOnce.Do(func() {
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
		go r.loop(e)
	})
}

// loop is the control goroutine.
func (r *controlRunner) loop(e *Effect) {
	defer close(r.done)
	ticker := time.NewTicker(time.Second / controlRate)
	defer ticker.Stop()

	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
		}
		if e.faulted() {
			// Once the watchdog has pulled the effect, keep the goroutine
			// parked rather than run a callback whose result is ignored.
			continue
		}

		snapshot := e.store.snapshot()
		started := time.Now()
		values, err := r.bridge.invoke(snapshot)
		elapsed := time.Since(started)
		if err != nil {
			e.noteFailure(fmt.Sprintf("control callback returned %v", err))

			return
		}
		if elapsed > watchdogBudget {
			r.overBudget++
			if r.overBudget >= watchdogStrikes {
				e.noteFailure(fmt.Sprintf("control callback ran %s, over the %s budget %d times in a row", elapsed.Round(time.Microsecond), watchdogBudget, r.overBudget))

				return
			}
			// A single overrun is not a fault yet. Skip publishing this tick so
			// a slow result cannot reach the audio path, and let the next tick
			// decide.
			continue
		}
		r.overBudget = 0
		r.committed.Store(&controlValues{values: values})
	}
}

// close stops the loop. It is safe on a niche runner and safe to call twice.
func (r *controlRunner) close() {
	if r == nil || r.stop == nil {
		return
	}
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
	r.stop = nil
}

// latest returns the latest published values, or nil before the first tick.
func (r *controlRunner) latest() []ctrlValue {
	if r == nil {
		return nil
	}
	v := r.committed.Load()
	if v == nil {
		return nil
	}

	return v.values
}

// applyControl copies the latest published values onto the plan. It is the only
// place the audio thread reads control state, and it is a bound number of float
// copies with no allocation.
func applyControl(p *plan, values []ctrlValue) {
	if p == nil || len(values) == 0 {
		return
	}
	for i := range values {
		v := &values[i]
		if v.index < 0 || v.index >= len(p.ctrl) {
			continue
		}
		n := p.ctrl[v.index]
		if n == nil {
			continue
		}
		switch v.kind {
		case ctrlParam:
			n.value = v.scalar

		case ctrlVals:
			n.vals = v.vals
			// An envelope keeps its smoothing coefficients in state, so they
			// are derived here as well. Two exps per parameter change per
			// block is nothing next to a block of samples, and it keeps the
			// script's attack and release live.
			if s, ok := n.state.(*envState); ok {
				s.attack = smoothCoeff(v.vals.attack, p.rate)
				s.release = smoothCoeff(v.vals.release, p.rate)
			}

		case ctrlBiquad:
			if s, ok := n.state.(*biquadState); ok {
				s.setCoeffs(v.coeffs[0], v.coeffs[1], v.coeffs[2], v.coeffs[3], v.coeffs[4])
			}

		case ctrlOnePole:
			if s, ok := n.state.(*onePoleState); ok {
				s.a = v.scalar
			}
		}
	}
}

// goToLua converts a Go parameter value to the Lua scalar the callback reads.
func goToLua(v any) lua.LValue {
	switch x := v.(type) {
	case bool:
		return lua.LBool(x)
	case string:
		return lua.LString(x)
	case int:
		return lua.LNumber(x)
	case float64:
		return lua.LNumber(x)
	case float32:
		return lua.LNumber(x)
	default:
		return lua.LNil
	}
}
