package script

import (
	"fmt"
	"strings"

	lua "github.com/yuin/gopher-lua"

	"github.com/dlcuy22/molo/dsp"
)

// factory is what Load returns: a dsp.Factory that builds one scripted effect.
// It is the integration point, so the engine treats a script exactly like a
// built-in implementation.
//
// The factory keeps the script source, not a built graph. Every New call runs
// the script again in its own sandbox, which is what gives each instance its
// own nodes and, for a Tier 2 script, its own interpreter. A shared
// interpreter would make two instances race on it.
type factory struct {
	name    string
	schema  []dsp.Param
	source  []byte
	path    string
	control bool
}

var _ dsp.Factory = (*factory)(nil)

// Kind is the pipeline slot. Every script shares the one kind, so a UI groups
// them together and picks an implementation by name.
func (f *factory) Kind() string { return "script" }

// Impl is the script's own name, the value that selects it out of the kind.
func (f *factory) Impl() string { return f.name }

// FriendlyName is the label a UI shows.
func (f *factory) FriendlyName() string { return f.name }

// Weight is a formality: scripts compete with each other, not with the built-in
// kinds, and the pair (kind, impl) is unique per script.
func (f *factory) Weight() int { return 50 }

// Placement is Post by default. A script describes a per-sample effect, which
// belongs on the real-time path like every other stage.
func (f *factory) Placement() dsp.Placement { return dsp.Post }

// Schema returns the parameter set, standard parameters included.
func (f *factory) Schema() []dsp.Param { return cloneSchema(f.schema) }

// New builds an effect from a value set. Missing keys take their defaults and
// an unknown key is an error, exactly as dsp.paramStore does.
func (f *factory) New(values dsp.Values) (dsp.Effect, error) {
	v := make(map[string]any, len(values))
	for k, val := range values {
		v[k] = val
	}
	st, err := newStore(f.schema, v)
	if err != nil {
		return nil, err
	}

	// Run the script for this instance. Its graph and interpreter belong to
	// this effect alone.
	def, L, err := instantiate(f.source, f.chunkName())
	if err != nil {
		return nil, err
	}
	e := &Effect{
		store: st,
		name:  f.name,
		graph: def.graph,
	}
	if def.control == nil {
		L.Close()
	} else {
		runner, err := newControlRunner(&luaState{state: L})
		if err != nil {
			L.Close()

			return nil, err
		}
		e.control = runner
		e.lua = &luaState{state: L}
		e.control.bridge.nodes = def.graph.nodes
		for _, n := range def.graph.nodes {
			n.sink = e.control.bridge
		}
	}

	return e, nil
}

// chunkName names the script in an error report.
func (f *factory) chunkName() string {
	if f.path != "" {
		return f.path
	}

	return f.name
}

// String names the factory for diagnostics.
func (f *factory) String() string { return fmt.Sprintf("script %q", strings.TrimSpace(f.name)) }

// luaState bundles the interpreter an effect owns, so closing it is one call
// and the ownership is explicit.
type luaState struct {
	state *lua.LState
}

func (s *luaState) close() {
	if s != nil && s.state != nil {
		s.state.Close()
		s.state = nil
	}
}
