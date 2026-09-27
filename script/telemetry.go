package script

import (
	"fmt"
	"math"
	"sync/atomic"

	lua "github.com/yuin/gopher-lua"

	"github.com/dlcuy22/player/dsp"
)

// Telemetry is what a script says about what its effect is doing, as opposed to
// what it is (params) or how loud it is (the standard in/out pair). A meter node
// publishes one named reading; ctx.visual declares a plot the UI draws from the
// effect's parameters. Both are graph-level declarations compiled into the plan,
// and neither runs Lua on the audio thread.

// readingDecl is one meter declaration, in the shape the Lua front end produces
// and the graph keeps. It is static metadata; the number it describes lives in
// the meter node's published value.
type readingDecl struct {
	Key   string
	Label string
	Unit  string
	Min   float64
	Max   float64
	Kind  dsp.ReadingKind
}

// reading returns the dsp form. It is a value copy, so a caller cannot reach
// back into the graph.
func (d readingDecl) reading() dsp.Reading {
	return dsp.Reading{
		Key:   d.Key,
		Label: d.Label,
		Unit:  d.Unit,
		Min:   d.Min,
		Max:   d.Max,
		Kind:  d.Kind,
	}
}

// parseReadingKind maps a script's kind word to a dsp reading kind. An unknown
// word is a load error rather than a silent scalar.
func parseReadingKind(name string) (dsp.ReadingKind, bool) {
	switch dsp.ReadingKind(lowerName(name)) {
	case dsp.ReadingLevel:
		return dsp.ReadingLevel, true
	case dsp.ReadingGainReduction:
		return dsp.ReadingGainReduction, true
	case dsp.ReadingScalar:
		return dsp.ReadingScalar, true
	default:
		return "", false
	}
}

// parseVisualKind maps a script's kind word to a dsp visual kind.
func parseVisualKind(name string) (dsp.VisualKind, bool) {
	switch dsp.VisualKind(lowerName(name)) {
	case dsp.VisualTransfer:
		return dsp.VisualTransfer, true
	case dsp.VisualGainReduction:
		return dsp.VisualGainReduction, true
	case dsp.VisualDynamics:
		return dsp.VisualDynamics, true
	default:
		return "", false
	}
}

// meterValue is one published reading. The audio thread stores the block's
// sample in store; a control goroutine reads it in load. It is the same
// lock-free handoff dsp.Meter uses, but a plain signed float rather than a
// magnitude: a gain reduction of -6 dB is not 6, and dsp.Meter's peak is the
// absolute value.
type meterValue struct {
	bits atomic.Uint32
}

// store publishes one sample. A non-finite value becomes silence for the same
// reason a meter's does: one NaN would otherwise stick for the session.
func (m *meterValue) store(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		v = 0
	}
	m.bits.Store(math.Float32bits(float32(v)))
}

func (m *meterValue) load() float64 {
	return float64(math.Float32frombits(m.bits.Load()))
}

// checkOverlays validates every overlay a visual named against the readings the
// graph declares. The standard in/out pair is a reading too: it is always
// published under dsp.MeterIn/dsp.MeterOut, so a visual may draw it without a
// meter node of its own.
func checkOverlays(g *graph) error {
	if len(g.visuals) == 0 {
		return nil
	}
	known := map[string]struct{}{
		dsp.MeterIn:  {},
		dsp.MeterOut: {},
	}
	for _, r := range g.readings {
		known[r.Key] = struct{}{}
	}
	for _, v := range g.visuals {
		for _, name := range v.Overlays {
			if _, ok := known[name]; !ok {
				return fmt.Errorf("script: visual overlay %q is not a declared reading", name)
			}
		}
	}

	return nil
}

// stringList reads a Lua array of names. A nil value is an empty list; a
// non-table or a non-string entry is an error, because silently dropping a name
// would hide a typo in a params or overlays list. A table with string keys is
// rejected too: `{ bogus = "nope" }` has length zero, so reading only the array
// part would accept it as an empty list and hide the typo.
func stringList(v lua.LValue, what string) ([]string, error) {
	switch v.Type() {
	case lua.LTNil:
		return nil, nil
	case lua.LTTable:
	default:
		return nil, fmt.Errorf("script: visual %s must be a list of names", what)
	}
	tbl := v.(*lua.LTable)
	var keyErr error
	tbl.ForEach(func(key, _ lua.LValue) {
		if keyErr != nil {
			return
		}
		if _, ok := key.(lua.LString); ok {
			keyErr = fmt.Errorf("script: visual %s must be a list, not a table with string keys", what)
		}
	})
	if keyErr != nil {
		return nil, keyErr
	}
	n := tbl.Len()
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		name, ok := tbl.RawGetInt(i).(lua.LString)
		if !ok {
			return nil, fmt.Errorf("script: visual %s entry %d is not a name", what, i)
		}
		out = append(out, string(name))
	}

	return out, nil
}
