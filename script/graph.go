// Package script builds dsp.Effect implementations from Lua descriptions.
//
// The split is the whole point: Lua declares a parameter set and a node graph
// once at load time, the Go kernel here builds that graph into a flat execution
// plan, and only the Go plan runs per sample. Lua never runs on the audio
// thread and never runs per sample, so the real-time contract that every other
// dsp stage obeys holds unchanged: Process takes no lock, allocates nothing,
// and never calls back into the interpreter.
//
// Tier 2 scripts may register a control callback that recomputes node state per
// block. That callback runs on a control goroutine and publishes its result
// atomically, and a watchdog silences it if it ever overruns its budget.
package script

import (
	"fmt"
	"math"
	"strings"

	"github.com/dlcuy22/molo/dsp"
)

// Resource limits. A script is untrusted input, so the compiler bounds what it
// can ask for up front instead of discovering the cost while building.
const (
	// maxNodes caps the number of graph nodes. It is generous for a real
	// effect and small enough that the topological sort and the run loop stay
	// trivially cheap.
	maxNodes = 256
	// maxDelayFrames caps one delay line, about 21.8 seconds at 48 kHz. A
	// delay node is the only node that reserves memory proportional to a
	// parameter, so it is the only one that needs a ceiling.
	maxDelayFrames = 1 << 20
	// maxFanOut caps how many other nodes may read one node's output. It
	// exists so a script cannot turn one node into a combinatorial explosion
	// by naming it a thousand times.
	maxFanOut = 64
)

// Port names. Every node has exactly one signal output, addressed as "out";
// a delay additionally exposes "tap" to read its delayed output before the
// input sum, which is what makes feedback expressible.
const (
	portOut = "out"
	portTap = "tap"
)

// nodeKind names a node primitive. The string values are the vocabulary a Lua
// script writes in ctx.node(...).
type nodeKind string

const (
	kindIn      nodeKind = "in"
	kindConst   nodeKind = "const"
	kindParam   nodeKind = "param"
	kindLFO     nodeKind = "lfo"
	kindDelay   nodeKind = "delay"
	kindBiquad  nodeKind = "biquad"
	kindOnePole nodeKind = "onepole"
	kindShaper  nodeKind = "shaper"
	kindEnv     nodeKind = "env"
	kindAdd     nodeKind = "add"
	kindSub     nodeKind = "sub"
	kindMul     nodeKind = "mul"
	kindDiv     nodeKind = "div"
	kindScale   nodeKind = "scale"
	kindMin     nodeKind = "min"
	kindMax     nodeKind = "max"
	kindMeter   nodeKind = "meter"
)

// inputSpec describes one input port: a name, and whether a script may leave it
// unconnected. It is the single source of truth for both the Lua binder and the
// kernel builder.
type inputSpec struct {
	name     string
	optional bool
}

// nodeSpec is the static description of a primitive: which inputs exist and
// which scalar attributes it reads. It is the single source of truth for both
// the Lua binder and the kernel builder.
type nodeSpec struct {
	kind   nodeKind
	inputs []inputSpec
	fields []string
}

// specs is the primitive catalogue. The order is the order a script may name
// them in; it is otherwise meaningless.
var specs = []nodeSpec{
	{kind: kindIn},
	{kind: kindConst, fields: []string{"value"}},
	{kind: kindParam, fields: []string{"param"}},
	{kind: kindLFO, fields: []string{"rate", "phase", "shape"}},
	{kind: kindDelay, fields: []string{"time", "max"}, inputs: []inputSpec{{name: "in"}}},
	{kind: kindBiquad, fields: []string{"type", "freq", "q", "gain"}, inputs: []inputSpec{{name: "in"}}},
	{kind: kindOnePole, fields: []string{"cutoff"}, inputs: []inputSpec{{name: "in"}}},
	{kind: kindShaper, fields: []string{"shape", "drive", "threshold", "ratio", "knee"}, inputs: []inputSpec{{name: "in"}}},
	{kind: kindEnv, fields: []string{"attack", "release", "mode"}, inputs: []inputSpec{{name: "in"}}},
	{kind: kindAdd, inputs: []inputSpec{{name: "a"}, {name: "b", optional: true}}},
	{kind: kindSub, inputs: []inputSpec{{name: "a"}, {name: "b"}}},
	{kind: kindMul, inputs: []inputSpec{{name: "a"}, {name: "b"}}},
	{kind: kindDiv, inputs: []inputSpec{{name: "a"}, {name: "b"}}},
	{kind: kindScale, fields: []string{"factor"}, inputs: []inputSpec{{name: "in"}}},
	{kind: kindMin, inputs: []inputSpec{{name: "a"}, {name: "b"}}},
	{kind: kindMax, inputs: []inputSpec{{name: "a"}, {name: "b"}}},
	{kind: kindMeter, fields: []string{"key"}, inputs: []inputSpec{{name: "in"}}},
}

func specFor(kind nodeKind) *nodeSpec {
	for i := range specs {
		if specs[i].kind == kind {
			return &specs[i]
		}
	}

	return nil
}

// inputRef is one edge: the source node and which of its ports feeds this
// input.
type inputRef struct {
	node *node
	port string
}

// node is one instance in a script's graph. Specs and fields are filled by the
// binder; input, value, constant and out are filled by the compiler.
type node struct {
	kind nodeKind
	// spec is the primitive description, nil after a constant is folded away.
	spec *nodeSpec
	// fields is the raw scalar attributes, validated by the builder.
	fields map[string]any
	// inputs maps an input port name to its source.
	input map[string]inputRef
	// position is the index in the declaration order, used to make error
	// messages refer to nodes the way a script wrote them.
	position int

	// value carries the node's constant output when constant is true. Every
	// output is one channel, so a constant is one scalar.
	value    float64
	constant bool

	// paramFields records the field names whose value came from ctx.param. At
	// publish time the parameter's current number is written into vals, so the
	// run loop reads a resolved scalar and not a store lookup.
	paramFields map[string]string
	// vals is the resolved form of every scalar the run loop needs. Resolving
	// once per parameter change keeps the per-sample path to plain reads.
	vals nodeValues
	// lit is the literal form, captured at compile and never mutated. The
	// control side reads it to fill fields a script did not parameterise, so it
	// never touches the audio-owned vals.
	lit nodeValues

	// out is the node's signal output buffer. A constant node has no buffer;
	// readers use value instead.
	out   []float64
	state any
	// sink is where a Tier 2 `set` writes go. It is per effect, installed when
	// the node is cloned into a plan, so two effects built from one script do
	// not share control state.
	sink *controlBridge
	// meter is the published reading a meter node samples into. It is nil for
	// every other kind, and it is allocated on the plan's own clone so two
	// effects built from one script never share a reading.
	meter *meterValue
}

// nodeValues is the resolved scalar set one node runs on. Not every kind uses
// every field; the zero value is the same as an unset field.
type nodeValues struct {
	rate  float64 // lfo rate, in turns per sample
	phase float64 // lfo phase offset, in turns
	time  float64 // delay length, in frames
	drive float64 // shaper drive
	// Shaper soft-knee parameters, read only by the softknee shape: a
	// threshold and a knee width in dB, and a compression ratio.
	threshold float64
	ratio     float64
	knee      float64
	factor    float64 // scale factor
	cutoff    float64 // onepole coefficient
	freq      float64 // biquad centre frequency
	q         float64 // biquad Q
	gain      float64 // biquad peaking gain, dB
	attack    float64
	release   float64
}

func (n *node) label() string {
	return fmt.Sprintf("#%d %s", n.position, n.kind)
}

// graph is a whole script's description before it is compiled, plus the
// parameter metadata its scripts declared.
type graph struct {
	nodes []*node
	// params maps a parameter key to its declaration, in sorted key order so
	// the schema does not depend on Go's randomised map iteration.
	params []paramDecl
	// paramRefs records every key build read through ctx.param, so the binder
	// can reject a reference to a parameter that was never declared.
	paramRefs []string
	// visualRefs records every key a ctx.visual named in its params list, so a
	// missing key is reported against the visual rather than as a bare build
	// reference.
	visualRefs []string
	// control is the Tier 2 callback, when the script registered one.
	control any
	// tail is the node ctx.out named: the value Process writes back.
	tail *node
	// readings is every meter declaration, in declaration order. It is static
	// metadata; the numbers live in the meter nodes' published values.
	readings []readingDecl
	// visuals is every plot the script declared, in declaration order.
	visuals []*dsp.Visual
	// latencyParam is the parameter key ctx.latency named, empty when the
	// script passed a literal.
	latencyParam string
	// latencySec is the literal latency in seconds, used when no parameter
	// was named.
	latencySec float64
	// latencySet records that ctx.latency ran, so a second call is rejected
	// even when the first declared a zero literal.
	latencySet bool
}

// newGraph returns an empty graph.
func newGraph() *graph { return &graph{} }

// addNode appends a node of the given primitive and returns it. It enforces the
// node cap here rather than in the compiler because the cap is about what a
// script may declare, not about what survives compilation.
func (g *graph) addNode(kind nodeKind) (*node, error) {
	if len(g.nodes) >= maxNodes {
		return nil, fmt.Errorf("script: graph has more than %d nodes", maxNodes)
	}
	spec := specFor(kind)
	if spec == nil {
		return nil, fmt.Errorf("script: unknown node kind %q", kind)
	}
	n := &node{
		kind:     kind,
		spec:     spec,
		fields:   make(map[string]any, len(spec.fields)),
		input:    make(map[string]inputRef, len(spec.inputs)),
		position: len(g.nodes),
	}
	g.nodes = append(g.nodes, n)

	return n, nil
}

// addParam registers a parameter declaration. Duplicate keys are rejected
// because dsp's own store would reject them anyway, and a clearer message here
// points at the script.
func (g *graph) addParam(decl paramDecl) error {
	for _, p := range g.params {
		if p.Key == decl.Key {
			return fmt.Errorf("script: duplicate parameter %q", decl.Key)
		}
	}
	g.params = append(g.params, decl)

	return nil
}

// paramKindOf returns the declared kind for a parameter key and whether it was
// declared. It lets a build-time check read the kind before the schema is built.
func (g *graph) paramKindOf(key string) (string, bool) {
	for _, p := range g.params {
		if p.Key == key {
			return lowerName(p.Kind), true
		}
	}

	return "", false
}

// connect wires one input port of a node, checking the port exists and never
// double-driving it. Replacing a connection is not allowed: a second wire is
// almost always a script mistake, and silently keeping one hides it.
func connect(dst *node, input string, src *node, port string) error {
	if spec := dst.spec; spec != nil {
		known := false
		for _, in := range spec.inputs {
			if in.name == input {
				known = true

				break
			}
		}
		if !known {
			return fmt.Errorf("script: %s has no input %q", dst.label(), input)
		}
	}
	if _, ok := dst.input[input]; ok {
		return fmt.Errorf("script: %s input %q is already connected", dst.label(), input)
	}
	if src.kind == kindDelay && port == portTap {
		// The tap port exists only on a delay; every other port must be out.
	} else if port != portOut {
		return fmt.Errorf("script: %s has no output %q", src.label(), port)
	}
	dst.input[input] = inputRef{node: src, port: port}

	return nil
}

// maxDelayFor converts a delay time in seconds to a whole number of frames at a
// rate, with a floor so a zero or tiny time still yields a usable line.
func maxDelayFor(seconds float64, rate int) int {
	if rate <= 0 {
		rate = 48000
	}
	frames := int(math.Ceil(seconds * float64(rate)))
	if frames < 1 {
		frames = 1
	}
	if frames > maxDelayFrames {
		frames = maxDelayFrames
	}

	return frames
}

// lowerName normalises a script-supplied name for comparison.
func lowerName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// paramKey reads the parameter key a param node was built from.
func paramKey(n *node) string {
	if n == nil {
		return ""
	}
	s, _ := n.fields["param"].(string)

	return s
}
