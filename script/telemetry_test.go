package script

// Tests for the telemetry surface: a meter node publishes a named reading, and
// ctx.visual declares a plot. The load errors are pinned here too, because an
// undeclared reference must fail at Load rather than silently draw nothing.

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/dsp"
)

// TestCompressorDeclaresReadingAndVisual loads the bundled compressor and pins
// the three declarations it makes: the schema params, the gr reading and the
// transfer visual.
func TestCompressorDeclaresReadingAndVisual(t *testing.T) {
	f, err := Load(filepath.Join("testdata", "compressor.lua"))
	if err != nil {
		t.Fatalf("Load(compressor.lua): %v", err)
	}

	keys := map[string]dsp.Param{}
	for _, p := range f.Schema() {
		keys[p.Key] = p
	}
	for _, want := range []string{"threshold", "ratio", "knee", "attack", "release", "makeup", "mix", "lookahead"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("compressor schema is missing %q; has %v", want, schemaKeys(f.Schema()))
		}
	}

	e, err := f.New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ef := e.(*Effect)
	if _, err := ef.Configure(core.FrameFormat{Rate: testRate, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { _ = ef.Close() })

	readings := ef.Readings()
	if len(readings) != 1 {
		t.Fatalf("Readings() returned %d readings, want 1: %+v", len(readings), readings)
	}
	gr := readings[0]
	t.Logf("compressor reading: %+v", gr)
	if gr.Key != "gr" {
		t.Fatalf("reading key = %q, want gr", gr.Key)
	}
	if gr.Label != "Gain Reduction" || gr.Unit != "dB" {
		t.Fatalf("reading metadata lost: %+v", gr)
	}
	if gr.Min != -30 || gr.Max != 0 {
		t.Fatalf("reading range = [%v, %v], want [-30, 0]", gr.Min, gr.Max)
	}
	if gr.Kind != dsp.ReadingGainReduction {
		t.Fatalf("reading kind = %q, want %q", gr.Kind, dsp.ReadingGainReduction)
	}

	visuals := ef.Visuals()
	if len(visuals) != 2 {
		t.Fatalf("Visuals() returned %d visuals, want 2: %+v", len(visuals), visuals)
	}
	visual := visuals[0]
	t.Logf("compressor transfer visual: %+v", visual)
	if visual.Kind != dsp.VisualTransfer {
		t.Fatalf("visual kind = %q, want %q", visual.Kind, dsp.VisualTransfer)
	}
	if got := strings.Join(visual.Params, ","); got != "threshold,ratio,makeup,knee" {
		t.Fatalf("visual params = %q, want threshold,ratio,makeup,knee", got)
	}
	if got := strings.Join(visual.Overlays, ","); got != "in,gr" {
		t.Fatalf("visual overlays = %q, want in,gr", got)
	}
	if visual.XMin != -60 || visual.XMax != 0 || visual.YMin != -60 || visual.YMax != 0 {
		t.Fatalf("visual range lost: %+v", visual)
	}

	// The second visual is the scrolling dynamics display over the in/out pair
	// and the gain-reduction reading.
	dyn := visuals[1]
	t.Logf("compressor dynamics visual: %+v", dyn)
	if dyn.Kind != dsp.VisualDynamics {
		t.Fatalf("dynamics kind = %q, want %q", dyn.Kind, dsp.VisualDynamics)
	}
	if got := strings.Join(dyn.Params, ","); got != "" {
		t.Fatalf("dynamics params = %q, want empty", got)
	}
	if got := strings.Join(dyn.Overlays, ","); got != "in,out,gr" {
		t.Fatalf("dynamics overlays = %q, want in,out,gr", got)
	}
	if dyn.YMin != -60 || dyn.YMax != 0 {
		t.Fatalf("dynamics range lost: %+v", dyn)
	}
}

// TestCompressorMeterPublishesGainReduction runs a loud tone through the
// compressor and checks the declared reading actually shows up under its key
// and is the reduction the law implies.
func TestCompressorMeterPublishesGainReduction(t *testing.T) {
	// A 0 dBFS tone against a -18 dB threshold with a 4:1 ratio is 18 dB over,
	// so the reduction is 18 * (1 - 1/4) = 13.5 dB.
	e := loadEffect(t, "compressor.lua", dsp.Values{
		"threshold": -18.0,
		"ratio":     4.0,
		"attack":    0.001,
		"release":   0.05,
		"makeup":    0.0,
		"mix":       1.0,
	})
	buf := sine(48000, 1000, 1.0)
	if err := e.Process(buf, 48000); err != nil {
		t.Fatalf("Process: %v", err)
	}
	m := e.Meters()
	gr, ok := m["gr"]
	if !ok {
		t.Fatalf("Meters has no gr key: %v", m)
	}
	t.Logf("compressor gr meter = %.2f dB (want about -13.5)", gr)
	if !(gr < -10 && gr > -16) {
		t.Fatalf("gr reading %.2f dB is not the law's reduction", gr)
	}
}

// TestVisualParamMustBeDeclared pins the params check: a visual that derives its
// curve from a key the schema does not have must fail to load.
func TestVisualParamMustBeDeclared(t *testing.T) {
	_, err := Load(writeScript(t, `return effect {
  name = "BadParam",
  params = { threshold = { float, min = -60, max = 0, default = -18 } },
  build = function(ctx)
    ctx.out(ctx.input())
    ctx.visual { kind = "transfer", params = { "threshold", "ratio" }, overlays = {} }
  end,
}`))
	t.Logf("undeclared visual param: %v", err)
	if err == nil {
		t.Fatal("a visual param naming an undeclared key loaded")
	}
	if !strings.Contains(err.Error(), "ratio") {
		t.Fatalf("error does not name the undeclared key: %v", err)
	}
}

// TestVisualOverlayMustBeDeclared pins the overlays check: an overlay must name
// a declared reading (or the standard in/out pair), so a typo fails to load
// rather than drawing nothing.
func TestVisualOverlayMustBeDeclared(t *testing.T) {
	_, err := Load(writeScript(t, `return effect {
  name = "BadOverlay",
  params = { threshold = { float, min = -60, max = 0, default = -18 } },
  build = function(ctx)
    local gr = ctx.meter("gr", { label = "Gain Reduction", unit = "dB", min = -30, max = 0, kind = "gain-reduction" })
    gr(ctx.input())
    ctx.out(ctx.input())
    ctx.visual { kind = "transfer", params = {}, overlays = { "in", "nope" } }
  end,
}`))
	t.Logf("undeclared visual overlay: %v", err)
	if err == nil {
		t.Fatal("an overlay naming an undeclared reading loaded")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error does not name the undeclared overlay: %v", err)
	}
}

// TestMeterPassesThrough checks the meter is observational: its output is its
// input, so wiring one in must not alter the signal.
func TestMeterPassesThrough(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Pass",
  build = function(ctx)
    local m = ctx.meter("level", { label = "Level", unit = "dB", min = -60, max = 0, kind = "level" })
    ctx.out(m(ctx.input()))
  end,
}`, nil)
	buf := sine(960, 1000, 0.5)
	want := append([]float32(nil), buf...)
	if err := e.Process(buf, 960); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range buf {
		if buf[i] != want[i] {
			t.Fatalf("meter changed sample %d: %v vs %v", i, buf[i], want[i])
		}
	}
	if _, ok := e.Meters()["level"]; !ok {
		t.Fatalf("meter published no level key: %v", e.Meters())
	}
}

// TestUnknownReadingKindFailsLoad pins that a meter's kind is validated.
func TestUnknownReadingKindFailsLoad(t *testing.T) {
	_, err := Load(writeScript(t, `return effect {
  name = "BadKind",
  build = function(ctx)
    local m = ctx.meter("x", { label = "X", unit = "", min = 0, max = 1, kind = "sparkle" })
    m(ctx.input())
    ctx.out(ctx.input())
  end,
}`))
	t.Logf("unknown reading kind: %v", err)
	if err == nil {
		t.Fatal("a meter with an unknown kind loaded")
	}
	if !strings.Contains(err.Error(), "sparkle") {
		t.Fatalf("error does not name the unknown kind: %v", err)
	}
}

// B1: a meter key that reuses a standard meter key would clobber the level the
// UI draws under that key. It must be rejected with the reserved key named.
func TestMeterReservedKeyRejected(t *testing.T) {
	for _, key := range []string{dsp.MeterIn, dsp.MeterOut} {
		_, err := Load(writeScript(t, `return effect {
  name = "Hijack",
  build = function(ctx)
    local m = ctx.meter("`+key+`", { label = "Hijack", unit = "dB", min = -60, max = 0, kind = "level" })
    m(ctx.input())
    ctx.out(ctx.input())
  end,
}`))
		t.Logf("meter key %q: %v", key, err)
		if err == nil {
			t.Fatalf("a meter reusing the reserved key %q loaded", key)
		}
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error does not name the reserved key %q: %v", key, err)
		}
	}
}

// N1: an unknown metadata key is a typo such as `maxx`, and dropping it would
// silently loosen the range the UI draws. It must fail to load.
func TestMeterUnknownMetadataRejected(t *testing.T) {
	_, err := Load(writeScript(t, `return effect {
  name = "TypoMeta",
  build = function(ctx)
    local m = ctx.meter("gr", { label = "GR", unit = "dB", min = -30, maxx = 0, kind = "gain-reduction" })
    m(ctx.input())
    ctx.out(ctx.input())
  end,
}`))
	t.Logf("unknown meter metadata: %v", err)
	if err == nil {
		t.Fatal("a meter with a misspelled metadata key loaded")
	}
	if !strings.Contains(err.Error(), "maxx") {
		t.Fatalf("error does not name the unknown metadata key: %v", err)
	}
}

// S1: a non-finite visual range would make the bridge's json.Marshal fail at
// render time, reachable from untrusted input. It must fail to load instead,
// naming the field.
func TestNonFiniteVisualRangeRejected(t *testing.T) {
	for field, expr := range map[string]string{
		"xMin": "0/0",
		"xMax": "1/0",
		"yMin": "-1/0",
		"yMax": "0/0",
	} {
		export := map[string]string{
			"xMin": "xMin = " + expr,
			"xMax": "xMax = " + expr,
			"yMin": "yMin = " + expr,
			"yMax": "yMax = " + expr,
		}[field]
		_, err := Load(writeScript(t, `return effect {
  name = "BadRange",
  build = function(ctx)
    ctx.out(ctx.input())
    ctx.visual { kind = "transfer", params = {}, overlays = {}, `+export+` }
  end,
}`))
		t.Logf("visual %s: %v", field, err)
		if err == nil {
			t.Fatalf("a visual with a non-finite %s loaded", field)
		}
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("error does not name the non-finite field %q: %v", field, err)
		}
	}
}

// S3: an overlay may name a reading a meter declares after the visual. The
// check runs once the whole graph is built, so the order must not matter.
func TestOverlayDeclaredAfterVisualLoads(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "LateMeter",
  build = function(ctx)
    ctx.visual { kind = "transfer", params = {}, overlays = { "in", "gr" } }
    local m = ctx.meter("gr", { label = "Gain Reduction", unit = "dB", min = -30, max = 0, kind = "gain-reduction" })
    m(ctx.input())
    ctx.out(ctx.input())
  end,
}`, nil)
	if got := strings.Join(e.Visuals()[0].Overlays, ","); got != "in,gr" {
		t.Fatalf("overlays = %q, want in,gr", got)
	}
}

// S3: an overlay that names no reading after the whole build is still a load
// error, and it names the overlay key.
func TestOverlayGenuinelyAbsentStillRejected(t *testing.T) {
	_, err := Load(writeScript(t, `return effect {
  name = "NoReading",
  build = function(ctx)
    ctx.visual { kind = "transfer", params = {}, overlays = { "nope" } }
    ctx.out(ctx.input())
  end,
}`))
	t.Logf("genuinely absent overlay: %v", err)
	if err == nil {
		t.Fatal("an overlay naming no declared reading loaded")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error does not name the overlay key: %v", err)
	}
}

// S4: a map-form list has length zero, so a naive read would accept it as an
// empty list and hide a typo. It must be rejected.
func TestMapFormListRejected(t *testing.T) {
	_, err := Load(writeScript(t, `return effect {
  name = "MapList",
  build = function(ctx)
    ctx.out(ctx.input())
    ctx.visual { kind = "transfer", params = { bogus = "nope" }, overlays = {} }
  end,
}`))
	t.Logf("map-form params list: %v", err)
	if err == nil {
		t.Fatal("a map-form params list loaded as empty")
	}
	if !strings.Contains(err.Error(), "params") {
		t.Fatalf("error does not name the offending list: %v", err)
	}
}

// S4: a proper array still loads, and a nil/absent list is still accepted.
func TestArrayAndAbsentListsAccepted(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Arrays",
  params = { gain = { float, min = 0, max = 1, default = 1 } },
  build = function(ctx)
    ctx.out(ctx.input() * ctx.param("gain"))
    ctx.visual { kind = "transfer", params = { "gain" }, overlays = { "in" } }
  end,
}`, nil)
	v := e.Visuals()[0]
	if got := strings.Join(v.Params, ","); got != "gain" {
		t.Fatalf("params = %q, want gain", got)
	}
	if got := strings.Join(v.Overlays, ","); got != "in" {
		t.Fatalf("overlays = %q, want in", got)
	}

	// A visual with both lists omitted must still load.
	buildConfigured(t, `return effect {
  name = "Omitted",
  build = function(ctx)
    ctx.out(ctx.input())
    ctx.visual { kind = "transfer" }
  end,
}`, nil)
}

// S5: Readings() is documented as an empty slice, so it must marshal as [] and
// never null.
func TestReadingsJSONEmptyNotNil(t *testing.T) {
	e := loadEffect(t, "lowpass.lua", nil)
	if got := e.Readings(); got == nil {
		t.Fatal("Readings() returned nil, want an empty slice")
	}
	b, err := json.Marshal(e.Readings())
	if err != nil {
		t.Fatalf("json.Marshal(Readings()): %v", err)
	}
	t.Logf("empty Readings JSON: %s", b)
	if string(b) != "[]" {
		t.Fatalf("empty Readings marshalled as %s, want []", b)
	}
}

// S6: a visual with lists omitted must marshal its lists as [] to match an
// explicitly empty list, so the bridge's JSON is stable.
func TestVisualJSONListsEmptyNotNil(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "NoLists",
  build = function(ctx)
    ctx.out(ctx.input())
    ctx.visual { kind = "transfer" }
  end,
}`, nil)
	v := e.Visuals()[0]
	if v.Params == nil || v.Overlays == nil {
		t.Fatalf("Visual() lists are nil: %+v", v)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(Visual()): %v", err)
	}
	t.Logf("omitted-list Visual JSON: %s", b)
	if strings.Contains(string(b), "null") {
		t.Fatalf("Visual marshalled a null list: %s", b)
	}
}

// N2: a visual param naming an undeclared key reports the visual, not the
// generic build reference.
func TestVisualParamErrorNamesVisual(t *testing.T) {
	_, err := Load(writeScript(t, `return effect {
  name = "BadVisualParam",
  params = { threshold = { float, min = -60, max = 0, default = -18 } },
  build = function(ctx)
    ctx.out(ctx.input())
    ctx.visual { kind = "transfer", params = { "ratio" }, overlays = {} }
  end,
}`))
	t.Logf("undeclared visual param: %v", err)
	if err == nil {
		t.Fatal("a visual param naming an undeclared key loaded")
	}
	if !strings.Contains(err.Error(), "visual") || !strings.Contains(err.Error(), "ratio") {
		t.Fatalf("error does not name the visual and the key: %v", err)
	}
}

// S2: a bypassed effect runs no plan, so its readings must be republished as
// zero rather than frozen at their last pre-bypass value.
func TestBypassZeroesReadings(t *testing.T) {
	e := buildConfigured(t, `return effect {
  name = "Reduction",
  build = function(ctx)
    local m = ctx.meter("gr", { label = "GR", unit = "dB", min = -30, max = 0, kind = "gain-reduction" })
    m(ctx.const(-9.0))
    ctx.out(ctx.input())
  end,
}`, nil)
	buf := sine(4800, 1000, 0.5)
	if err := e.Process(buf, 4800); err != nil {
		t.Fatalf("Process: %v", err)
	}
	before := e.Meters()["gr"]
	t.Logf("gr before bypass: %.2f", before)
	if before == 0 {
		t.Fatalf("gr is zero before bypass, the fixture is not measuring")
	}

	if err := e.Set(dsp.ParamBypass, true); err != nil {
		t.Fatalf("Set(bypass): %v", err)
	}
	if err := e.Process(buf, 4800); err != nil {
		t.Fatalf("Process bypassed: %v", err)
	}
	after := e.Meters()["gr"]
	t.Logf("gr after bypass: %.2f", after)
	if after != 0 {
		t.Fatalf("bypassed reading = %.2f, want 0 (stale value not cleared)", after)
	}
	// The standard pair still measures the audio a bypass passes.
	if _, ok := e.Meters()[dsp.MeterOut]; !ok {
		t.Fatalf("standard out meter missing while bypassed: %v", e.Meters())
	}
	if math.IsNaN(float64(after)) {
		t.Fatal("bypassed reading is NaN")
	}
}
