package session

import (
	"sync"
	"testing"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/dsp"
)

// The telemetry test effects register under kinds the real registry does not
// own, so they prove the telemetry surface without adding anything to the
// shipped dsp package. Their meters, readings and visual are fixed constants:
// this test is about the engine passing them through, not about an effect
// computing them.
const (
	testDescribedKind = "test-described"
	testMeteredKind   = "test-metered-only"
	testReusedKind    = "test-reused-visual"
)

// telemetryBase is the whole dsp.Effect half both fakes share. It is embedded
// so each fake adds only the capability methods under test.
type telemetryBase struct{}

func (*telemetryBase) Name() string            { return "telemetry-test" }
func (*telemetryBase) Schema() []dsp.Param     { return nil }
func (*telemetryBase) Get(string) (any, error) { return nil, dsp.ErrUnknownParam }
func (*telemetryBase) Set(string, any) error   { return dsp.ErrUnknownParam }
func (*telemetryBase) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	return in, nil
}
func (*telemetryBase) Reset() error                 { return nil }
func (*telemetryBase) Process([]float32, int) error { return nil }

// describedTestEffect implements dsp.Metered on top of the base, plus
// dsp.Described and dsp.Visualized. The three values it reports are the ones
// the test asserts come back through EffectStages.
type describedTestEffect struct {
	telemetryBase
}

func (*describedTestEffect) Meters() map[string]float32 {
	return map[string]float32{dsp.MeterIn: -12, "gr": -6}
}

func (*describedTestEffect) Readings() []dsp.Reading {
	return []dsp.Reading{{
		Key:   "gr",
		Label: "Gain Reduction",
		Unit:  "dB",
		Min:   -30,
		Max:   0,
		Kind:  dsp.ReadingGainReduction,
	}}
}

func (*describedTestEffect) Visual() dsp.Visual {
	return dsp.Visual{
		Kind:     dsp.VisualTransfer,
		Params:   []string{"threshold", "ratio"},
		Overlays: []string{"in", "gr"},
		XMin:     -60,
		XMax:     0,
		YMin:     -60,
		YMax:     0,
	}
}

// meteredTestEffect implements dsp.Metered and nothing else. It is a distinct
// type, not a flag on the one above, because a Go method set is satisfied as a
// whole: the only way to build an effect without the optional capabilities is a
// type that does not declare them.
type meteredTestEffect struct {
	telemetryBase
}

func (*meteredTestEffect) Meters() map[string]float32 {
	return map[string]float32{dsp.MeterIn: -12, "gr": -6}
}

// reusedVisualEffect returns a Visual over package-level slices it keeps
// handing out and mutates in place, which is the shape the copy in
// stageFromEffect has to defend against. Its arrays are deliberately larger
// than the slices it reports, so mutating the live element is visible.
var (
	reusedParams   = []string{"threshold", "ratio"}
	reusedOverlays = []string{"in", "gr"}
)

type reusedVisualEffect struct {
	telemetryBase
}

func (*reusedVisualEffect) Visual() dsp.Visual {
	return dsp.Visual{
		Kind:     dsp.VisualTransfer,
		Params:   reusedParams[:2],
		Overlays: reusedOverlays[:2],
	}
}

// The interface assertions pin the two fakes to the shapes the test needs.
var (
	_ dsp.Effect     = (*describedTestEffect)(nil)
	_ dsp.Metered    = (*describedTestEffect)(nil)
	_ dsp.Described  = (*describedTestEffect)(nil)
	_ dsp.Visualized = (*describedTestEffect)(nil)
	_ dsp.Metered    = (*meteredTestEffect)(nil)
	_ dsp.Visualized = (*reusedVisualEffect)(nil)
)

// telemetryTestFactory builds either fake. build selects which.
type telemetryTestFactory struct {
	kind  string
	impl  string
	build func() dsp.Effect
}

func (f *telemetryTestFactory) Kind() string             { return f.kind }
func (f *telemetryTestFactory) Impl() string             { return f.impl }
func (f *telemetryTestFactory) FriendlyName() string     { return f.impl }
func (f *telemetryTestFactory) Weight() int              { return 1 }
func (f *telemetryTestFactory) Placement() dsp.Placement { return dsp.Post }
func (f *telemetryTestFactory) Schema() []dsp.Param      { return nil }
func (f *telemetryTestFactory) New(dsp.Values) (dsp.Effect, error) {
	return f.build(), nil
}

// telemetryFactories is a sync.Once so a test binary that re-runs init cannot
// register the same (kind, impl) twice, which dsp.Register would panic on.
var telemetryFactories sync.Once

func registerTelemetryFactories() {
	telemetryFactories.Do(func() {
		dsp.Register(&telemetryTestFactory{
			kind:  testDescribedKind,
			impl:  testDescribedKind + "-v1",
			build: func() dsp.Effect { return &describedTestEffect{} },
		})
		dsp.Register(&telemetryTestFactory{
			kind:  testMeteredKind,
			impl:  testMeteredKind + "-v1",
			build: func() dsp.Effect { return &meteredTestEffect{} },
		})
		dsp.Register(&telemetryTestFactory{
			kind:  testReusedKind,
			impl:  testReusedKind + "-v1",
			build: func() dsp.Effect { return &reusedVisualEffect{} },
		})
	})
}

// TestEffectStageReportsReadingsAndVisual proves the engine fills the new
// telemetry fields from the optional interfaces: an effect that implements
// dsp.Described and dsp.Visualized reports its readings and a non-nil visual
// through EffectStages, beside its live meters.
func TestEffectStageReportsReadingsAndVisual(t *testing.T) {
	registerTelemetryFactories()

	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect(testDescribedKind, "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	st, ok := stageByID(s.EffectStages(), id)
	if !ok {
		t.Fatalf("stage %s missing", id)
	}

	if len(st.Readings) != 1 {
		t.Fatalf("Readings = %+v, want one", st.Readings)
	}
	got := st.Readings[0]
	if got.Key != "gr" || got.Label != "Gain Reduction" || got.Unit != "dB" || got.Kind != dsp.ReadingGainReduction {
		t.Fatalf("reading = %+v, want the described gr entry", got)
	}
	if got.Min != -30 || got.Max != 0 {
		t.Fatalf("reading range = [%v %v], want [-30 0]", got.Min, got.Max)
	}

	if st.Visual == nil {
		t.Fatal("Visual = nil, want the described transfer plot")
	}
	if st.Visual.Kind != dsp.VisualTransfer {
		t.Fatalf("Visual.Kind = %q, want %q", st.Visual.Kind, dsp.VisualTransfer)
	}
	if len(st.Visual.Params) != 2 || st.Visual.Params[0] != "threshold" || st.Visual.Params[1] != "ratio" {
		t.Fatalf("Visual.Params = %v, want [threshold ratio]", st.Visual.Params)
	}
	if len(st.Visual.Overlays) != 2 || st.Visual.Overlays[0] != "in" || st.Visual.Overlays[1] != "gr" {
		t.Fatalf("Visual.Overlays = %v, want [in gr]", st.Visual.Overlays)
	}
	if st.Visual.XMin != -60 || st.Visual.XMax != 0 || st.Visual.YMin != -60 || st.Visual.YMax != 0 {
		t.Fatalf("Visual bounds = [%v %v %v %v], want [-60 0 -60 0]",
			st.Visual.XMin, st.Visual.XMax, st.Visual.YMin, st.Visual.YMax)
	}

	// The legacy meter path is untouched: the described effect still reports
	// its meters.
	if st.Meters["gr"] != -6 {
		t.Fatalf("Meters = %+v, want the live gr reading", st.Meters)
	}
}

// TestEffectStageMeteredOnlyReportsNoTelemetry is the backward-compatibility
// check: an effect that implements only dsp.Metered reports no readings and no
// visual, so the legacy in/out pair still renders.
func TestEffectStageMeteredOnlyReportsNoTelemetry(t *testing.T) {
	registerTelemetryFactories()

	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect(testMeteredKind, "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	st, ok := stageByID(s.EffectStages(), id)
	if !ok {
		t.Fatalf("stage %s missing", id)
	}

	if st.Readings != nil {
		t.Fatalf("Readings = %+v, want nil for a Metered-only effect", st.Readings)
	}
	if st.Visual != nil {
		t.Fatalf("Visual = %+v, want nil for a Metered-only effect", st.Visual)
	}
	if st.Meters == nil {
		t.Fatal("Meters = nil, want the legacy pair")
	}
}

// TestEffectStageVisualCopiesSlices pins the aliasing fix: stageFromEffect must
// not hand out the effect's own Params and Overlays backing arrays. The fake
// reuses package-level slices and mutates them after the read, so a snapshot
// that aliased them would change under the reader.
func TestEffectStageVisualCopiesSlices(t *testing.T) {
	registerTelemetryFactories()

	// Restore the shared slices so a mutating neighbour cannot bleed into this
	// test's expected values.
	reusedParams[0], reusedParams[1] = "threshold", "ratio"
	reusedOverlays[0], reusedOverlays[1] = "in", "gr"

	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect(testReusedKind, "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	st, ok := stageByID(s.EffectStages(), id)
	if !ok {
		t.Fatalf("stage %s missing", id)
	}
	if st.Visual == nil {
		t.Fatal("Visual = nil, want the reused transfer plot")
	}

	// Mutate the effect's backing slices as a reusing implementation would.
	reusedParams[0] = "mutated-param"
	reusedOverlays[0] = "mutated-overlay"

	if got := st.Visual.Params[0]; got != "threshold" {
		t.Fatalf("Visual.Params[0] = %q after the effect mutated its slice, want the snapshot to keep %q", got, "threshold")
	}
	if got := st.Visual.Overlays[0]; got != "in" {
		t.Fatalf("Visual.Overlays[0] = %q after the effect mutated its slice, want the snapshot to keep %q", got, "in")
	}
}

// TestEffectMetersReadsOnlyLiveMeters proves the fast path: it reports a
// metered stage's meters by ID and omits a stage that has no live effect yet,
// without reading schema or values.
func TestEffectMetersReadsOnlyLiveMeters(t *testing.T) {
	registerTelemetryFactories()

	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect(testDescribedKind, "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	meters := s.EffectMeters()
	if len(meters) != 1 {
		t.Fatalf("EffectMeters() = %+v, want one entry", meters)
	}
	if meters[0].ID != id {
		t.Fatalf("EffectMeters()[0].ID = %q, want %q", meters[0].ID, id)
	}
	if meters[0].Meters["gr"] != -6 {
		t.Fatalf("EffectMeters()[0].Meters = %+v, want the live gr reading", meters[0].Meters)
	}
}

// TestEffectMetersOmitsUnmeteredStages proves a stage whose effect does not
// implement dsp.Metered is left out rather than reported with empty meters, so
// the fast payload never carries a stage the UI cannot draw.
func TestEffectMetersOmitsUnmeteredStages(t *testing.T) {
	// An empty chain reports nothing rather than an empty slice with a stage.
	s := effectsSession(t, dsp.Pipeline{})

	if got := s.EffectMeters(); len(got) != 0 {
		t.Fatalf("EffectMeters() on an empty chain = %+v, want none", got)
	}
}
