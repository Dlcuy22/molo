package dsp

// Telemetry is the observation half of an effect: what it is doing, as opposed
// to what it is (Schema) or how loud it is (Metered, which reports a bare
// map). A compressor's gain reduction and its transfer curve have no home in
// the parameter set, so they get one here.
//
// Nothing in this file touches the audio path. An effect computes a reading on
// the control side, exactly as it computes Meters, and a UI reads it. The
// interfaces are optional capabilities in the same style as Latent, Bypassable
// and Metered, so an effect that does not implement them is unchanged.

// ReadingKind is how a reading should be drawn. It is a string type for the
// same reason Widget is: the value crosses the JSON bridge, and an iota order
// is not a contract.
type ReadingKind string

const (
	// ReadingLevel is a level in dBFS, filled from the floor up.
	ReadingLevel ReadingKind = "level"
	// ReadingGainReduction is a reduction in dB, 0 at the top, filled down.
	ReadingGainReduction ReadingKind = "gain-reduction"
	// ReadingScalar is a plain value with a unit.
	ReadingScalar ReadingKind = "scalar"
)

// Reading describes one key that Meters reports, so a UI can draw it without
// knowing which effect it is looking at. Key must match a Meters key: the
// number is still the effect's to publish, this only says how to render it.
type Reading struct {
	Key   string      `json:"key"`
	Label string      `json:"label"`
	Unit  string      `json:"unit"`
	Min   float64     `json:"min"`
	Max   float64     `json:"max"`
	Kind  ReadingKind `json:"kind"`
}

// Described is implemented by an effect that can explain its Meters keys.
// Metered stays the source of the numbers; this is the rendering contract. An
// effect that implements only Metered shows the legacy in/out pair, so this is
// additive.
type Described interface {
	// Readings returns the metadata for every key Meters reports beyond the
	// standard pair. The returned slice is a copy and may be kept.
	Readings() []Reading
}

// VisualKind names a plot the UI knows how to draw. The effect names the kind
// and the params; the UI owns the renderer, so a script author never writes
// frontend.
type VisualKind string

const (
	// VisualTransfer is a compressor, gate or limiter curve: an input level
	// mapped to an output level.
	VisualTransfer VisualKind = "transfer"
	// VisualGainReduction is a reduction-over-time display.
	VisualGainReduction VisualKind = "gain-reduction"
	// VisualDynamics is a scrolling level display: the overlays are reading
	// keys plotted over time, newest at the right edge, on the yMin..yMax scale.
	VisualDynamics VisualKind = "dynamics"
)

// Visual declares a plot. Params are schema keys the curve is derived from,
// which is what keeps a static curve off the meter path: the UI recomputes it
// from the Values it already has. Overlays are reading keys drawn live, such as
// the current input dot and the gain-reduction bar; for VisualDynamics they are
// the series the scrolling display plots.
type Visual struct {
	Kind     VisualKind `json:"kind"`
	Params   []string   `json:"params"`
	Overlays []string   `json:"overlays"`
	XMin     float64    `json:"xMin"`
	XMax     float64    `json:"xMax"`
	YMin     float64    `json:"yMin"`
	YMax     float64    `json:"yMax"`
}

// Visualized is implemented by an effect that wants one or more plots. Like
// Described it is optional and additive.
type Visualized interface {
	// Visuals returns every plot this effect wants drawn, in display order.
	// The returned slice and the inner Params and Overlays slices are copies
	// and may be kept.
	Visuals() []Visual
}
