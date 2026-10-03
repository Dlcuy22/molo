package dsp

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
)

// ErrUnknownParam means a key is not in an effect's schema. A preset loader
// leans on this: a mistyped key fails loudly instead of quietly doing nothing.
var ErrUnknownParam = errors.New("dsp: unknown parameter")

// Kind is the type of a parameter value. The set is small on purpose, because
// a parameter is something a UI can render as a slider, a switch or a list.
type Kind uint8

const (
	// Float is a continuous number, clamped to its range.
	Float Kind = iota
	// Int is a whole number, rounded and clamped to its range.
	Int
	// Bool is a two-state value.
	Bool
	// Enum is one of a fixed list of Options; a value outside it is rejected.
	Enum
)

// Widget names the control a UI should draw for a parameter. It is a display
// hint only: the value's Kind is what decides how it is validated and stored,
// so a UI that ignores Widget entirely still renders every parameter.
//
// Auto is the zero value and means "pick from Kind", which is what an effect
// that has no opinion should use.
type Widget string

const (
	// WidgetAuto lets the UI choose a control from the parameter's Kind.
	WidgetAuto Widget = ""
	// WidgetSlider is a horizontal range, the default for Float and Int.
	WidgetSlider Widget = "slider"
	// WidgetKnob is a rotary control, for a parameter a UI wants to group
	// tightly, such as one EQ band.
	WidgetKnob Widget = "knob"
	// WidgetSwitch is a two-state control for Bool.
	WidgetSwitch Widget = "switch"
	// WidgetSelect is a dropdown or segmented control for Enum.
	WidgetSelect Widget = "select"
)

// Param describes one editable setting.
//
// Min and Max bound Float and Int; an out-of-range number is clamped rather
// than rejected, which is how a bounded control behaves. An Enum is rejected
// when the value is not one of Options, because there is no nearest neighbour
// to clamp to. Step, Unit and Options the rest of the way are display
// metadata.
//
// Label, Group and Widget are the UI layer. A UI groups parameters by Group in
// the order they first appear, shows Label instead of the raw Key, and picks a
// control from Widget. All three are optional: an effect that sets none of
// them still renders, because the UI falls back to the Key and the Kind.
type Param struct {
	Key     string
	Kind    Kind
	Min     float64
	Max     float64
	Step    float64
	Unit    string
	Options []string
	Default any

	// Label is the human name a UI shows. Empty means the UI derives one from
	// Key.
	Label string
	// Group is the section a UI files the control under. Empty means the
	// effect's own unnamed section.
	Group string
	// Widget is the control to draw. WidgetAuto means pick from Kind.
	Widget Widget
}

// Values is a parameter set at the API edge. It is the shape a preset and a UI
// work with; an effect compiles it into the typed state its audio path reads.
type Values map[string]any

// ParamBypass is the parameter every effect carries to drop its processing.
// An effect must still accept Process while bypassed and pass audio through.
const ParamBypass = "bypass"

// ParamInputGain and ParamOutputGain are the standard per-effect gains in dB,
// applied before and after the effect's own processing. They exist so a preset
// maps one to one with the reference chain and so one stage can be trimmed
// without touching the master volume.
const (
	ParamInputGain  = "input-gain"
	ParamOutputGain = "output-gain"
)

// GroupGain is the section the standard gains live in. A UI pins this group so
// the input and output trim always sit together, above whatever sections the
// effect itself declares.
const GroupGain = "Gain"

func bypassParam() Param {
	return Param{Key: ParamBypass, Kind: Bool, Default: false, Label: "Bypass", Widget: WidgetSwitch}
}

func gainParam(key, label string) Param {
	return Param{
		Key: key, Kind: Float, Min: -36, Max: 36, Step: 0.1, Unit: "dB", Default: 0.0,
		Label: label, Group: GroupGain, Widget: WidgetSlider,
	}
}

// CommonParam reports whether key is one of the standard parameters every
// effect carries: bypass, input gain and output gain. A UI renders these as
// its fixed base controls rather than as effect-specific ones, so an effect
// cannot present them as optional or hide them.
func CommonParam(key string) bool {
	switch key {
	case ParamBypass, ParamInputGain, ParamOutputGain:
		return true
	default:
		return false
	}
}

// StandardParamKeys lists the standard parameter keys in display order. It is
// the single source a caller iterates to pin the base controls, so it cannot
// drift from StandardParams.
func StandardParamKeys() []string {
	return []string{ParamBypass, ParamInputGain, ParamOutputGain}
}

// Standard meter keys. An effect that implements Metered reports levels under
// these names, so a UI can draw a fixed input/output pair without knowing which
// effect it is looking at. A missing key means "this effect does not measure
// it", never "zero".
const (
	MeterIn  = "in"
	MeterOut = "out"
)

// withCommon returns a schema with the standard parameters in front: bypass,
// input gain and output gain. Every effect factory uses it, so the three are
// present and identical everywhere a UI or a preset looks.
func withCommon(schema []Param) []Param {
	out := make([]Param, 0, len(schema)+3)
	out = append(out, StandardParams()...)

	return append(out, schema...)
}

// StandardParams returns the three parameters every effect carries, in the
// order a UI shows them: bypass, input gain, output gain. It is exported
// because an effect built outside this package, such as a scripted one, has to
// prepend the same three and must not invent its own copy of them.
func StandardParams() []Param {
	return []Param{
		bypassParam(),
		gainParam(ParamInputGain, "Input Gain"),
		gainParam(ParamOutputGain, "Output Gain"),
	}
}

// cloneParams copies a schema so a caller cannot mutate the one an effect
// holds. Options is cloned too, because a shared backing array would leak the
// same way.
func cloneParams(in []Param) []Param {
	out := make([]Param, len(in))
	for i, p := range in {
		out[i] = p
		out[i].Options = slices.Clone(p.Options)
	}

	return out
}

// coerce validates one value against a parameter and returns the value to
// store. A number is clamped into range and an Enum is checked against its
// options; anything else is returned as an error rather than guessed at.
func (p Param) coerce(value any) (any, error) {
	switch p.Kind {
	case Bool:
		b, ok := value.(bool)
		if !ok {
			return nil, badValue(p, value, "bool")
		}

		return b, nil

	case Int:
		f, ok := asFloat(value)
		if !ok {
			return nil, badValue(p, value, "number")
		}
		if !isFinite(f) {
			return nil, badValue(p, value, "finite number")
		}

		return int(math.Round(clampToRange(f, p.Min, p.Max))), nil

	case Float:
		f, ok := asFloat(value)
		if !ok {
			return nil, badValue(p, value, "number")
		}
		if !isFinite(f) {
			return nil, badValue(p, value, "finite number")
		}

		return clampToRange(f, p.Min, p.Max), nil

	case Enum:
		s, ok := value.(string)
		if !ok {
			return nil, badValue(p, value, "string")
		}
		if !slices.Contains(p.Options, s) {
			return nil, fmt.Errorf("%w: %q is not one of %v for %q", ErrUnknownParam, s, p.Options, p.Key)
		}

		return s, nil

	default:
		return nil, fmt.Errorf("%w: %q has no usable kind", ErrUnknownParam, p.Key)
	}
}

func (p Param) checkSchema() error {
	if p.Key == "" {
		return errors.New("dsp: parameter has no key")
	}
	switch p.Kind {
	case Float, Int:
		if p.Min > p.Max {
			return fmt.Errorf("dsp: %q has min %v above max %v", p.Key, p.Min, p.Max)
		}
	case Bool, Enum:
	default:
		return fmt.Errorf("dsp: %q has unknown kind %d", p.Key, p.Kind)
	}

	return nil
}

// asFloat reads the numeric types a caller can plausibly supply. JSON decodes
// a number to float64, Go callers usually hold an int or a float64.
func asFloat(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	}

	return 0, false
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

func clampToRange(f, lo, hi float64) float64 {
	return math.Min(math.Max(f, lo), hi)
}

func badValue(p Param, value any, want string) error {
	return fmt.Errorf("%w: %q wants a %s, got %T", ErrUnknownParam, p.Key, want, value)
}

// paramStore is the control-side copy of an effect's settings. The audio path
// never reads it: Set runs on whichever goroutine a UI or a preset loader
// owns, and the effect compiles its own immutable state from it.
type paramStore struct {
	mu     sync.Mutex
	schema []Param
	index  map[string]struct{}
	values Values
}

// newParamStore validates values against schema and fills every default. It
// rejects a key the schema does not contain, so a preset with a typo is
// reported instead of half-applied.
func newParamStore(schema []Param, values Values) (*paramStore, error) {
	s := &paramStore{
		schema: cloneParams(schema),
		index:  make(map[string]struct{}, len(schema)),
		values: make(Values, len(schema)),
	}
	for _, p := range s.schema {
		if err := p.checkSchema(); err != nil {
			return nil, err
		}
		if _, dup := s.index[p.Key]; dup {
			return nil, fmt.Errorf("dsp: duplicate parameter %q", p.Key)
		}
		s.index[p.Key] = struct{}{}
	}
	for key := range values {
		if _, ok := s.index[key]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownParam, key)
		}
	}
	for _, p := range s.schema {
		v := p.Default
		if given, ok := values[p.Key]; ok && given != nil {
			v = given
		}
		coerced, err := p.coerce(v)
		if err != nil {
			return nil, err
		}
		s.values[p.Key] = coerced
	}

	return s, nil
}

func (s *paramStore) Schema() []Param {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneParams(s.schema)
}

func (s *paramStore) Get(key string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[key]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownParam, key)
	}

	return s.values[key], nil
}

func (s *paramStore) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[key]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownParam, key)
	}
	for _, p := range s.schema {
		if p.Key != key {
			continue
		}
		coerced, err := p.coerce(value)
		if err != nil {
			return err
		}
		s.values[key] = coerced

		return nil
	}

	return fmt.Errorf("%w: %q", ErrUnknownParam, key)
}

func (s *paramStore) Values() Values {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(Values, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}

	return out
}

// Float reads a numeric parameter. Callers pass a key they declared in their
// schema, so a missing one yields zero rather than an error.
func (s *paramStore) Float(key string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, _ := asFloat(s.values[key])

	return f
}

// Bool reads a boolean parameter, defaulting to false when it is absent.
func (s *paramStore) Bool(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := s.values[key].(bool)

	return b
}

// Bypassed reports the standard bypass parameter.
func (s *paramStore) Bypassed() bool { return s.Bool(ParamBypass) }
