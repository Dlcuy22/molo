package main

import (
	"encoding/json"
	"testing"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/dsp"
)

// The wire shape is a contract the frontend reads by name, so it is pinned here
// rather than left to the struct tags drifting from effect-types.ts.
func TestEffectParamInfoWireShape(t *testing.T) {
	p := dsp.Param{
		Key:     "rate",
		Kind:    dsp.Enum,
		Min:     0,
		Max:     10,
		Step:    1,
		Unit:    "Hz",
		Options: []string{"slow", "fast"},
		Default: "slow",
		Label:   "Rate",
		Group:   "Tremolo",
		Widget:  dsp.WidgetSelect,
	}

	raw, err := json.Marshal(effectParamInfo(p))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]any{
		"key": "rate", "kind": float64(dsp.Enum), "min": float64(0), "max": float64(10),
		"step": float64(1), "unit": "Hz", "label": "Rate", "group": "Tremolo", "widget": "select",
		"default": "slow",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("json[%q] = %v, want %v", k, got[k], v)
		}
	}
	opts, ok := got["options"].([]any)
	if !ok || len(opts) != 2 || opts[0] != "slow" || opts[1] != "fast" {
		t.Errorf("options = %v, want [slow fast]", got["options"])
	}
}

// A param with no options must still marshal an array, not null: the frontend
// treats null and empty differently when deciding whether to render a select.
func TestEffectParamInfoOptionsAreNeverNull(t *testing.T) {
	raw, err := json.Marshal(effectParamInfo(dsp.Param{Key: "x"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["options"]) != "[]" {
		t.Fatalf("options = %s, want []", got["options"])
	}
}

// A stage with no meters must marshal null, which is how the UI knows the
// effect does not meter rather than that it is silent.
func TestEffectStageInfoNilMetersAreNull(t *testing.T) {
	raw, err := json.Marshal(effectStageInfo(player.EffectStage{
		ID: "s1", Kind: "crossfeed", Label: "Crossfeed", Meters: nil,
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["meters"]) != "null" {
		t.Fatalf("meters = %s, want null", got["meters"])
	}
	if string(got["schema"]) != "[]" {
		t.Fatalf("schema = %s, want []", got["schema"])
	}
}

// A stage carrying readings and a visual must map into the DTO with the field
// names the frontend reads, and the dsp kind constants must cross as their
// string values, not their iota order.
func TestEffectStageInfoReadingsAndVisual(t *testing.T) {
	stage := effectStageInfo(player.EffectStage{
		ID:    "s1",
		Kind:  "compressor",
		Label: "Compressor",
		Readings: []dsp.Reading{{
			Key:   "gr",
			Label: "Gain Reduction",
			Unit:  "dB",
			Min:   -30,
			Max:   0,
			Kind:  dsp.ReadingGainReduction,
		}},
		Visual: &dsp.Visual{
			Kind:     dsp.VisualTransfer,
			Params:   []string{"threshold", "ratio"},
			Overlays: []string{"in", "gr"},
			XMin:     -60,
			XMax:     0,
			YMin:     -48,
			YMax:     6,
		},
	})

	raw, err := json.Marshal(stage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	readings, ok := got["readings"].([]any)
	if !ok || len(readings) != 1 {
		t.Fatalf("readings = %v, want one entry", got["readings"])
	}
	reading, ok := readings[0].(map[string]any)
	if !ok {
		t.Fatalf("readings[0] = %v, want object", readings[0])
	}
	wantReading := map[string]any{
		"key": "gr", "label": "Gain Reduction", "unit": "dB",
		"min": float64(-30), "max": float64(0), "kind": "gain-reduction",
	}
	for k, v := range wantReading {
		if reading[k] != v {
			t.Errorf("readings[0][%q] = %v, want %v", k, reading[k], v)
		}
	}

	visual, ok := got["visual"].(map[string]any)
	if !ok {
		t.Fatalf("visual = %v, want object", got["visual"])
	}
	wantVisual := map[string]any{
		"kind": "transfer",
		"xMin": float64(-60), "xMax": float64(0),
		"yMin": float64(-48), "yMax": float64(6),
	}
	for k, v := range wantVisual {
		if visual[k] != v {
			t.Errorf("visual[%q] = %v, want %v", k, visual[k], v)
		}
	}
	params, ok := visual["params"].([]any)
	if !ok || len(params) != 2 || params[0] != "threshold" || params[1] != "ratio" {
		t.Errorf("visual[params] = %v, want [threshold ratio]", visual["params"])
	}
	overlays, ok := visual["overlays"].([]any)
	if !ok || len(overlays) != 2 || overlays[0] != "in" || overlays[1] != "gr" {
		t.Errorf("visual[overlays] = %v, want [in gr]", visual["overlays"])
	}
}

// A stage with no readings and no visual must still marshal readings as an
// empty array (the frontend loops over it) and visual as null (which is how the
// UI knows not to mount a plot).
func TestEffectStageInfoNoReadingsOrVisual(t *testing.T) {
	raw, err := json.Marshal(effectStageInfo(player.EffectStage{
		ID: "s1", Kind: "crossfeed", Label: "Crossfeed",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["readings"]) != "[]" {
		t.Fatalf("readings = %s, want []", got["readings"])
	}
	if string(got["visual"]) != "null" {
		t.Fatalf("visual = %s, want null", got["visual"])
	}
}

// A visual with nil params and overlays must marshal both as arrays, not null:
// the frontend treats null and empty differently when deriving the curve.
func TestEffectVisualInfoSlicesAreNeverNull(t *testing.T) {
	raw, err := json.Marshal(effectVisualInfo(&dsp.Visual{Kind: dsp.VisualTransfer}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["params"]) != "[]" {
		t.Fatalf("params = %s, want []", got["params"])
	}
	if string(got["overlays"]) != "[]" {
		t.Fatalf("overlays = %s, want []", got["overlays"])
	}
}

// The bridge does not validate the visual kind; the UI decides how to draw an
// unknown one. A well-formed object must cross verbatim, with params and
// overlays normalized to arrays like any other visual.
func TestEffectVisualInfoUnknownKindPassesThrough(t *testing.T) {
	raw, err := json.Marshal(effectVisualInfo(&dsp.Visual{
		Kind: "spectrogram",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["kind"] != "spectrogram" {
		t.Errorf("kind = %v, want spectrogram", got["kind"])
	}
	params, ok := got["params"].([]any)
	if !ok || len(params) != 0 {
		t.Errorf("params = %v, want []", got["params"])
	}
	overlays, ok := got["overlays"].([]any)
	if !ok || len(overlays) != 0 {
		t.Errorf("overlays = %v, want []", got["overlays"])
	}
}

// stageFixture builds a minimal engine stage for the mapping tests. It is not a
// session, so the mapping is tested without a player or an audio device.
func stageFixture(meters map[string]float32) EffectStageInfo {
	return effectStageInfo(player.EffectStage{
		ID:       "s1",
		Kind:     "crossfeed",
		Impl:     "",
		Label:    "Crossfeed",
		Bypassed: false,
		Schema:   nil,
		Values:   map[string]any{"cutoff": 700.0},
		Meters:   meters,
	})
}
