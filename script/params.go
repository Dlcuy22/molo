package script

import (
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/dlcuy22/molo/dsp"
)

// paramDecl is one parameter a Lua script declared, in the shape the Lua front
// end produces and the schema builder consumes. Keeping it separate from
// dsp.Param means a script can leave a field out and let the defaults here fill
// it, while dsp.Param stays a fully spelled-out value.
type paramDecl struct {
	Key     string
	Kind    string // "float", "int", "bool", "enum"
	Min     float64
	Max     float64
	Step    float64
	Unit    string
	Options []string
	Default any
	Label   string
	Group   string
	Widget  string
}

// hasRange reports whether the declaration supplied a numeric bound. A script
// that omits both gets a wide default, which is friendlier than a zero range
// that clamps everything away.
func (d paramDecl) hasRange() bool { return d.Min != 0 || d.Max != 0 }

// store is the control-side parameter set. It is a deliberate reimplementation
// of dsp.paramStore's behaviour, not a copy of its code: dsp exposes no
// validator, so a script package that wants the same rules has to own them.
// The rules are the same because the UI and presets depend on them: a number
// clamps into its range, an enum must be one of its options, an unknown key is
// an error, and every declared default is filled.
type store struct {
	mu     sync.Mutex
	schema []dsp.Param
	index  map[string]int
	values map[string]any
}

// newStore validates a schema and a value set together, the way
// dsp.newParamStore does.
func newStore(schema []dsp.Param, values map[string]any) (*store, error) {
	s := &store{
		schema: cloneSchema(schema),
		index:  make(map[string]int, len(schema)),
		values: make(map[string]any, len(schema)),
	}
	for i, p := range s.schema {
		if err := checkSchema(p); err != nil {
			return nil, err
		}
		if _, dup := s.index[p.Key]; dup {
			return nil, fmt.Errorf("script: duplicate parameter %q", p.Key)
		}
		s.index[p.Key] = i
	}
	for key := range values {
		if _, ok := s.index[key]; !ok {
			return nil, fmt.Errorf("%w: %q", dsp.ErrUnknownParam, key)
		}
	}
	for _, p := range s.schema {
		v := p.Default
		if given, ok := values[p.Key]; ok && given != nil {
			v = given
		}
		coerced, err := coerce(p, v)
		if err != nil {
			return nil, err
		}
		s.values[p.Key] = coerced
	}

	return s, nil
}

func (s *store) Schema() []dsp.Param {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneSchema(s.schema)
}

func (s *store) Get(key string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[key]; !ok {
		return nil, fmt.Errorf("%w: %q", dsp.ErrUnknownParam, key)
	}

	return s.values[key], nil
}

func (s *store) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.index[key]
	if !ok {
		return fmt.Errorf("%w: %q", dsp.ErrUnknownParam, key)
	}
	coerced, err := coerce(s.schema[i], value)
	if err != nil {
		return err
	}
	s.values[key] = coerced

	return nil
}

// Float reads a numeric parameter without an error, because the caller declared
// the key in its own schema.
func (s *store) Float(key string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, _ := asFloat(s.values[key])

	return f
}

// Bool reads a boolean parameter, defaulting to false.
func (s *store) Bool(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := s.values[key].(bool)

	return b
}

// Bypassed reports the standard bypass parameter.
func (s *store) Bypassed() bool { return s.Bool(dsp.ParamBypass) }

// snapshot copies every current value, which is what the control callback is
// handed. It runs on the control side, so allocation is fine here.
func (s *store) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}

	return out
}

// coerce validates one value against a parameter. It mirrors dsp's unexported
// rule set: a number clamps, an enum is checked, a wrong type is an error.
func coerce(p dsp.Param, value any) (any, error) {
	switch p.Kind {
	case dsp.Bool:
		b, ok := value.(bool)
		if !ok {
			return nil, badValue(p, value, "bool")
		}

		return b, nil

	case dsp.Int:
		f, ok := asFloat(value)
		if !ok || !isFinite(f) {
			return nil, badValue(p, value, "finite number")
		}

		return int(math.Round(clamp(f, p.Min, p.Max))), nil

	case dsp.Float:
		f, ok := asFloat(value)
		if !ok || !isFinite(f) {
			return nil, badValue(p, value, "finite number")
		}

		return clamp(f, p.Min, p.Max), nil

	case dsp.Enum:
		s, ok := value.(string)
		if !ok {
			return nil, badValue(p, value, "string")
		}
		if !slices.Contains(p.Options, s) {
			return nil, fmt.Errorf("%w: %q is not one of %v for %q", dsp.ErrUnknownParam, s, p.Options, p.Key)
		}

		return s, nil

	default:
		return nil, fmt.Errorf("%w: %q has no usable kind", dsp.ErrUnknownParam, p.Key)
	}
}

// checkSchema mirrors dsp.Param.checkSchema.
func checkSchema(p dsp.Param) error {
	if p.Key == "" {
		return fmt.Errorf("script: parameter has no key")
	}
	switch p.Kind {
	case dsp.Float, dsp.Int:
		if p.Min > p.Max {
			return fmt.Errorf("script: %q has min %v above max %v", p.Key, p.Min, p.Max)
		}
		if !isFinite(p.Min) || !isFinite(p.Max) {
			return fmt.Errorf("script: %q has a non-finite range", p.Key)
		}
	case dsp.Bool, dsp.Enum:
	default:
		return fmt.Errorf("script: %q has unknown kind %d", p.Key, p.Kind)
	}

	return nil
}

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
	default:
		return 0, false
	}
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func clamp(f, lo, hi float64) float64 { return math.Min(math.Max(f, lo), hi) }

func badValue(p dsp.Param, value any, want string) error {
	return fmt.Errorf("%w: %q wants a %s, got %T", dsp.ErrUnknownParam, p.Key, want, value)
}

// cloneSchema copies a schema so a caller cannot mutate the one the effect
// holds; Options is cloned too for the same reason.
func cloneSchema(in []dsp.Param) []dsp.Param {
	out := make([]dsp.Param, len(in))
	for i, p := range in {
		out[i] = p
		out[i].Options = slices.Clone(p.Options)
	}

	return out
}
