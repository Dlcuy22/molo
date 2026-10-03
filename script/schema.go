package script

import (
	"fmt"
	"math"

	"github.com/dlcuy22/player/dsp"
)

// numericKind reports whether a declared kind word names a number. It is the
// one place the float/int aliases live, so a build-time check that reads a kind
// before the schema exists cannot drift from buildParam's switch.
func numericKind(kind string) bool {
	switch lowerName(kind) {
	case "float", "number", "int", "integer":
		return true
	default:
		return false
	}
}

// buildSchema turns Lua parameter declarations into a dsp schema, with the
// three standard parameters in front.
//
// The standard three come from dsp.StandardParams, the one definition a UI and
// a preset compare across effects. Reproducing them here would let a scripted
// input gain drift from a built-in one, which is a visible inconsistency.
func buildSchema(decls []paramDecl) ([]dsp.Param, error) {
	standard := dsp.StandardParams()
	out := make([]dsp.Param, 0, len(decls)+len(standard))
	out = append(out, standard...)
	// A script parameter that collides with one of the standard keys is caught
	// here, at load, rather than by newStore at the first New: a Load that
	// succeeds but whose every instance fails to build is the worst report.
	standardKeys := make(map[string]struct{}, len(standard))
	for _, p := range standard {
		standardKeys[p.Key] = struct{}{}
	}
	for _, d := range decls {
		if _, ok := standardKeys[d.Key]; ok {
			return nil, fmt.Errorf("script: parameter %q collides with a standard parameter", d.Key)
		}
		p, err := buildParam(d)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	for _, p := range out {
		if err := checkSchema(p); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// buildParam maps one declaration. A script may name any subset of the UI
// fields; anything it omits falls back the way dsp would: an empty Label and
// Group so the UI derives them from the Key, and WidgetAuto so the UI picks
// from the Kind.
func buildParam(d paramDecl) (dsp.Param, error) {
	switch lowerName(d.Kind) {
	case "float", "number":
		lo, hi := d.Min, d.Max
		if !d.hasRange() {
			lo, hi = 0, 1
		}
		if !isFinite(lo) || !isFinite(hi) || lo > hi {
			return dsp.Param{}, fmt.Errorf("script: %q has an invalid range [%v, %v]", d.Key, lo, hi)
		}
		p := dsp.Param{Key: d.Key, Kind: dsp.Float, Min: lo, Max: hi, Step: d.Step, Unit: d.Unit, Label: d.Label, Group: d.Group}
		p.Widget = widgetFor(d.Widget, dsp.WidgetSlider)
		p.Default = orNumber(d.Default, lo)

		return p, nil

	case "int", "integer":
		lo, hi := d.Min, d.Max
		if !d.hasRange() {
			lo, hi = 0, 1
		}
		if !isFinite(lo) || !isFinite(hi) || lo > hi {
			return dsp.Param{}, fmt.Errorf("script: %q has an invalid range [%v, %v]", d.Key, lo, hi)
		}
		p := dsp.Param{Key: d.Key, Kind: dsp.Int, Min: math.Round(lo), Max: math.Round(hi), Step: d.Step, Unit: d.Unit, Label: d.Label, Group: d.Group}
		p.Widget = widgetFor(d.Widget, dsp.WidgetSlider)
		p.Default = int(math.Round(orNumber(d.Default, lo)))

		return p, nil

	case "bool", "boolean":
		p := dsp.Param{Key: d.Key, Kind: dsp.Bool, Label: d.Label, Group: d.Group}
		p.Widget = widgetFor(d.Widget, dsp.WidgetSwitch)
		p.Default = false
		if b, ok := d.Default.(bool); ok {
			p.Default = b
		}

		return p, nil

	case "enum", "option":
		if len(d.Options) == 0 {
			return dsp.Param{}, fmt.Errorf("script: enum %q has no options", d.Key)
		}
		p := dsp.Param{Key: d.Key, Kind: dsp.Enum, Options: d.Options, Label: d.Label, Group: d.Group}
		p.Widget = widgetFor(d.Widget, dsp.WidgetSelect)
		def := d.Options[0]
		if s, ok := d.Default.(string); ok && contains(d.Options, s) {
			def = s
		}
		p.Default = def

		return p, nil

	default:
		return dsp.Param{}, fmt.Errorf("script: %q has unknown kind %q", d.Key, d.Kind)
	}
}

// widgetFor honours a requested widget and falls back to the Kind's default,
// which is what WidgetAuto means.
func widgetFor(name string, fallback dsp.Widget) dsp.Widget {
	switch dsp.Widget(lowerName(name)) {
	case dsp.WidgetSlider, dsp.WidgetKnob, dsp.WidgetSwitch, dsp.WidgetSelect:
		return dsp.Widget(lowerName(name))
	default:
		return fallback
	}
}

func orNumber(v any, fallback float64) float64 {
	if f, ok := asFloat(v); ok && isFinite(f) {
		return f
	}

	return fallback
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}
