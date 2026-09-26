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
