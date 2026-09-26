package script

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/dlcuy22/player/dsp"
)

// The Lua front end is deliberately a binder, not an evaluator. It runs the
// script once, walks the graph description the script's build function
// produced, and hands the result to the Go compiler. It never runs on the audio
// thread and is never called again after load for a Tier 1 script.
//
// The sandbox is the other half of this file. A script is untrusted, so the
// standard libraries that reach outside the process are removed before the
// script runs, and a script that tries to use one fails to load rather than
// quietly doing something.

// sandboxLibs are the standard libraries a script may not have. Removing the
// loaders as well as the tables means a script cannot require or package.loadlib
// its way back to them.
var sandboxLibs = []string{"os", "io", "debug", "package", "ffi"}

// scriptDef is what a script's top-level return produced, before compilation.
type scriptDef struct {
	name    string
	build   *lua.LFunction
	control *lua.LFunction
	graph   *graph
}

// Load reads one Lua file and returns the factory that builds its effect.
// Every failure is an error: a bad script never panics and never registers a
// half-built effect.
func Load(path string) (dsp.Factory, error) {
	f, err := parseFile(path)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// LoadDir reads every .lua file in dir, in name order, and returns one factory
// per script. A bad script fails the whole load with the file named, which is
// what a registration path wants: a half-loaded directory is harder to reason
// about than a failed one. Two scripts with the same effect name are rejected
// here: dsp's registry keeps only the last of a duplicate (kind, impl), so a
// silent collision would hide one effect behind the other.
func LoadDir(dir string) ([]dsp.Factory, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.lua"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	out := make([]dsp.Factory, 0, len(paths))
	byName := make(map[string]string, len(paths))
	for _, path := range paths {
		f, err := parseFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if prev, ok := byName[f.Impl()]; ok {
			return nil, fmt.Errorf("script: %s and %s both declare effect %q", prev, path, f.Impl())
		}
		byName[f.Impl()] = path
		out = append(out, f)
	}

	return out, nil
}

// Register adds a script factory to dsp's default registry. It is the script
// equivalent of dsp.Register with the one check the registry does not do: two
// scripts that share an effect name would collide on the (kind, impl) pair, so
// the second registration is refused instead of shadowing the first.
func Register(f dsp.Factory) error {
	if f == nil {
		return fmt.Errorf("script: Register requires a factory")
	}
	for _, existing := range dsp.Default.Impls() {
		if existing.Kind() == f.Kind() && existing.Impl() == f.Impl() {
			return fmt.Errorf("script: effect %q is already registered", f.Impl())
		}
	}
	dsp.Default.Register(f)

	return nil
}

// parseFile loads a script, validates it by building it once, and returns a
// factory that can build instances. The source is kept because every instance
// gets its own sandbox: a control callback needs its own interpreter, and two
// instances of one script must never share one.
func parseFile(path string) (*factory, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("script: %w", err)
	}
	f, err := newFactory(source, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	f.path = path

	return f, nil
}

// newFactory validates a script by instantiating it once, then keeps the source
// for later instances.
func newFactory(source []byte, chunkName string) (*factory, error) {
	def, L, err := instantiate(source, chunkName)
	if err != nil {
		return nil, err
	}
	schema, err := buildSchema(def.graph.params)
	if err != nil {
		L.Close()

		return nil, err
	}
	if err := checkParamRefs(def, schema); err != nil {
		L.Close()

		return nil, err
	}
	// The validation interpreter is not needed: each instance gets its own.
	L.Close()

	return &factory{
		name:    def.name,
		schema:  schema,
		source:  source,
		control: def.control != nil,
	}, nil
}

// instantiate runs a script's chunk in a fresh sandbox and builds its graph
// description. The caller owns the returned interpreter and must close it when
// the instance is gone. Both the chunk and build(ctx) run under a context
// deadline, so a script that loops at top level or in build fails to load
// instead of hanging Register.
func instantiate(source []byte, chunkName string) (*scriptDef, *lua.LState, error) {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})

	if err := openSandbox(L); err != nil {
		L.Close()

		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), loadDeadline)
	defer cancel()
	L.SetContext(ctx)

	fn, err := L.Load(strings.NewReader(string(source)), chunkName)
	if err != nil {
		L.Close()

		return nil, nil, fmt.Errorf("script: %s: %w", chunkName, err)
	}
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		L.Close()

		return nil, nil, fmt.Errorf("script: %s: %w", chunkName, loadError(ctx, err))
	}
	def, err := readDefinition(L)
	if err != nil {
		L.Close()

		return nil, nil, err
	}
	if err := def.buildGraph(L, ctx); err != nil {
		L.Close()

		return nil, nil, err
	}
	if def.control != nil {
		// The callback has to outlive the loader; the instance recovers it from
		// this global.
		L.SetGlobal(controlGlobal, def.control)
	}
	// The load context does not follow the interpreter into the instance: the
	// control goroutine installs its own per-invocation deadline.
	L.RemoveContext()

	return def, L, nil
}

// loadDeadline bounds the load chunk and build(ctx). It is generous for a
// script that describes a graph and finite for one that never returns.
const loadDeadline = 500 * time.Millisecond

// loadError rewrites a context abort into the loader's vocabulary, so a script
// that loops forever reads as a timeout rather than an interpreter error.
func loadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("script did not finish within %s and was interrupted", loadDeadline)
	}

	return err
}

// openSandbox installs the safe part of the standard library and removes the
// rest. Only the pure libraries are opened: base, table, string, math. The
// libraries that touch the host (os, io, debug, package, coroutine's channel)
// are never opened at all, and the base library's remaining escape hatches are
// removed by name.
func openSandbox(L *lua.LState) error {
	for name, open := range map[string]lua.LGFunction{
		"base":   lua.OpenBase,
		"table":  lua.OpenTable,
		"string": lua.OpenString,
		"math":   lua.OpenMath,
	} {
		if err := L.CallByParam(lua.P{Fn: L.NewFunction(open), NRet: 0, Protect: true}); err != nil {
			return fmt.Errorf("script: open library %s: %w", name, err)
		}
	}
	for _, name := range sandboxLibs {
		L.SetGlobal(name, lua.LNil)
	}

	// The base library's escape hatches are removed one by one: they run code,
	// load code, or reach the host, and none of that belongs in a script.
	for _, name := range []string{"dofile", "loadfile", "load", "loadstring", "require", "collectgarbage", "module", "print"} {
		L.SetGlobal(name, lua.LNil)
	}

	// `effect{...}` is the one word the script vocabulary adds to the globals.
	// It only hands its table back; the loader validates what is in it.
	L.SetGlobal("effect", L.NewFunction(func(L *lua.LState) int {
		L.Push(L.CheckTable(1))

		return 1
	}))

	// Parameter kinds are plain words in a params table, so they are bound as
	// globals. The values are strings, which readParam maps to a dsp kind.
	for _, kind := range []string{"float", "int", "bool", "enum", "number", "integer", "boolean", "option"} {
		L.SetGlobal(kind, lua.LString(kind))
	}

	return nil
}

// readDefinition pulls the effect table out of the chunk's return value.
func readDefinition(L *lua.LState) (*scriptDef, error) {
	top := L.GetTop()
	if top == 0 {
		return nil, fmt.Errorf("script: chunk returned nothing, want effect{...}")
	}
	tbl, ok := L.Get(-1).(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("script: chunk returned %s, want a table", L.Get(-1).Type().String())
	}
	name := lua.LVAsString(tbl.RawGetString("name"))
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("script: effect has no name")
	}
	build, ok := tbl.RawGetString("build").(*lua.LFunction)
	if !ok {
		return nil, fmt.Errorf("script: effect %q has no build function", name)
	}

	def := &scriptDef{name: name, build: build, graph: newGraph()}
	if err := readParams(def, tbl); err != nil {
		return nil, err
	}

	return def, nil
}

// readParams reads the params table. Each entry is a table whose first element
// selects the kind and whose named fields carry the UI metadata.
//
// Entries are read in sorted key order. A Lua table has no order for its string
// keys and gopher-lua stores them in a Go map, whose iteration order is
// randomised per run, so reading the table directly would reorder the schema on
// every load. A script that wants a natural order pads its keys to sort that
// way, which is how the bundled EQ keeps its sliders in frequency order.
func readParams(def *scriptDef, tbl *lua.LTable) error {
	params, ok := tbl.RawGetString("params").(*lua.LTable)
	if !ok {
		return nil
	}
	keys := make([]string, 0, 8)
	params.ForEach(func(key, _ lua.LValue) {
		if k, ok := key.(lua.LString); ok {
			keys = append(keys, string(k))
		}
	})
	sort.Strings(keys)
	for _, k := range keys {
		entry, ok := params.RawGetString(k).(*lua.LTable)
		if !ok {
			return fmt.Errorf("script: parameter %q must be a table", k)
		}
		decl, err := readParam(k, entry)
		if err != nil {
			return err
		}
		if err := def.graph.addParam(decl); err != nil {
			return err
		}
	}

	return nil
}

// readParam maps one parameter table. The kind may be given positionally, as in
// `{ float, min = 0 }`, or by name in a `kind` field.
func readParam(key string, entry *lua.LTable) (paramDecl, error) {
	decl := paramDecl{Key: key}
	if v := entry.RawGetInt(1); v != lua.LNil {
		decl.Kind = lua.LVAsString(v)
	}
	if v := entry.RawGetString("kind"); v != lua.LNil {
		decl.Kind = lua.LVAsString(v)
	}
	if decl.Kind == "" {
		return decl, fmt.Errorf("script: parameter %q has no kind", key)
	}
	if v, ok := entry.RawGetString("min").(lua.LNumber); ok {
		decl.Min = float64(v)
	}
	if v, ok := entry.RawGetString("max").(lua.LNumber); ok {
		decl.Max = float64(v)
	}
	if v, ok := entry.RawGetString("step").(lua.LNumber); ok {
		decl.Step = float64(v)
	}
	decl.Unit = lua.LVAsString(entry.RawGetString("unit"))
	if v := entry.RawGetString("label"); v != lua.LNil {
		decl.Label = lua.LVAsString(v)
	}
	if v := entry.RawGetString("group"); v != lua.LNil {
		decl.Group = lua.LVAsString(v)
	}
	if v := entry.RawGetString("widget"); v != lua.LNil {
		decl.Widget = lua.LVAsString(v)
	}
	if opts, ok := entry.RawGetString("options").(*lua.LTable); ok {
		var out []string
		opts.ForEach(func(_, o lua.LValue) {
			out = append(out, lua.LVAsString(o))
		})
		decl.Options = out
	}
	if v := entry.RawGetString("default"); v != lua.LNil {
		decl.Default = toGoValue(v)
	}
	for _, field := range []string{"Min", "Max", "Step"} {
		if err := validateDeclNumber(key, field, decl); err != nil {
			return decl, err
		}
	}

	return decl, nil
}

func validateDeclNumber(key, field string, d paramDecl) error {
	vals := map[string]float64{"Min": d.Min, "Max": d.Max, "Step": d.Step}
	if !isFinite(vals[field]) {
		return fmt.Errorf("script: parameter %q has a non-finite %s", key, field)
	}

	return nil
}

// buildGraph runs build(ctx) and records the graph it described. This is the
// one place the script executes, and even here it is generating Go data
// structures, not computing samples. The context is the loader's deadline: a
// build that never returns is interrupted, not a startup hang.
func (d *scriptDef) buildGraph(L *lua.LState, ctx context.Context) error {
	b := &builder{L: L, g: d.graph}
	c := b.newContext()
	L.Push(d.build)
	L.Push(c)
	if err := L.PCall(1, 0, nil); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("script: build did not finish within %s and was interrupted", loadDeadline)
		}

		return fmt.Errorf("script: build: %w", err)
	}
	if b2, ok := b.g.control.(*lua.LFunction); ok {
		d.control = b2
	}
	if !b.outSet {
		return fmt.Errorf("script: build did not call ctx.out")
	}
	// A required input that build never connected is a script mistake, and a
	// load error is the right place to report it: doing it here also spares the
	// author a Configure failure that only shows the declaration index.
	for _, n := range d.graph.nodes {
		if n.spec == nil {
			continue
		}
		for _, in := range n.spec.inputs {
			if in.optional {
				continue
			}
			if _, ok := n.input[in.name]; !ok {
				return fmt.Errorf("script: %s requires input %q", n.label(), in.name)
			}
		}
	}

	return nil
}

// checkParamRefs checks every parameter a build referenced actually exists.
func checkParamRefs(d *scriptDef, schema []dsp.Param) error {
	index := make(map[string]struct{}, len(schema))
	for _, p := range schema {
		index[p.Key] = struct{}{}
	}
	for _, key := range d.graph.paramRefs {
		if _, ok := index[key]; !ok {
			return fmt.Errorf("script: build references parameter %q, which is not declared", key)
		}
	}

	return nil
}

// toGoValue converts a Lua scalar to the Go value dsp expects.
func toGoValue(v lua.LValue) any {
	switch v.Type() {
	case lua.LTBool:
		return bool(lua.LVAsBool(v))
	case lua.LTNumber:
		return float64(v.(lua.LNumber))
	case lua.LTString:
		return lua.LVAsString(v)
	default:
		return nil
	}
}
