package script

import (
	"fmt"
	"math"
)

// The kernel is pure Go and knows nothing about Lua. It takes a compiled graph
// and runs it as a flat list, in place, with no allocation and no locks. Fase A
// of the plan exists separately for this reason: if the kernel cannot hold the
// zero-allocation contract, adding an interpreter in front of it is pointless.
//
// Every node buffers one frame of ch channels in float64. A filter recurrence
// in float32 drifts, so the internal arithmetic is float64 and only the block
// edges are float32, the same choice the engine's own crossfeed makes.

// plan is a graph compiled for one sample rate: a flat order of nodes, a value
// buffer per node, and the scratch the combinators need. It is built once in
// Configure and only read afterwards.
type plan struct {
	ops   []*node
	ch    int
	rate  int
	frame []float64
	// scratch is the staging buffer a node reads its own wired input into
	// before writing its result. The delay needs it because it must read its
	// source while writing its line, and the source may be a node whose output
	// buffer is the one being written. It is allocated once, so the run loop
	// still allocates nothing.
	scratch []float64
	// ctrl indexes nodes by declaration position, so a published control value
	// can find its node without a map on the audio thread. Entries for constants
	// are nil.
	ctrl []*node
	// meters is every meter node, so one block's sample is published once per
	// block instead of once per frame.
	meters []*node
	// tail is the node ctx.out named. Its output is what leaves the effect.
	tail *node
}

// process consumes a whole block: for every frame it copies the interleaved
// input into the frame buffer, evaluates the flat plan, and writes the tail
// node's result back. A plan with no tail is a pass-through. It allocates
// nothing and takes no lock, which is the contract Process promises.
func (p *plan) process(buf []float32, frames int) {
	if p.tail == nil || p.ch < 1 {
		return
	}
	if n := frames * p.ch; n > len(buf) {
		frames = len(buf) / p.ch
	}
	for f := 0; f < frames; f++ {
		base := f * p.ch
		for c := 0; c < p.ch; c++ {
			p.frame[c] = float64(buf[base+c])
		}
		for _, n := range p.ops {
			runNode(n, n.out, p.frame, p.scratch, p.ch)
		}
		for c := 0; c < p.ch; c++ {
			buf[base+c] = float32(p.tail.output(c))
		}
	}
	// Publish each reading from the block's last frame, which is the value at
	// the block boundary. It is one store per meter per block, so it neither
	// allocates nor takes a lock.
	for _, n := range p.meters {
		if n.meter != nil {
			n.meter.store(n.out[0])
		}
	}
}

// runNode evaluates one node into scratch. Every node writes exactly ch values,
// including `in`, which copies the frame.
//
// staging is the shared scratch a node copies its wired input into when that
// input may alias its own output: the delay writes its line while reading its
// source, so it must snapshot the source first.
func runNode(n *node, scratch, frame, staging []float64, ch int) {
	switch n.kind {
	case kindIn:
		copy(scratch[:ch], frame[:ch])

	case kindConst:
		fill(scratch, ch, n.value)

	case kindParam:
		fill(scratch, ch, n.value)

	case kindLFO:
		s := n.state.(*lfoState)
		fill(scratch, ch, s.next(n.vals.rate, n.vals.phase))

	case kindDelay:
		s := n.state.(*delayState)
		in := src(n, "in")
		if in == nil {
			fill(staging, ch, 0)
		} else {
			readInto(staging, in, ch)
		}
		s.process(scratch, staging, ch, n.vals.time)

	case kindBiquad:
		s := n.state.(*biquadState)
		readInto(scratch, src(n, "in"), ch)
		for c := 0; c < ch; c++ {
			scratch[c] = s.step(scratch[c], c)
		}

	case kindOnePole:
		s := n.state.(*onePoleState)
		readInto(scratch, src(n, "in"), ch)
		for c := 0; c < ch; c++ {
			scratch[c] = s.step(scratch[c], c)
		}

	case kindShaper:
		s := n.state.(*shaperState)
		readInto(scratch, src(n, "in"), ch)
		for c := 0; c < ch; c++ {
			scratch[c] = s.apply(scratch[c], n.vals)
		}

	case kindEnv:
		s := n.state.(*envState)
		readInto(scratch, src(n, "in"), ch)
		v := s.step(scratch[:ch])
		fill(scratch, ch, v)

	case kindAdd, kindSub, kindMul, kindDiv, kindMin, kindMax:
		combine(n, scratch, ch)

	case kindScale:
		readInto(scratch, src(n, "in"), ch)
		k := n.vals.factor
		for c := 0; c < ch; c++ {
			scratch[c] *= k
		}

	case kindMeter:
		// Pass-through: the meter only observes the signal fed to it.
		readInto(scratch, src(n, "in"), ch)

	default:
		fill(scratch, ch, 0)
	}
	// A node cannot leave a non-finite value in the plan. A NaN reaching a
	// combinator poisons every later frame, so the run loop stops it here the
	// way the biquad stops it in its own memory.
	for c := 0; c < ch; c++ {
		if v := scratch[c]; math.IsNaN(v) || math.IsInf(v, 0) {
			scratch[c] = 0
		}
	}
}

func fill(dst []float64, ch int, v float64) {
	for c := 0; c < ch; c++ {
		dst[c] = v
	}
}

// output reads one channel of a node's result. A folded constant has no buffer
// and is materialised from its value, so a constant tail still passes through.
func (n *node) output(c int) float64 {
	if n.constant {
		return n.value
	}

	return n.out[c]
}

// src returns a node's input source, or nil when unconnected.
func src(n *node, input string) *node {
	return n.input[input].node
}

// readInto copies a node's output into a destination buffer. A folded constant
// has no buffer, so it is materialised here; a nil source is silence.
func readInto(dst []float64, s *node, ch int) {
	if s == nil {
		fill(dst, ch, 0)

		return
	}
	if s.constant {
		fill(dst, ch, s.value)

		return
	}
	copy(dst[:ch], s.out[:ch])
}

// combine evaluates the arithmetic nodes. Both inputs are required, so a
// one-argument `max` is a load error rather than a silent clamp toward zero.
// Division by a zero divisor yields a zero, not a silent infinity: the plan
// never carries a non-finite value into a feedback path.
func combine(n *node, scratch []float64, ch int) {
	left, right := src(n, "a"), src(n, "b")
	for c := 0; c < ch; c++ {
		a, b := operand(left, c), operand(right, c)
		var v float64
		switch n.kind {
		case kindAdd:
			v = a + b
		case kindSub:
			v = a - b
		case kindMul:
			v = a * b
		case kindDiv:
			if b == 0 {
				v = 0
			} else {
				v = a / b
			}
		case kindMin:
			v = math.Min(a, b)
		case kindMax:
			v = math.Max(a, b)
		}
		scratch[c] = v
	}
}

// operand reads one channel of a node's output. A folded constant is material,
// a nil input is silence.
func operand(n *node, c int) float64 {
	if n == nil {
		return 0
	}
	if n.constant {
		return n.value
	}

	return n.out[c]
}

// lfoState is an oscillator. Phase is kept in turns, so it can run for days
// without losing precision at large sample counts.
type lfoState struct {
	phase float64
	shape int
}

// LFO shape selectors. A script names them in the `shape` field; sine is the
// default.
const (
	lfoSin = iota
	lfoSquare
	lfoTriangle
	lfoSaw
)

func (s *lfoState) reset() { s.phase = 0 }

// next advances the oscillator one sample and returns its -1..1 value. The
// phase offset is in turns, matching the rate's unit, so `phase = 0.25` is a
// quarter cycle ahead.
func (s *lfoState) next(rate, phase float64) float64 {
	s.phase += rate
	if s.phase >= 1 || s.phase < 0 {
		s.phase -= math.Floor(s.phase)
	}
	p := s.phase + phase
	p -= math.Floor(p)

	return lfoValue(s.shape, p)
}

// lfoValue maps a 0..1 phase to the selected shape. Square and saw are step or
// ramp functions of phase, triangle is a lifted absolute ramp.
func lfoValue(shape int, p float64) float64 {
	switch shape {
	case lfoSquare:
		if p < 0.5 {
			return 1
		}

		return -1

	case lfoTriangle:
		if p < 0.25 {
			return 4 * p
		}
		if p < 0.75 {
			return 2 - 4*p
		}

		return 4*p - 4

	case lfoSaw:
		return 2*p - 1

	default:
		return math.Sin(2 * math.Pi * p)
	}
}

// lfoSelector maps a script shape name to its selector; an unknown name is a
// load error rather than a silent sine.
func lfoSelector(name string) (int, error) {
	switch lowerName(name) {
	case "sin", "sine":
		return lfoSin, nil
	case "square", "pulse":
		return lfoSquare, nil
	case "tri", "triangle":
		return lfoTriangle, nil
	case "saw", "sawtooth", "ramp":
		return lfoSaw, nil
	default:
		return 0, fmt.Errorf("script: unknown lfo shape %q", name)
	}
}

// maxLine is the saturation ceiling a delay line holds. A feedback loop at or
// above unity grows without bound, and while a float64 can hold a long way the
// block edge is float32: a value past 3.4e38 becomes Inf the moment it is
// written back. Saturating the line at 4 bounds the loop well inside that,
// while normal audio, which peaks near 1, never reaches it.
const maxLine = 4

// clampLine keeps a delay line's contents finite and bounded. A non-finite
// value becomes silence; a runaway value saturates at the ceiling.
func clampLine(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v > maxLine {
		return maxLine
	}
	if v < -maxLine {
		return -maxLine
	}

	return v
}

// delayState is a ring buffer of whole frames. Its tap output is what a
// feedback path reads, which is why it is a separate port: reading the tap
// gives the loop one sample of memory, so the loop cannot be algebraic.
type delayState struct {
	buf    []float64
	frames int
	ch     int
	write  int
}

func newDelayState(frames, ch int) *delayState {
	if frames < 1 {
		frames = 1
	}
	if ch < 1 {
		ch = 1
	}

	return &delayState{
		buf:    make([]float64, frames*ch),
		frames: frames,
		ch:     ch,
	}
}

// process writes the incoming frame into the line and reads the delayed frame
// out of it. A delay of n frames returns the input from n frames ago; a delay
// of zero is a pass-through.
func (s *delayState) process(dst, frame []float64, ch int, delay float64) {
	n := int(delay)
	if n < 0 {
		n = 0
	}
	if n >= s.frames {
		n = s.frames - 1
	}

	if ch > s.ch {
		ch = s.ch
	}
	wb := s.write * s.ch
	if n == 0 {
		// The read index would equal the write index, so reading first would
		// return the value from a full buffer ago; a zero delay passes through.
		for c := 0; c < ch; c++ {
			dst[c] = frame[c]
			s.buf[wb+c] = clampLine(frame[c])
		}
	} else {
		read := s.write - n
		if read < 0 {
			read += s.frames
		}
		rb := read * s.ch
		for c := 0; c < ch; c++ {
			dst[c] = clampLine(s.buf[rb+c])
		}
		for c := 0; c < ch; c++ {
			s.buf[wb+c] = clampLine(frame[c])
		}
	}

	s.write++
	if s.write >= s.frames {
		s.write = 0
	}
}

// biquadState is a direct form I biquad. Coefficients are recomputed off the
// audio thread; only the filter memory below is touched by the run loop.
//
// The memory is per channel: a shared x1/y1 pair would advance once per channel
// instead of once per frame, so in stereo the filter would run twice as fast
// and its cutoff would land an octave high. The slices are sized in
// newBiquadState, so the run loop still allocates nothing.
type biquadState struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     []float64
}

func newBiquadState(ch int) *biquadState {
	if ch < 1 {
		ch = 1
	}

	return &biquadState{
		x1: make([]float64, ch),
		x2: make([]float64, ch),
		y1: make([]float64, ch),
		y2: make([]float64, ch),
	}
}

// setCoeffs installs new coefficients without touching the memory, which is
// what keeps a cutoff sweep click-free.
func (s *biquadState) setCoeffs(b0, b1, b2, a1, a2 float64) {
	s.b0, s.b1, s.b2, s.a1, s.a2 = b0, b1, b2, a1, a2
}

func (s *biquadState) step(x float64, c int) float64 {
	y := s.b0*x + s.b1*s.x1[c] + s.b2*s.x2[c] - s.a1*s.y1[c] - s.a2*s.y2[c]
	// A sweep across a resonant setting can push an IIR's memory large enough
	// to overflow. A filter that recovers to silence beats one that emits NaN
	// for the rest of the session, so a non-finite result resets the memory.
	if math.IsNaN(y) || math.IsInf(y, 0) {
		s.x1[c], s.x2[c], s.y1[c], s.y2[c] = 0, 0, 0, 0

		return 0
	}
	s.x2[c], s.x1[c] = s.x1[c], x
	s.y2[c], s.y1[c] = s.y1[c], y

	return y
}

// onePoleState is a one-pole low-pass, and parameter smoothing is the same
// recurrence with a coefficient derived from a time constant. Its memory is per
// channel for the same reason the biquad's is.
type onePoleState struct {
	a float64
	y []float64
}

func newOnePoleState(a float64, ch int) *onePoleState {
	if ch < 1 {
		ch = 1
	}

	return &onePoleState{a: a, y: make([]float64, ch)}
}

func (s *onePoleState) step(x float64, c int) float64 {
	s.y[c] += (x - s.y[c]) * s.a
	if math.IsNaN(s.y[c]) || math.IsInf(s.y[c], 0) {
		s.y[c] = 0
	}

	return s.y[c]
}

// shaperState is a memoryless map chosen by a name. It is a type rather than a
// bare function so a shape that needs state later has somewhere to live.
type shaperState struct {
	shape int
}

// Shape selectors. A script names them in the shaper's `shape` field.
const (
	shapeTanh = iota
	shapeSoft
	shapeHard
	shapeDb
	shapeLin
	shapeSoftKnee
)

// apply runs the map over the resolved scalar set. drive scales the input
// before the non-linearity; the soft-knee shape reads threshold, ratio and knee
// instead. Both are read per block, so they can be parameters.
func (s *shaperState) apply(x float64, vals nodeValues) float64 {
	drive := vals.drive
	if drive <= 0 {
		drive = 1
	}
	switch s.shape {
	case shapeDb:
		// Linear amplitude to dB. Zero is -inf dB, which is not usable by a
		// downstream arithmetic node, so it floors at -160 dB.
		a := math.Abs(x)
		if a < 1e-8 {
			return -160
		}

		return 20 * math.Log10(a)

	case shapeLin:
		// dB to linear amplitude. A finite input always yields a finite gain.
		return math.Pow(10, x/20)

	case shapeSoft:
		v := x * drive
		if v <= -1 {
			return -2.0 / 3.0
		}
		if v >= 1 {
			return 2.0 / 3.0
		}

		return v - v*v*v/3.0

	case shapeHard:
		v := x * drive
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}

		return v

	case shapeSoftKnee:
		// The Audacity GainReductionComputer law: a level in dB in, a gain
		// reduction in dB out. Matches the frontend's transferSample.
		r := vals.ratio
		if r < 1 {
			r = 1
		}
		over := x - vals.threshold
		if vals.knee <= 0 {
			if over <= 0 {
				return 0
			}

			return over * (1 - 1/r)
		}
		half := vals.knee / 2
		if over <= -half {
			return 0
		}
		if over >= half {
			return over * (1 - 1/r)
		}

		return (1 - 1/r) * (over + half) * (over + half) / (2 * vals.knee)

	default:
		return math.Tanh(x * drive)
	}
}

// envState is a detector: it rectifies (peak) or averages power (rms), then
// smooths the result with an attack coefficient on a rise and a release
// coefficient on a fall.
type envState struct {
	attack, release float64
	mode            int
	y               float64
}

func (s *envState) reset() { s.y = 0 }

const (
	envPeak = iota
	envRMS
)

func (s *envState) step(frame []float64) float64 {
	var x float64
	switch s.mode {
	case envRMS:
		var power float64
		for _, v := range frame {
			power += v * v
		}
		power /= float64(len(frame))
		x = math.Sqrt(power) * math.Sqrt(math.Pi/2.0)
	default:
		for _, v := range frame {
			if a := math.Abs(v); a > x {
				x = a
			}
		}
	}

	a := s.release
	if x > s.y {
		a = s.attack
	}
	s.y += (x - s.y) * a
	if math.IsNaN(s.y) || math.IsInf(s.y, 0) {
		s.y = 0
	}

	return s.y
}

// designBiquad returns RBJ cookbook coefficients for a low-pass, high-pass,
// band-pass or peaking filter at a rate. The node's `type` field picks the
// form; an unknown type is a load error rather than a silent low-pass. `gainDB`
// is the peaking form's gain and is ignored by the others. Coefficients are
// normalised so a0 is 1.
func designBiquad(n *node, freq, q, gainDB float64, rate int) ([5]float64, error) {
	if rate <= 0 {
		rate = 48000
	}
	if !(freq > 0) || !(freq < float64(rate)/2) {
		return [5]float64{}, fmt.Errorf("script: biquad freq %v is outside (0, rate/2)", freq)
	}
	if !(q > 0) {
		q = 0.707
	}

	kind, _ := stringField(n, "type")
	if kind == "" {
		kind = "lowpass"
	}
	kind = lowerName(kind)

	w0 := 2 * math.Pi * freq / float64(rate)
	cos, sin := math.Cos(w0), math.Sin(w0)
	alpha := sin / (2 * q)
	a0 := 1 + alpha

	var b0, b1, b2, a1, a2 float64
	switch kind {
	case "lowpass", "low-pass", "lp":
		b0 = (1 - cos) / 2
		b1 = 1 - cos
		b2 = b0
		a1 = -2 * cos
		a2 = 1 - alpha

	case "highpass", "high-pass", "hp":
		b0 = (1 + cos) / 2
		b1 = -(1 + cos)
		b2 = b0
		a1 = -2 * cos
		a2 = 1 - alpha

	case "bandpass", "band-pass", "bp":
		b0 = alpha
		b1 = 0
		b2 = -alpha
		a1 = -2 * cos
		a2 = 1 - alpha

	case "peaking", "peak", "bell":
		A := math.Pow(10, gainDB/40)
		b0 = 1 + alpha*A
		b1 = -2 * cos
		b2 = 1 - alpha*A
		a0 = 1 + alpha/A
		a1 = -2 * cos
		a2 = 1 - alpha/A

	default:
		return [5]float64{}, fmt.Errorf("script: unknown biquad type %q", kind)
	}

	return [5]float64{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0}, nil
}
