package script

import (
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/dsp"
)

// The effect wraps a compiled plan and exposes it as an ordinary dsp.Effect.
// Nothing above this file knows a script is involved: the registry, the chain,
// the preset loader and the UI all go through the same interface a built-in
// effect implements.

// Effect is a scripted effect instance.
type Effect struct {
	store *store
	name  string

	// graph is the compiled plan's source. It stays reachable so Configure can
	// rebuild the plan for a new rate; the plan itself is what the audio thread
	// runs.
	graph *graph

	// state is the immutable audio-side snapshot: the plan, the gains and the
	// bypass flags. Set rebuilds it and publishes it whole, so Process loads it
	// once and never races a control goroutine.
	state atomic.Pointer[compiledState]

	// mu serialises the control side: Set, Configure and Reset. It is never
	// held on the audio path.
	mu sync.Mutex

	rate, ch int
	isSet    bool

	// control is the Tier 2 callback and its published values. The callback
	// runs on a control goroutine; the audio thread only ever reads the
	// committed controlState below.
	control *controlRunner

	// lua owns the instance's interpreter when it is a Tier 2 script. Closing
	// it releases the sandbox; a Tier 1 instance has none.
	lua *luaState

	// watchdog is the running verdict on the control callback. It is atomic
	// because the watchdog goroutine writes it and Process reads it.
	watchdog atomic.Pointer[watchdogState]

	// meterIn and meterOut are the block meters. Only the audio thread pushes
	// into them and only a control goroutine reads, which is what the meter is
	// built for; neither takes a lock.
	meterIn  dsp.Meter
	meterOut dsp.Meter
}

// compiledState is what the audio thread sees: a plan plus everything that can
// change without recompiling. It is replaced whole and never mutated.
type compiledState struct {
	plan *plan
	// params is the resolved parameter set, recomputed on every Set. It is a
	// fresh slice, so a swap is atomic and Process reads one consistent set.
	params []ctrlValue
	// bypassed is the bypass parameter; watchdogFailed forces bypass
	// regardless of it, so a script that overran cannot keep touching audio.
	bypassed       bool
	watchdogFailed bool
	gains          gainState
}

// watchdogState is the fault verdict and its reason, kept together so a reader
// never sees half of a decision.
type watchdogState struct {
	failed bool
	reason string
}

// gainState is the compiled form of the two standard trims.
type gainState struct {
	in  float32
	out float32
}

var (
	_ dsp.Effect     = (*Effect)(nil)
	_ dsp.Bypassable = (*Effect)(nil)
	_ dsp.Metered    = (*Effect)(nil)
	_ dsp.Described  = (*Effect)(nil)
	_ dsp.Visualized = (*Effect)(nil)
	_ dsp.Latent     = (*Effect)(nil)
)

// Name identifies the instance in a chain.
func (e *Effect) Name() string { return e.name }

// Schema returns the parameter set, standard parameters included.
func (e *Effect) Schema() []dsp.Param { return e.store.Schema() }

// Get returns the current value of a parameter.
func (e *Effect) Get(key string) (any, error) { return e.store.Get(key) }

// Set validates a value, stores it and republishes the audio state. It is the
// only method a control goroutine calls while audio is running.
//
// A parameter change must not reset node state: moving a delay time or a filter
// cutoff would otherwise wipe the line and click. So Set does not recompile; it
// recomputes the resolved parameter values and swaps in a snapshot that reuses
// the same plan, which keeps every delay buffer and filter memory intact.
func (e *Effect) Set(key string, value any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.store.Set(key, value); err != nil {
		return err
	}

	return e.publishParamsLocked()
}

// Configure negotiates the format and builds the audio state for it. It is the
// only place the plan is compiled, so the plan always belongs to the rate and
// channel count Process will use.
func (e *Effect) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Rate <= 0 || in.Ch < 1 {
		return in, fmt.Errorf("script: %s needs a positive rate and channel count, got %+v", e.name, in)
	}
	if in.Fmt != core.F32 {
		return in, fmt.Errorf("script: %s needs float32 samples, got format %d", e.name, in.Fmt)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.rate = in.Rate
	e.ch = in.Ch
	e.isSet = true
	if err := e.publishLocked(); err != nil {
		return in, err
	}
	if e.control != nil {
		e.control.bridge.rate = in.Rate
		e.control.start(e)
	}

	return in, nil
}

// Process runs the plan in place. It loads the state once, so a Set during the
// call cannot make one buffer run on two configurations. It allocates nothing
// and takes no lock.
func (e *Effect) Process(buf []float32, frames int) error {
	st := e.state.Load()
	if st == nil {
		return nil
	}
	e.meterIn.Push(buf, frames, e.ch)

	if st.bypassed || st.watchdogFailed {
		e.meterOut.Push(buf, frames, e.ch)
		// The plan does not run while bypassed, so nothing would republish a
		// reading: it would stay frozen at its last pre-bypass value and report
		// a reduction the effect is no longer applying. Zeroing every reading
		// says "no reduction" instead of a stale one. The in/out pair still
		// measures the audio this pass-through carries, so it is left alone.
		if st.plan != nil {
			for _, n := range st.plan.meters {
				if n.meter != nil {
					n.meter.store(0)
				}
			}
		}

		return nil
	}
	// Resolve parameter values first, then any Tier 2 writes, both on a block
	// boundary so no buffer sees half a change. Both are float copies into
	// audio-owned fields, so neither allocates.
	applyControl(st.plan, st.params)
	applyControl(st.plan, e.control.latest())

	n := frames * e.ch
	if n > len(buf) {
		n = len(buf)
	}
	n -= n % e.ch

	if st.gains.in != 1 {
		for i := range buf[:n] {
			buf[i] *= st.gains.in
		}
	}
	st.plan.process(buf, frames)
	if st.gains.out != 1 {
		for i := range buf[:n] {
			buf[i] *= st.gains.out
		}
	}
	e.meterOut.Push(buf, frames, e.ch)

	return nil
}

// Bypassed reports the bypass parameter or a watchdog fault, so the chain can
// skip the effect entirely and contribute no latency.
func (e *Effect) Bypassed() bool {
	st := e.state.Load()

	return st != nil && (st.bypassed || st.watchdogFailed)
}

// Latency reports how far the effect delays its output; the chain sums it into
// the reported position. It is only called on the control side, so reading the
// store's mutex here is fine. A negative resolved value clamps to zero: a
// script cannot claim a latency that would advance the position.
func (e *Effect) Latency() time.Duration {
	if e.graph == nil {
		return 0
	}
	sec := e.graph.latencySec
	if e.graph.latencyParam != "" {
		sec = e.store.Float(e.graph.latencyParam)
	}
	if !(sec > 0) {
		return 0
	}

	return time.Duration(sec * float64(time.Second))
}

// Meters reports the input and output peaks in dBFS under the standard keys,
// plus one entry per meter node the script declared. The meter values are
// block samples the audio thread published; this only reads atomics, so it is
// safe from the control goroutine.
func (e *Effect) Meters() map[string]float32 {
	inPeak, _ := e.meterIn.Read().DB()
	outPeak, _ := e.meterOut.Read().DB()
	m := map[string]float32{dsp.MeterIn: inPeak, dsp.MeterOut: outPeak}
	if st := e.state.Load(); st != nil && st.plan != nil {
		for _, n := range st.plan.meters {
			if n.meter == nil {
				continue
			}
			key, _ := stringField(n, "key")
			if key == "" {
				continue
			}
			m[key] = float32(n.meter.load())
		}
	}

	return m
}

// Readings describes every meter node's key, so a UI knows how to draw it. It
// is a copy: the effect's declaration is not the caller's to mutate. An effect
// with no meter nodes returns an empty slice, never nil, so it always marshals
// as [] rather than null.
func (e *Effect) Readings() []dsp.Reading {
	out := make([]dsp.Reading, 0)
	if e.graph == nil {
		return out
	}
	for _, r := range e.graph.readings {
		out = append(out, r.reading())
	}

	return out
}

// Visuals returns the plots the script declared, in declaration order, or an
// empty slice when it declared none. The slices are copied so a caller cannot
// reach into the effect's declaration, and they are always non-nil so an empty
// list marshals as [] rather than null.
func (e *Effect) Visuals() []dsp.Visual {
	if e.graph == nil || len(e.graph.visuals) == 0 {
		return []dsp.Visual{}
	}
	out := make([]dsp.Visual, 0, len(e.graph.visuals))
	for _, src := range e.graph.visuals {
		v := *src
		if v.Params == nil {
			v.Params = []string{}
		}
		if v.Overlays == nil {
			v.Overlays = []string{}
		}
		v.Params = slices.Clone(v.Params)
		v.Overlays = slices.Clone(v.Overlays)
		out = append(out, v)
	}

	return out
}

// Watchdog returns the fault reason, empty when the callback is healthy. It
// exists so a host can surface why an effect went quiet instead of guessing.
func (e *Effect) Watchdog() string {
	if w := e.watchdog.Load(); w != nil && w.failed {
		return w.reason
	}

	return ""
}

// Reset clears every node's state and the meters, so a seek cannot carry a tail
// across it. It builds a fresh plan and publishes it whole, the same way Set
// does, so a concurrent Process reads either the old plan or the new one and
// never the plan being cleared.
func (e *Effect) Reset() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if st := e.state.Load(); st != nil {
		p, err := compile(e.graph, e.rate, e.ch)
		if err != nil {
			return err
		}
		if e.control != nil {
			e.control.bridge.replaceNodes(p.ctrl)
		}
		e.state.Store(&compiledState{
			plan:           p,
			params:         st.params,
			bypassed:       e.store.Bypassed(),
			watchdogFailed: e.faulted(),
			gains:          st.gains,
		})
	}
	e.meterIn.Reset()
	e.meterOut.Reset()

	return nil
}

// ClearFault clears a watchdog verdict so the effect can run again after a
// transient overrun. The fault is fail-closed by default: a host must ask for
// recovery explicitly, and until it does the effect stays bypassed. This is
// the documented way out of that state.
func (e *Effect) ClearFault() {
	e.watchdog.Store(nil)

	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.publishParamsLocked()
}

// Close stops the control goroutine and releases the instance's interpreter. A
// Tier 2 effect owns a goroutine and a Lua state, neither of which the engine's
// Effect contract knows how to free, so a host that discards an effect should
// call this. It is safe on a Tier 1 effect and safe to call twice.
func (e *Effect) Close() error {
	if e.control != nil {
		e.control.close()
	}
	e.lua.close()

	return nil
}

// publishLocked compiles a fresh plan and publishes it. The caller holds mu. It
// runs only from Configure, where node state is meant to start empty anyway.
func (e *Effect) publishLocked() error {
	if !e.isSet {
		return nil
	}
	p, err := compile(e.graph, e.rate, e.ch)
	if err != nil {
		return err
	}
	if e.control != nil {
		e.control.bridge.nodes = p.ctrl
	}
	e.state.Store(&compiledState{
		plan:           p,
		params:         paramValues(p, e.store),
		bypassed:       e.store.Bypassed(),
		watchdogFailed: e.faulted(),
		gains:          compileGains(e.store),
	})

	return nil
}

// publishParamsLocked recomputes the resolved parameter values and swaps in a
// snapshot that reuses the existing plan. The caller holds mu. Reusing the plan
// is what keeps a delay line and a filter memory alive across a slider move.
func (e *Effect) publishParamsLocked() error {
	st := e.state.Load()
	if st == nil {
		return nil
	}
	e.state.Store(&compiledState{
		plan:           st.plan,
		params:         paramValues(st.plan, e.store),
		bypassed:       e.store.Bypassed(),
		watchdogFailed: e.faulted(),
		gains:          compileGains(e.store),
	})

	return nil
}

// compileGains is the standard per-effect trim in linear form.
func compileGains(st *store) gainState {
	return gainState{
		in:  float32(dbToLinear(st.Float(dsp.ParamInputGain))),
		out: float32(dbToLinear(st.Float(dsp.ParamOutputGain))),
	}
}

// paramValues resolves the store into the ctrlValue form the audio side applies
// by copying floats. It includes both `param` signal nodes and every
// parameter-backed scalar field, and it runs on the control side, so the
// allocation it makes is off the audio path.
func paramValues(p *plan, st *store) []ctrlValue {
	if p == nil {
		return nil
	}
	var out []ctrlValue
	for _, n := range p.ctrl {
		if n == nil {
			continue
		}
		if n.kind == kindParam {
			out = append(out, ctrlValue{kind: ctrlParam, index: n.position, scalar: st.Float(paramKey(n))})

			continue
		}
		if n.kind == kindBiquad {
			if c, err := biquadCoeffs(n, st, p.rate); err == nil {
				out = append(out, ctrlValue{kind: ctrlBiquad, index: n.position, coeffs: c})
			}

			continue
		}
		if len(n.paramFields) == 0 {
			continue
		}
		vals := n.lit
		var onePole float64
		var hasOnePole bool
		for field, key := range n.paramFields {
			v := st.Float(key)
			switch field {
			case "rate":
				vals.rate = v / float64(p.rate)
			case "time":
				vals.time = v * float64(p.rate)
			case "drive":
				vals.drive = v
			case "threshold":
				vals.threshold = v
			case "ratio":
				vals.ratio = v
			case "knee":
				vals.knee = v
			case "factor":
				vals.factor = v
			case "cutoff":
				onePole = smoothCoeff(1.0/v, p.rate)
				hasOnePole = true
			case "attack":
				vals.attack = v
			case "release":
				vals.release = v
			}
		}
		out = append(out, ctrlValue{kind: ctrlVals, index: n.position, vals: vals})
		if hasOnePole {
			out = append(out, ctrlValue{kind: ctrlOnePole, index: n.position, scalar: onePole})
		}
	}

	return out
}

// biquadCoeffs reads a biquad's parameters from the store, filling the ones it
// did not read from the node's literals. It must produce the same coefficients
// as buildState's literal path, gain included.
func biquadCoeffs(n *node, st *store, rate int) ([5]float64, error) {
	freq := n.lit.freq
	if key, ok := n.paramFields["freq"]; ok {
		freq = st.Float(key)
	}
	if freq == 0 {
		freq = 1000
	}
	q := n.lit.q
	if key, ok := n.paramFields["q"]; ok {
		q = st.Float(key)
	}
	if q == 0 {
		q = 0.707
	}
	gain := n.lit.gain
	if key, ok := n.paramFields["gain"]; ok {
		gain = st.Float(key)
	}

	return designBiquad(n, freq, q, gain, rate)
}

func (e *Effect) faulted() bool {
	w := e.watchdog.Load()

	return w != nil && w.failed
}

// noteFailure records a watchdog fault and republishes the state, which is what
// actually silences the effect the next time Process runs. Recovery is explicit
// through ClearFault, so one transient overrun does not brick the session.
func (e *Effect) noteFailure(reason string) {
	e.watchdog.Store(&watchdogState{failed: true, reason: reason})

	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.publishParamsLocked()
}

func dbToLinear(db float64) float64 {
	if db == 0 {
		return 1
	}

	return math.Pow(10, db/20)
}
