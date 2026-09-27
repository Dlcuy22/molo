package script

import (
	"fmt"
	"math"
	"strings"

	lua "github.com/yuin/gopher-lua"

	"github.com/dlcuy22/player/dsp"
)

// The builder is the Lua-visible half of the front end. Every ctx call appends
// a node to the graph here in Go; the script sees handles that know their node
// and can be connected, indexed and combined with operators. Nothing in this
// file runs after load for a Tier 1 script.
//
// Arithmetic is expressed by Lua operators on the handles. A handle carries a
// shared metatable whose __add, __sub, __mul, __div and __unm build the
// corresponding graph node, so `ctx.in() * amt` builds a mul node, not
// arithmetic on one sample. A number operand is folded to a constant, which is
// what keeps `x * 0.5` from running a node for a literal.

// builder holds the interpreter and the graph being described.
type builder struct {
	L *lua.LState
	g *graph

	// outSet records that ctx.out ran, so a script that forgot it is an error
	// rather than silent output.
	outSet bool
	// ops is the shared operator metatable, built lazily.
	ops *lua.LTable
}

// handle is a signal handle's Lua-side payload: the node plus which of its
// ports this handle names. A delay's `.tap` is the same node on the tap port.
type handle struct {
	node *node
	port string
}

// newContext builds the ctx table a build function receives.
func (b *builder) newContext() *lua.LTable {
	ctx := b.L.NewTable()
	ctx.RawSetString("param", b.L.NewFunction(b.fnParam))
	ctx.RawSetString("out", b.L.NewFunction(b.fnOut))
	ctx.RawSetString("input", b.L.NewFunction(b.fnIn))
	ctx.RawSetString("in_", b.L.NewFunction(b.fnIn))
	ctx.RawSetString("const", b.L.NewFunction(b.fnConst))
	ctx.RawSetString("lfo", b.L.NewFunction(b.fnNode(kindLFO)))
	ctx.RawSetString("delay", b.L.NewFunction(b.fnNode(kindDelay)))
	ctx.RawSetString("biquad", b.L.NewFunction(b.fnNode(kindBiquad)))
	ctx.RawSetString("onepole", b.L.NewFunction(b.fnNode(kindOnePole)))
	ctx.RawSetString("shaper", b.L.NewFunction(b.fnNode(kindShaper)))
	ctx.RawSetString("env", b.L.NewFunction(b.fnNode(kindEnv)))
	ctx.RawSetString("add", b.L.NewFunction(b.fnBinary(kindAdd)))
	ctx.RawSetString("sub", b.L.NewFunction(b.fnBinary(kindSub)))
	ctx.RawSetString("mul", b.L.NewFunction(b.fnBinary(kindMul)))
	ctx.RawSetString("div", b.L.NewFunction(b.fnBinary(kindDiv)))
	ctx.RawSetString("scale", b.L.NewFunction(b.fnScale))
	ctx.RawSetString("min", b.L.NewFunction(b.fnBinary(kindMin)))
	ctx.RawSetString("max", b.L.NewFunction(b.fnBinary(kindMax)))
	ctx.RawSetString("control", b.L.NewFunction(b.fnControl))
	ctx.RawSetString("db", b.L.NewFunction(b.fnUnaryShape("db")))
	ctx.RawSetString("db2lin", b.L.NewFunction(b.fnUnaryShape("lin")))
	ctx.RawSetString("meter", b.L.NewFunction(b.fnMeter))
	ctx.RawSetString("visual", b.L.NewFunction(b.fnVisual))

	return ctx
}

// handle wraps a node for Lua on its default port.
func (b *builder) handle(n *node) *lua.LUserData {
	return b.handlePort(n, portOut)
}

// handlePort wraps a node for Lua on a named port.
func (b *builder) handlePort(n *node, port string) *lua.LUserData {
	ud := b.L.NewUserData()
	ud.Value = &handle{node: n, port: port}
	ud.Metatable = b.meta()

	return ud
}

// meta returns the shared metatable every handle uses.
func (b *builder) meta() *lua.LTable {
	if b.ops != nil {
		return b.ops
	}
	mt := b.L.NewTable()
	b.installBinary(mt, "__add", kindAdd)
	b.installBinary(mt, "__sub", kindSub)
	b.installBinary(mt, "__mul", kindMul)
	b.installBinary(mt, "__div", kindDiv)
	b.installBinary(mt, "__min", kindMin)
	b.installBinary(mt, "__max", kindMax)
	b.installUnary(mt, "__unm")
	mt.RawSetString("__call", b.L.NewFunction(b.fnCall))
	mt.RawSetString("__index", b.L.NewFunction(b.fnIndex))
	mt.RawSetString("__newindex", b.L.NewFunction(b.fnNewIndex))
	b.ops = mt

	return mt
}

// fnNewIndex handles `node.input = signal`, the assignment form of a
// connection. An unknown field is a script error rather than a silent no-op.
func (b *builder) fnNewIndex(L *lua.LState) int {
	h, err := b.requireRef(L, 1)
	if err != nil {
		L.RaiseError("%v", err)

		return 0
	}
	key := L.CheckString(2)
	if !inputName(h.node, key) {
		L.RaiseError("%s has no input %q", h.node.kind, key)

		return 0
	}
	value := L.Get(3)
	if err := b.connectValue(h.node, key, value); err != nil {
		L.RaiseError("%v", err)
	}

	return 0
}

// fnCall makes a node callable: `lp(input)` connects the argument to the node's
// single input and returns the same node, which reads like a filter applied to
// a signal. A node with no single input is a script error.
func (b *builder) fnCall(L *lua.LState) int {
	h, err := b.requireRef(L, 1)
	if err != nil {
		L.RaiseError("%v", err)

		return 0
	}
	if len(h.node.spec.inputs) != 1 {
		L.RaiseError("%s is not callable; it has no single input", h.node.kind)

		return 0
	}
	if L.GetTop() >= 2 {
		if err := b.connectValue(h.node, h.node.spec.inputs[0].name, L.Get(2)); err != nil {
			L.RaiseError("%v", err)

			return 0
		}
	}
	L.Push(b.handle(h.node))

	return 1
}

// refOf reads a handle payload from a userdata.
func refOf(v lua.LValue) (*handle, bool) {
	ud, ok := v.(*lua.LUserData)
	if !ok {
		return nil, false
	}
	h, ok := ud.Value.(*handle)

	return h, ok
}

// requireRef extracts a handle argument or raises, so a script that passes a
// number where a signal belongs gets a clear message.
func (b *builder) requireRef(L *lua.LState, n int) (*handle, error) {
	h, ok := refOf(L.Get(n))
	if !ok {
		return nil, fmt.Errorf("expected a node, got %s", L.Get(n).Type().String())
	}

	return h, nil
}

// fnParam returns a handle to a parameter's current value. It records the
// reference so the loader can reject an undeclared key.
func (b *builder) fnParam(L *lua.LState) int {
	key := L.CheckString(1)
	b.g.paramRefs = append(b.g.paramRefs, key)
	n, ok := b.mustNode(L, kindParam)
	if !ok {
		return 0
	}
	n.fields["param"] = key
	L.Push(b.handle(n))

	return 1
}

// fnIn returns the effect input node.
func (b *builder) fnIn(L *lua.LState) int {
	n, ok := b.mustNode(L, kindIn)
	if !ok {
		return 0
	}
	L.Push(b.handle(n))

	return 1
}

// fnConst returns a constant node.
func (b *builder) fnConst(L *lua.LState) int {
	v := L.CheckNumber(1)
	if !finite(luaNumberF(v)) {
		L.RaiseError("const must be finite")

		return 0
	}
	n, ok := b.mustNode(L, kindConst)
	if !ok {
		return 0
	}
	n.fields["value"] = float64(v)
	L.Push(b.handle(n))

	return 1
}

// fnNode returns the constructor for a primitive that takes a table of fields.
func (b *builder) fnNode(kind nodeKind) lua.LGFunction {
	return func(L *lua.LState) int {
		n, ok := b.mustNode(L, kind)
		if !ok {
			return 0
		}
		if v := L.Get(1); v != lua.LNil {
			tbl, ok := v.(*lua.LTable)
			if !ok {
				L.RaiseError("%s expects a table of fields", kind)

				return 0
			}
			if err := b.readFields(L, n, tbl); err != nil {
				L.RaiseError("%v", err)

				return 0
			}
		}
		L.Push(b.handle(n))

		return 1
	}
}

// readFields copies a node's scalar attributes and input connections from a
// Lua table. A key that names a port connects; every other key is a field.
func (b *builder) readFields(L *lua.LState, n *node, tbl *lua.LTable) error {
	var readErr error
	tbl.ForEach(func(key, value lua.LValue) {
		if readErr != nil {
			return
		}
		k, ok := key.(lua.LString)
		if !ok {
			return
		}
		name := string(k)
		if inputName(n, name) {
			readErr = b.connectValue(n, name, value)

			return
		}
		if !fieldName(n, name) {
			readErr = fmt.Errorf("%s has no field %q", n.kind, name)

			return
		}
		// A field backed by a parameter is recorded, not resolved: the value
		// is written every time the parameter changes.
		if ref, ok := refOf(value); ok && ref.node.kind == kindParam {
			b.g.paramRefs = append(b.g.paramRefs, paramKey(ref.node))
			if n.paramFields == nil {
				n.paramFields = make(map[string]string, 2)
			}
			n.paramFields[name] = paramKey(ref.node)

			return
		}
		// Any other handle where a field belongs is almost always a dropped
		// port name. Reinterpreting it as the node's input would connect the
		// wrong thing while leaving the named field at zero, so it is an
		// error: only an explicit port name may carry a connection.
		if _, ok := refOf(value); ok {
			readErr = fmt.Errorf("%s.%s wants a number, got a signal; name a port (in or tap) to connect", n.kind, name)

			return
		}
		switch value.Type() {
		case lua.LTNumber:
			f := float64(value.(lua.LNumber))
			if !finite(f) {
				readErr = fmt.Errorf("%s.%s must be finite", n.kind, name)

				return
			}
			n.fields[name] = f

		case lua.LTString:
			n.fields[name] = string(value.(lua.LString))

		case lua.LTBool:
			n.fields[name] = bool(lua.LVAsBool(value))
		}
	})

	return readErr
}

// inputName reports whether name names one of a node's input ports. A delay
// also accepts `input` as an alias for its single port, which is how the plan's
// delay example writes it.
func inputName(n *node, name string) bool {
	if n.spec == nil {
		return false
	}
	if name == "input" {
		return len(n.spec.inputs) == 1
	}
	for _, in := range n.spec.inputs {
		if in.name == name {
			return true
		}
	}

	return false
}

// fieldName reports whether name is a declared scalar field, so a typo is an
// error instead of a quietly ignored setting.
func fieldName(n *node, name string) bool {
	if n.spec == nil {
		return false
	}
	for _, f := range n.spec.fields {
		if f == name {
			return true
		}
	}

	return false
}

// meterMetaField reports whether name is a metadata key a meter table may
// carry. A key outside this set is a typo such as `maxx`, and dropping it would
// silently loosen the range the UI draws.
func meterMetaField(name string) bool {
	switch name {
	case "label", "unit", "min", "max", "kind":
		return true
	default:
		return false
	}
}

// connectValue wires an input port from a Lua handle.
func (b *builder) connectValue(dst *node, input string, value lua.LValue) error {
	if input == "input" {
		input = dst.spec.inputs[0].name
	}
	h, ok := refOf(value)
	if !ok {
		return fmt.Errorf("expected a node connection, got %s", value.Type().String())
	}

	return connect(dst, input, h.node, h.port)
}

// fnBinary builds a two-input arithmetic node.
func (b *builder) fnBinary(kind nodeKind) lua.LGFunction {
	return func(L *lua.LState) int {
		n, ok := b.mustNode(L, kind)
		if !ok {
			return 0
		}
		if err := b.bindOperand(n, "a", L.Get(1)); err != nil {
			L.RaiseError("%v", err)

			return 0
		}
		if v := L.Get(2); v != lua.LNil {
			if err := b.bindOperand(n, "b", v); err != nil {
				L.RaiseError("%v", err)

				return 0
			}
		}
		L.Push(b.handle(n))

		return 1
	}
}

// fnScale builds a scale node: one input multiplied by a constant factor.
func (b *builder) fnScale(L *lua.LState) int {
	n, ok := b.mustNode(L, kindScale)
	if !ok {
		return 0
	}
	if v := L.Get(1); v != lua.LNil {
		if err := b.bindOperand(n, "in", v); err != nil {
			L.RaiseError("%v", err)

			return 0
		}
	}
	if v := L.Get(2); v != lua.LNil {
		f := float64(L.CheckNumber(2))
		if !finite(f) {
			L.RaiseError("scale factor must be finite")

			return 0
		}
		n.fields["factor"] = f
	}
	L.Push(b.handle(n))

	return 1
}

// fnUnaryShape builds a shaper node with a fixed named shape, backing ctx.db
// and ctx.db2lin.
func (b *builder) fnUnaryShape(shape string) lua.LGFunction {
	return func(L *lua.LState) int {
		n, ok := b.mustNode(L, kindShaper)
		if !ok {
			return 0
		}
		n.fields["shape"] = shape
		if v := L.Get(1); v != lua.LNil {
			if err := b.bindOperand(n, "in", v); err != nil {
				L.RaiseError("%v", err)

				return 0
			}
		}
		L.Push(b.handle(n))

		return 1
	}
}

// bindOperand wires a binary input from either a handle or a number. A number
// becomes a constant node, which the folder then folds away.
func (b *builder) bindOperand(dst *node, input string, value lua.LValue) error {
	switch value.Type() {
	case lua.LTNumber:
		f := float64(value.(lua.LNumber))
		if !finite(f) {
			return fmt.Errorf("expected a finite number")
		}
		cf, err := b.g.addNode(kindConst)
		if err != nil {
			return err
		}
		cf.fields["value"] = f

		return connect(dst, input, cf, portOut)

	case lua.LTUserData:
		return b.connectValue(dst, input, value)

	default:
		return fmt.Errorf("expected a node or a number, got %s", value.Type().String())
	}
}

// fnOut sets the effect's output: the node whose value is written back.
func (b *builder) fnOut(L *lua.LState) int {
	h, err := b.requireRef(L, 1)
	if err != nil {
		L.RaiseError("%v", err)
	}
	b.g.tail = h.node
	b.outSet = true

	return 0
}

// fnMeter declares one named reading. The first argument is the key, the second
// a table of rendering metadata. The returned handle is callable like any
// single-input node: calling it feeds the signal whose value the reading
// publishes.
func (b *builder) fnMeter(L *lua.LState) int {
	key := strings.TrimSpace(L.CheckString(1))
	if key == "" {
		L.RaiseError("meter needs a non-empty key")

		return 0
	}
	// The standard pair is published under these keys by Meters, so a meter
	// that reused one would overwrite the level the UI draws and Readings would
	// then describe the standard key with the script's metadata.
	if key == dsp.MeterIn || key == dsp.MeterOut {
		L.RaiseError("meter %q is a reserved key", key)

		return 0
	}
	for _, r := range b.g.readings {
		if r.Key == key {
			L.RaiseError("meter %q is declared twice", key)

			return 0
		}
	}
	meta, ok := L.Get(2).(*lua.LTable)
	if !ok {
		L.RaiseError("meter %q needs a table of metadata", key)

		return 0
	}
	var metaErr error
	meta.ForEach(func(k, _ lua.LValue) {
		if metaErr != nil {
			return
		}
		name, ok := k.(lua.LString)
		if !ok {
			return
		}
		if !meterMetaField(string(name)) {
			metaErr = fmt.Errorf("meter %q has no metadata field %q", key, name)
		}
	})
	if metaErr != nil {
		L.RaiseError("%v", metaErr)

		return 0
	}
	decl := readingDecl{Key: key, Label: key}
	if v := meta.RawGetString("label"); v != lua.LNil {
		decl.Label = lua.LVAsString(v)
	}
	decl.Unit = lua.LVAsString(meta.RawGetString("unit"))
	if v, ok := meta.RawGetString("min").(lua.LNumber); ok {
		decl.Min = float64(v)
	}
	if v, ok := meta.RawGetString("max").(lua.LNumber); ok {
		decl.Max = float64(v)
	}
	if !finite(decl.Min) || !finite(decl.Max) {
		L.RaiseError("meter %q has a non-finite range", key)

		return 0
	}
	kindName := lua.LVAsString(meta.RawGetString("kind"))
	kind, ok := parseReadingKind(kindName)
	if !ok {
		L.RaiseError("meter %q has unknown kind %q", key, kindName)

		return 0
	}
	decl.Kind = kind

	n, ok := b.mustNode(L, kindMeter)
	if !ok {
		return 0
	}
	n.fields["key"] = key
	b.g.readings = append(b.g.readings, decl)
	L.Push(b.handle(n))

	return 1
}

// fnVisual records a plot the effect wants drawn. It is not a node: it names a
// kind the UI knows and the schema keys the curve is derived from, so the UI
// can recompute it from values it already has. A script may declare more than
// one, and the display order is the declaration order.
func (b *builder) fnVisual(L *lua.LState) int {
	tbl, ok := L.Get(1).(*lua.LTable)
	if !ok {
		L.RaiseError("visual expects a table")

		return 0
	}
	kindName := lua.LVAsString(tbl.RawGetString("kind"))
	kind, ok := parseVisualKind(kindName)
	if !ok {
		L.RaiseError("visual has unknown kind %q", kindName)

		return 0
	}
	params, err := stringList(tbl.RawGetString("params"), "params")
	if err != nil {
		L.RaiseError("%v", err)

		return 0
	}
	overlays, err := stringList(tbl.RawGetString("overlays"), "overlays")
	if err != nil {
		L.RaiseError("%v", err)

		return 0
	}
	// A param is a schema key the curve derives from. It is recorded as a
	// visual reference rather than a ctx.param one, so a missing key is reported
	// against the visual instead of as a bare build reference.
	for _, p := range params {
		b.g.visualRefs = append(b.g.visualRefs, p)
	}
	if params == nil {
		params = []string{}
	}
	if overlays == nil {
		overlays = []string{}
	}
	v := &dsp.Visual{
		Kind:     kind,
		Params:   params,
		Overlays: overlays,
	}
	ranges := []struct {
		name  string
		field *float64
	}{
		{"xMin", &v.XMin},
		{"xMax", &v.XMax},
		{"yMin", &v.YMin},
		{"yMax", &v.YMax},
	}
	for _, r := range ranges {
		*r.field = optNumber(tbl, r.name)
		if !finite(*r.field) {
			L.RaiseError("visual %s must be finite", r.name)

			return 0
		}
	}
	b.g.visuals = append(b.g.visuals, v)

	return 0
}

// optNumber reads an optional numeric field from a table.
func optNumber(tbl *lua.LTable, name string) float64 {
	if v, ok := tbl.RawGetString(name).(lua.LNumber); ok {
		return float64(v)
	}

	return 0
}

// fnControl registers the Tier 2 callback. It runs on the control goroutine,
// never on the audio thread or per sample.
func (b *builder) fnControl(L *lua.LState) int {
	fn, ok := L.Get(1).(*lua.LFunction)
	if !ok {
		L.RaiseError("control expects a function")

		return 0
	}
	b.g.control = fn
	L.Push(lua.LTrue)

	return 1
}

// fnIndex resolves a field on a signal handle. `tap` returns a delay's second
// port; `set` returns the Tier 2 writer, which records fields a control
// callback wants changed per block.
func (b *builder) fnIndex(L *lua.LState) int {
	h, err := b.requireRef(L, 1)
	if err != nil {
		L.Push(lua.LNil)

		return 1
	}
	key := L.CheckString(2)
	switch key {
	case portTap:
		if h.node.kind == kindDelay {
			L.Push(b.handlePort(h.node, portTap))

			return 1
		}
	case "set":
		L.Push(b.L.NewFunction(b.fnSet(h.node)))

		return 1
	}
	L.Push(lua.LNil)

	return 1
}

// fnSet returns the `node:set{...}` writer. It only records the desired fields;
// converting them to coefficients happens on the control goroutine, and
// applying them happens on the audio thread from a published snapshot. The sink
// is taken from the node, so a write from one effect's callback never lands in
// another instance's plan.
func (b *builder) fnSet(n *node) lua.LGFunction {
	return func(L *lua.LState) int {
		sink := n.sink
		if sink == nil {
			L.RaiseError("set is only valid inside a control callback")

			return 0
		}
		// `lp:set{...}` is method-call sugar: the handle arrives as argument 1
		// and the field table as argument 2.
		tbl, ok := L.Get(2).(*lua.LTable)
		if !ok {
			L.RaiseError("set expects a table of fields")

			return 0
		}
		var setErr error
		tbl.ForEach(func(key, value lua.LValue) {
			if setErr != nil {
				return
			}
			name, ok := key.(lua.LString)
			if !ok {
				return
			}
			f, ok := value.(lua.LNumber)
			if !ok {
				setErr = fmt.Errorf("set.%s expects a number", name)

				return
			}
			sink.write(n, string(name), float64(f))
		})
		if setErr != nil {
			L.RaiseError("%v", setErr)
		}

		return 0
	}
}

// installBinary installs an arithmetic metamethod that builds a node. The
// operand order is preserved so sub and div are correct for a number on the
// left: Lua moves a number to the right and calls the same metamethod, so a
// scalar-minus-handle is expressed as sub(const, handle).
func (b *builder) installBinary(mt *lua.LTable, name string, kind nodeKind) {
	mt.RawSetString(name, b.L.NewFunction(func(L *lua.LState) int {
		n, ok := b.mustNode(L, kind)
		if !ok {
			return 0
		}
		if err := b.bindOperand(n, "a", L.Get(1)); err != nil {
			L.RaiseError("%v", err)

			return 0
		}
		if err := b.bindOperand(n, "b", L.Get(2)); err != nil {
			L.RaiseError("%v", err)

			return 0
		}
		L.Push(b.handle(n))

		return 1
	}))
}

// installUnary installs the unary minus, which is a scale by -1.
func (b *builder) installUnary(mt *lua.LTable, name string) {
	mt.RawSetString(name, b.L.NewFunction(func(L *lua.LState) int {
		n, ok := b.mustNode(L, kindScale)
		if !ok {
			return 0
		}
		if err := b.bindOperand(n, "in", L.Get(1)); err != nil {
			L.RaiseError("%v", err)

			return 0
		}
		n.fields["factor"] = -1.0
		L.Push(b.handle(n))

		return 1
	}))
}

// mustNode appends a node, raising a Lua error instead of returning one because
// callers are Lua callbacks.
func (b *builder) mustNode(L *lua.LState, kind nodeKind) (*node, bool) {
	n, err := b.g.addNode(kind)
	if err != nil {
		L.RaiseError("%v", err)

		return nil, false
	}

	return n, true
}

// finite reports whether a float is a usable script value.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// luaNumberF converts a Lua number to float64 without an import cycle on the
// conversion's name.
func luaNumberF(v lua.LNumber) float64 { return float64(v) }
