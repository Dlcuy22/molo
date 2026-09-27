package script

import (
	"fmt"
	"math"
)

// Compiling turns a declared graph into a plan.
//
// The cycle rule is the interesting part. A combinatorial loop (a node reaches
// itself without passing through a delay) is rejected at build time: the run
// loop has no fixed point to solve, and a script that asks for one is asking
// for something the model cannot express. A loop that passes through a delay is
// legal, because the delay's tap reads the line before it is written, which
// contributes the one sample of memory that breaks the algebra.

// cloneGraph copies a declared graph into per-effect nodes. The factory's graph
// is a read-only template: every effect that is built from it owns its own
// nodes, so one instance's filter state and control writes cannot leak into
// another's.
func cloneGraph(g *graph) *graph {
	out := &graph{
		nodes:      make([]*node, len(g.nodes)),
		params:     g.params,
		paramRefs:  g.paramRefs,
		visualRefs: g.visualRefs,
		readings:   g.readings,
		visuals:    g.visuals,
	}
	// A first pass copies nodes and their scalar fields, the second pass
	// rewires edges to the clones.
	out.latencyParam = g.latencyParam
	out.latencySec = g.latencySec
	out.latencySet = g.latencySet
	for i, src := range g.nodes {
		n := &node{
			kind:     src.kind,
			spec:     src.spec,
			position: src.position,
			value:    src.value,
		}
		if src.fields != nil {
			n.fields = make(map[string]any, len(src.fields))
			for k, v := range src.fields {
				n.fields[k] = v
			}
		}
		if src.input != nil {
			n.input = make(map[string]inputRef, len(src.input))
		}
		if src.paramFields != nil {
			n.paramFields = make(map[string]string, len(src.paramFields))
			for k, v := range src.paramFields {
				n.paramFields[k] = v
			}
		}
		out.nodes[i] = n
	}
	for i, src := range g.nodes {
		for name, ref := range src.input {
			out.nodes[i].input[name] = inputRef{node: out.nodes[ref.node.position], port: ref.port}
		}
	}
	if g.tail != nil {
		out.tail = out.nodes[g.tail.position]
	}

	return out
}

// compile validates a declared graph for a sample rate and channel count and
// produces the flat plan. It runs in Configure, so every allocation here is off
// the audio thread.
func compile(g *graph, rate, ch int) (*plan, error) {
	if len(g.nodes) == 0 {
		return &plan{ch: ch, rate: rate}, nil
	}
	if rate <= 0 {
		return nil, fmt.Errorf("script: sample rate must be positive, got %d", rate)
	}
	if ch < 1 {
		ch = 1
	}

	// Each effect owns a private copy of the graph, so state and control
	// writes never bleed between instances built from one factory.
	cg := cloneGraph(g)

	if err := foldConstants(cg); err != nil {
		return nil, err
	}

	if err := checkInputs(cg); err != nil {
		return nil, err
	}
	if err := checkFanOut(cg); err != nil {
		return nil, err
	}

	order, err := topoSort(cg)
	if err != nil {
		return nil, err
	}

	p := &plan{
		ch:      ch,
		rate:    rate,
		ops:     order,
		frame:   make([]float64, ch),
		scratch: make([]float64, ch),
		ctrl:    make([]*node, len(cg.nodes)),
	}
	for _, n := range order {
		n.out = make([]float64, ch)
		if err := buildState(n, rate, ch); err != nil {
			return nil, err
		}
		p.ctrl[n.position] = n
		if n.kind == kindMeter {
			p.meters = append(p.meters, n)
		}
	}
	if cg.tail == nil {
		return nil, fmt.Errorf("script: graph has no output node")
	}
	p.tail = cg.tail

	return p, nil
}

// foldConstants marks every node whose output does not depend on audio. `const`
// is a leaf; a combinator over two folded inputs folds into a constant too.
// Folding is what makes arithmetic on literals free.
func foldConstants(g *graph) error {
	// A literal const carries its number in the field map; folding and every
	// reader need it in value, so it is lifted once up front.
	for _, n := range g.nodes {
		if n.kind == kindConst {
			if v, ok := numberField(n, "value"); ok {
				n.value = v
			}
		}
	}

	changed := true
	for changed {
		changed = false
		for _, n := range g.nodes {
			if n.constant {
				continue
			}
			v, ok := foldNode(n)
			if !ok {
				continue
			}
			n.value = v
			n.constant = true
			n.spec = nil
			changed = true
		}
	}

	return nil
}

// foldNode reports whether a node's output is a compile-time constant and, if
// so, its value.
func foldNode(n *node) (float64, bool) {
	switch n.kind {
	case kindConst:
		return n.value, true
	case kindAdd, kindSub, kindMul, kindDiv, kindMin, kindMax:
		// Both operands are required, so folding is only valid when both are
		// literals.
		left, okLeft := foldOperand(n.input["a"].node)
		right, okRight := foldOperand(n.input["b"].node)
		if !okLeft || !okRight {
			return 0, false
		}
		switch n.kind {
		case kindAdd:
			return left + right, true
		case kindSub:
			return left - right, true
		case kindMul:
			return left * right, true
		case kindDiv:
			if right == 0 {
				return 0, true
			}

			return left / right, true
		case kindMin:
			return math.Min(left, right), true
		case kindMax:
			return math.Max(left, right), true
		}
	}

	return 0, false
}

// foldOperand reads a node that may be a constant. An absent input is a folded
// zero, matching the run loop's default.
func foldOperand(n *node) (float64, bool) {
	if n == nil {
		return 0, true
	}
	if n.constant {
		return n.value, true
	}

	return 0, false
}

// checkInputs enforces the required-input rules. A missing required input is
// a script mistake, and a clearer error here beats silence on the audio path.
func checkInputs(g *graph) error {
	for _, n := range g.nodes {
		if n.constant {
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

// checkFanOut bounds how many consumers one node may have, so a script cannot
// name a single source enough times to blow up the plan.
func checkFanOut(g *graph) error {
	counts := make(map[*node]int, len(g.nodes))
	for _, n := range g.nodes {
		for _, ref := range n.input {
			counts[ref.node]++
			if counts[ref.node] > maxFanOut {
				return fmt.Errorf("script: %s is read by more than %d nodes", ref.node.label(), maxFanOut)
			}
		}
	}

	return nil
}

// topoSort returns the execution order: every node after the nodes it reads.
//
// A depth-first walk marks nodes as visited or on-stack. An edge to a node on
// the stack is a cycle; the edge is allowed only when it reads a delay's tap,
// because that edge carries the delay's memory and does not require the
// consumer to wait for the producer.
func topoSort(g *graph) ([]*node, error) {
	const (
		unvisited = iota
		visiting
		done
	)
	mark := make(map[*node]int, len(g.nodes))
	order := make([]*node, 0, len(g.nodes))

	var visit func(n *node) error
	visit = func(n *node) error {
		switch mark[n] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("script: cycle through %s is not allowed; feed a delay's tap to break it", n.label())
		}
		mark[n] = visiting
		// A folded constant has no spec and no inputs to walk.
		if n.spec != nil {
			for _, in := range n.spec.inputs {
				ref, ok := n.input[in.name]
				if !ok {
					continue
				}
				srcNode := ref.node
				if mark[srcNode] == visiting {
					// The only legal back edge is a tap read of the delay.
					// Every other back edge is combinatorial and has no
					// run-loop fixed point.
					if srcNode.kind == kindDelay && ref.port == portTap {
						continue
					}

					return fmt.Errorf("script: cycle through %s is not allowed; feed a delay's tap to break it", srcNode.label())
				}
				if err := visit(srcNode); err != nil {
					return err
				}
			}
		}
		mark[n] = done
		order = append(order, n)

		return nil
	}

	for _, n := range g.nodes {
		if n.constant {
			continue
		}
		if err := visit(n); err != nil {
			return nil, err
		}
	}

	// Constant nodes are dropped from the plan: every reader materialises them
	// from n.value, so running them would be wasted work.
	out := make([]*node, 0, len(order))
	for _, n := range order {
		if n.constant {
			continue
		}
		out = append(out, n)
	}

	return out, nil
}

// resolveValues fills a node's scalar set from its raw fields. A field whose
// value came from ctx.param is left at zero here and overwritten whenever the
// parameter is published, so this pass only settles the literals.
func resolveValues(n *node, rate int) error {
	// A literal const carries its number in the field map; foldConstants needs
	// it in value before it can fold, and readers need it after.
	if n.kind == kindConst {
		if v, ok := numberField(n, "value"); ok {
			n.value = v
		}
	}
	// The delay's `max` is the buffer size and also its default time.
	if n.kind == kindDelay {
		if v, ok := numberField(n, "time"); ok {
			n.vals.time = v * float64(rate)
		} else if v, ok := numberField(n, "max"); ok {
			n.vals.time = v * float64(rate)
		}
	}
	if v, ok := numberField(n, "rate"); ok {
		n.vals.rate = v / float64(rate)
	}
	if v, ok := numberField(n, "phase"); ok {
		n.vals.phase = v
	}
	if v, ok := numberField(n, "drive"); ok {
		n.vals.drive = v
	}
	if v, ok := numberField(n, "threshold"); ok {
		n.vals.threshold = v
	}
	if v, ok := numberField(n, "ratio"); ok {
		n.vals.ratio = v
	}
	if v, ok := numberField(n, "knee"); ok {
		n.vals.knee = v
	}
	if v, ok := numberField(n, "factor"); ok {
		n.vals.factor = v
	}
	if v, ok := numberField(n, "freq"); ok {
		n.vals.freq = v
	}
	if v, ok := numberField(n, "q"); ok {
		n.vals.q = v
	}
	if v, ok := numberField(n, "gain"); ok {
		n.vals.gain = v
	}
	if v, ok := numberField(n, "cutoff"); ok {
		n.vals.cutoff = smoothCoeff(1.0/v, rate)
	}
	if v, ok := numberField(n, "attack"); ok {
		n.vals.attack = v
	}
	if v, ok := numberField(n, "release"); ok {
		n.vals.release = v
	}
	n.lit = n.vals

	return nil
}

// buildState allocates each node's persistent state and settles its resolved
// scalars. Delay buffers are the only size that depends on a script field, so
// they are the only ones that consult the resource cap.
func buildState(n *node, rate, ch int) error {
	if err := resolveValues(n, rate); err != nil {
		return err
	}

	switch n.kind {
	case kindIn:
	case kindConst, kindParam:
		// Nothing to allocate; the value rides on the node.
	case kindLFO:
		shape := lfoSin
		if name, ok := stringField(n, "shape"); ok {
			sel, err := lfoSelector(name)
			if err != nil {
				return err
			}
			shape = sel
		}
		n.state = &lfoState{shape: shape}
	case kindDelay:
		seconds, ok := numberField(n, "max")
		if !ok {
			if _, paramed := n.paramFields["time"]; paramed {
				// A parameterised time has no literal to size the line from,
				// so `max` is required. Falling back to the time default would
				// silently pick a length the script never asked for.
				return fmt.Errorf("script: %s has a parameterised time and needs an explicit max to size the line", n.label())
			}
			seconds, ok = numberField(n, "time")
			if !ok {
				return fmt.Errorf("script: %s needs a time or a max", n.label())
			}
		}
		frames := maxDelayFor(seconds, rate)
		n.state = newDelayState(frames, ch)
	case kindBiquad:
		freq := n.vals.freq
		if freq == 0 {
			freq = 1000
		}
		q := n.vals.q
		if q == 0 {
			q = 0.707
		}
		c, err := designBiquad(n, freq, q, n.vals.gain, rate)
		if err != nil {
			return err
		}
		st := newBiquadState(ch)
		st.setCoeffs(c[0], c[1], c[2], c[3], c[4])
		n.state = st
	case kindOnePole:
		n.state = newOnePoleState(n.vals.cutoff, ch)
	case kindShaper:
		shape, ok := stringField(n, "shape")
		if !ok {
			shape = "tanh"
		}
		sel, err := shapeSelector(shape)
		if err != nil {
			return err
		}
		if sel == shapeTanh && n.vals.drive == 0 {
			n.vals.drive = 1
		}
		n.state = &shaperState{shape: sel}
	case kindEnv:
		mode := envPeak
		if m, ok := stringField(n, "mode"); ok && lowerName(m) == "rms" {
			mode = envRMS
		}
		n.state = &envState{
			attack:  smoothCoeff(n.vals.attack, rate),
			release: smoothCoeff(n.vals.release, rate),
			mode:    mode,
		}
	case kindMeter:
		// The reading is one published float, not a per-frame buffer.
		n.meter = &meterValue{}
	}

	return nil
}

func shapeSelector(name string) (int, error) {
	switch lowerName(name) {
	case "tanh", "soft-sat", "saturation":
		return shapeTanh, nil
	case "soft", "softclip", "soft-clip":
		return shapeSoft, nil
	case "hard", "hardclip", "hard-clip", "clip":
		return shapeHard, nil
	case "softknee", "soft-knee", "knee":
		return shapeSoftKnee, nil
	case "db", "linear2db", "lin2db":
		return shapeDb, nil
	case "lin", "db2lin":
		return shapeLin, nil
	default:
		return 0, fmt.Errorf("script: unknown shaper %q", name)
	}
}

// numberField reads a numeric field, reporting whether it was present.
func numberField(n *node, name string) (float64, bool) {
	v, ok := n.fields[name]
	if !ok {
		return 0, false
	}
	f, ok := toFloat(v)

	return f, ok
}

func stringField(n *node, name string) (string, bool) {
	v, ok := n.fields[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)

	return s, ok
}

// smoothCoeff converts a time in seconds to a per-sample one-pole coefficient.
// A non-positive time is treated as instantaneous, which a script that wants a
// raw envelope value would set.
func smoothCoeff(seconds float64, rate int) float64 {
	if seconds <= 0 || rate <= 0 {
		return 1
	}

	return 1 - math.Exp(-1.0/(seconds*float64(rate)))
}

// toFloat reads the numeric types a Lua binder produces.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
