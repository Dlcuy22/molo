// Clean-room note: tests verify EasyEffects preset parsing against independent fixtures.

package dsp

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func eeTestContainsWarning(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func eeTestRegisterKernel(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kernel.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0}, 48000, 2, 3, 32)
	if err := RegisterKernel(name, path); err != nil {
		t.Fatalf("RegisterKernel(%q): %v", name, err)
	}
}

func TestEEPresetRealFiles(t *testing.T) {
	const preset1Path = "/home/kasaki/Downloads/Jpop bass boosted.json"
	const preset2Path = "/home/kasaki/Downloads/Bass Enhancing + Perfect EQ - Low Latency.json"

	t.Run("Jpop_bass_boosted", func(t *testing.T) {
		if _, err := os.Stat(preset1Path); errors.Is(err, os.ErrNotExist) {
			t.Skipf("preset file not found: %s", preset1Path)
		}

		// First pass tests behavior when the named kernel is not registered.
		pipeline, warnings, err := LoadEasyEffectsPresetFile(preset1Path)
		if err != nil {
			t.Fatalf("LoadEasyEffectsPresetFile: %v", err)
		}

		expectedOrder := []string{"equalizer", "convolver", "bass_enhancer", "crossfeed", "maximizer"}
		if len(pipeline.Post) != len(expectedOrder) {
			t.Fatalf("post stages count = %d, want %d", len(pipeline.Post), len(expectedOrder))
		}
		for i, id := range expectedOrder {
			if pipeline.Post[i].ID != id {
				t.Errorf("stage %d ID = %q, want %q", i, pipeline.Post[i].ID, id)
			}
		}

		expectedKinds := []string{"equalizer", "convolver", "bass-enhancer", "crossfeed", "maximizer"}
		for i, kind := range expectedKinds {
			if pipeline.Post[i].Kind != kind {
				t.Errorf("stage %d Kind = %q, want %q", i, pipeline.Post[i].Kind, kind)
			}
		}

		// Assertions on actual mapped parameter values.
		eqParams := pipeline.Post[0].Params
		if eqParams[EqualizerBandGainKey(0)] != 4.0 {
			t.Errorf("equalizer band0 gain = %v, want 4.0", eqParams[EqualizerBandGainKey(0)])
		}
		if eqParams[EqualizerBandGainKey(9)] != 3.0 {
			t.Errorf("equalizer band9 gain = %v, want 3.0", eqParams[EqualizerBandGainKey(9)])
		}
		if eqParams[EqualizerBandFreqKey(0)] != 32.0 {
			t.Errorf("equalizer band0 freq = %v, want 32.0", eqParams[EqualizerBandFreqKey(0)])
		}
		if eqParams[EqualizerBandFreqKey(9)] != 16000.0 {
			t.Errorf("equalizer band9 freq = %v, want 16000.0", eqParams[EqualizerBandFreqKey(9)])
		}
		if eqParams[ParamInputGain] != -2.0 {
			t.Errorf("equalizer input-gain = %v, want -2.0", eqParams[ParamInputGain])
		}
		if eqParams[EqualizerNumBands] != 10 {
			t.Errorf("equalizer num-bands = %v, want 10", eqParams[EqualizerNumBands])
		}

		convParams := pipeline.Post[1].Params
		if convParams[ConvolverIRWidth] != 100.0 {
			t.Errorf("convolver ir-width = %v, want 100", convParams[ConvolverIRWidth])
		}
		if convParams[ParamInputGain] != -2.0 {
			t.Errorf("convolver input-gain = %v, want -2.0", convParams[ParamInputGain])
		}

		bassParams := pipeline.Post[2].Params
		if bassParams[BassEnhancerAmount] != 5.0 {
			t.Errorf("bass enhancer amount = %v, want 5.0", bassParams[BassEnhancerAmount])
		}
		if bassParams[BassEnhancerHarmonics] != 8.5 {
			t.Errorf("bass enhancer harmonics = %v, want 8.5", bassParams[BassEnhancerHarmonics])
		}
		if bassParams[BassEnhancerScope] != 80.0 {
			t.Errorf("bass enhancer scope = %v, want 80.0", bassParams[BassEnhancerScope])
		}

		cfParams := pipeline.Post[3].Params
		if cfParams[CrossfeedCutoff] != 700.0 {
			t.Errorf("crossfeed cutoff = %v, want 700.0", cfParams[CrossfeedCutoff])
		}
		if cfParams[CrossfeedFeed] != 4.5 {
			t.Errorf("crossfeed feed = %v, want 4.5", cfParams[CrossfeedFeed])
		}

		maxParams := pipeline.Post[4].Params
		if thresh, ok := maxParams[MaximizerThreshold].(float64); !ok || math.Abs(thresh-(-3.0)) > 0.01 {
			t.Errorf("maximizer threshold = %v, want -3.0", maxParams[MaximizerThreshold])
		}
		if rel, ok := maxParams[MaximizerRelease].(float64); !ok || math.Abs(rel-3.15) > 0.01 {
			t.Errorf("maximizer release = %v, want ~3.15", maxParams[MaximizerRelease])
		}
		if maxParams[MaximizerCeiling] != 0.0 {
			t.Errorf("maximizer ceiling = %v, want 0.0", maxParams[MaximizerCeiling])
		}

		if !eeTestContainsWarning(warnings, "convolver.kernel-name skipped: kernel") {
			t.Errorf("expected warning for unregistered convolver kernel")
		}

		effects, err := NewEffects(pipeline.Post)
		if err != nil {
			t.Fatalf("NewEffects: %v", err)
		}
		if len(effects) != len(pipeline.Post) {
			t.Errorf("built effects count = %d, want %d", len(effects), len(pipeline.Post))
		}

		// Second pass registers kernel name and verifies clean mapping without warning.
		const kName = "Razor Surround ((48k Z-Edition)) 2.Stereo +20 bass"
		eeTestRegisterKernel(t, kName)

		pipeline2, warnings2, err := LoadEasyEffectsPresetFile(preset1Path)
		if err != nil {
			t.Fatalf("LoadEasyEffectsPresetFile registered: %v", err)
		}
		if eeTestContainsWarning(warnings2, "convolver.kernel-name skipped") {
			t.Errorf("unexpected warning for registered kernel: %v", warnings2)
		}
		if got := pipeline2.Post[1].Params[ConvolverKernel]; got != kName {
			t.Errorf("convolver kernel = %v, want %q", got, kName)
		}

		effects2, err := NewEffects(pipeline2.Post)
		if err != nil {
			t.Fatalf("NewEffects after register: %v", err)
		}
		if len(effects2) != len(pipeline2.Post) {
			t.Errorf("built effects count = %d, want %d", len(effects2), len(pipeline2.Post))
		}
	})

	t.Run("Bass_Enhancing_Perfect_EQ", func(t *testing.T) {
		if _, err := os.Stat(preset2Path); errors.Is(err, os.ErrNotExist) {
			t.Skipf("preset file not found: %s", preset2Path)
		}

		pipeline, warnings, err := LoadEasyEffectsPresetFile(preset2Path)
		if err != nil {
			t.Fatalf("LoadEasyEffectsPresetFile: %v", err)
		}

		expectedOrder := []string{"equalizer#0", "convolver#0", "limiter#0"}
		if len(pipeline.Post) != len(expectedOrder) {
			t.Fatalf("post stages count = %d, want %d", len(pipeline.Post), len(expectedOrder))
		}
		for i, id := range expectedOrder {
			if pipeline.Post[i].ID != id {
				t.Errorf("stage %d ID = %q, want %q", i, pipeline.Post[i].ID, id)
			}
		}

		expectedKinds := []string{"equalizer", "convolver", "limiter"}
		for i, kind := range expectedKinds {
			if pipeline.Post[i].Kind != kind {
				t.Errorf("stage %d Kind = %q, want %q", i, pipeline.Post[i].Kind, kind)
			}
		}

		// Assertions on actual mapped parameter values.
		eqParams := pipeline.Post[0].Params
		if eqParams[ParamInputGain] != 0.0 {
			t.Errorf("equalizer input-gain = %v, want 0.0", eqParams[ParamInputGain])
		}
		if eqParams[EqualizerNumBands] != 10 {
			t.Errorf("equalizer num-bands = %v, want 10", eqParams[EqualizerNumBands])
		}
		if eqParams[EqualizerBandGainKey(0)] != 4.0 {
			t.Errorf("equalizer band0 gain = %v, want 4.0", eqParams[EqualizerBandGainKey(0)])
		}
		if eqParams[EqualizerBandGainKey(9)] != 3.0 {
			t.Errorf("equalizer band9 gain = %v, want 3.0", eqParams[EqualizerBandGainKey(9)])
		}

		convParams := pipeline.Post[1].Params
		if convParams[ConvolverIRWidth] != 100.0 {
			t.Errorf("convolver ir-width = %v, want 100", convParams[ConvolverIRWidth])
		}

		limParams := pipeline.Post[2].Params
		if limParams[LimiterThreshold] != 0.0 {
			t.Errorf("limiter threshold = %v, want 0.0", limParams[LimiterThreshold])
		}
		if limParams[LimiterLookahead] != 5.0 {
			t.Errorf("limiter lookahead = %v, want 5.0", limParams[LimiterLookahead])
		}
		if limParams[LimiterStereoLink] != 100.0 {
			t.Errorf("limiter stereo-link = %v, want 100.0", limParams[LimiterStereoLink])
		}
		if limParams[LimiterAttack] != 5.0 {
			t.Errorf("limiter attack = %v, want 5.0", limParams[LimiterAttack])
		}
		if limParams[LimiterRelease] != 5.0 {
			t.Errorf("limiter release = %v, want 5.0", limParams[LimiterRelease])
		}
		if limParams[LimiterMode] != "Herm Thin" {
			t.Errorf("limiter mode = %v, want Herm Thin", limParams[LimiterMode])
		}
		if limParams[LimiterOversampling] != "None" {
			t.Errorf("limiter oversampling = %v, want None", limParams[LimiterOversampling])
		}

		if !eeTestContainsWarning(warnings, "limiter#0.alr skipped: ALR not supported") {
			t.Errorf("expected warning for limiter ALR parameters")
		}

		effects, err := NewEffects(pipeline.Post)
		if err != nil {
			t.Fatalf("NewEffects: %v", err)
		}
		if len(effects) != len(pipeline.Post) {
			t.Errorf("built effects count = %d, want %d", len(effects), len(pipeline.Post))
		}
	})
}

func TestEEPresetHandwrittenCrossfeed(t *testing.T) {
	raw := `{
		"output": {
			"crossfeed": {
				"fcut": 850.0,
				"feed": 6.2,
				"bypass": true,
				"input-gain": -1.5,
				"output-gain": 0.5
			},
			"plugins_order": [
				"crossfeed"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(pipeline.Post) != 1 {
		t.Fatalf("post stage count = %d, want 1", len(pipeline.Post))
	}

	stage := pipeline.Post[0]
	if stage.ID != "crossfeed" {
		t.Errorf("stage ID = %q, want crossfeed", stage.ID)
	}
	if stage.Kind != "crossfeed" {
		t.Errorf("stage Kind = %q, want crossfeed", stage.Kind)
	}
	if stage.Params[CrossfeedCutoff] != 850.0 {
		t.Errorf("cutoff = %v, want 850.0", stage.Params[CrossfeedCutoff])
	}
	if stage.Params[CrossfeedFeed] != 6.2 {
		t.Errorf("feed = %v, want 6.2", stage.Params[CrossfeedFeed])
	}
	if stage.Params[ParamBypass] != true {
		t.Errorf("bypass = %v, want true", stage.Params[ParamBypass])
	}
	if stage.Params[ParamInputGain] != -1.5 {
		t.Errorf("input-gain = %v, want -1.5", stage.Params[ParamInputGain])
	}
	if stage.Params[ParamOutputGain] != 0.5 {
		t.Errorf("output-gain = %v, want 0.5", stage.Params[ParamOutputGain])
	}

	effects, err := NewEffects(pipeline.Post)
	if err != nil {
		t.Fatalf("NewEffects: %v", err)
	}
	if len(effects) != 1 {
		t.Errorf("effects count = %d, want 1", len(effects))
	}
}

func TestEEPresetDuplicateStageID(t *testing.T) {
	raw := `{
		"output": {
			"crossfeed": {
				"fcut": 700.0,
				"feed": 4.5
			},
			"plugins_order": [
				"crossfeed",
				"crossfeed"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}
	if len(pipeline.Post) != 1 {
		t.Fatalf("post stage count = %d, want 1", len(pipeline.Post))
	}
	if !eeTestContainsWarning(warnings, "crossfeed skipped: duplicate stage in plugins_order") {
		t.Errorf("expected duplicate stage warning, got %v", warnings)
	}
}

func TestEEPresetNegativeBandIndex(t *testing.T) {
	raw := `{
		"output": {
			"equalizer": {
				"left": {
					"band-1": {"frequency": 100.0, "gain": 1.0, "q": 1.0},
					"band0": {"frequency": 200.0, "gain": 2.0, "q": 1.0}
				}
			},
			"plugins_order": [
				"equalizer"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}
	if !eeTestContainsWarning(warnings, "equalizer.band-1 skipped: band index out of range") {
		t.Errorf("expected warning for band-1, got %v", warnings)
	}

	effects, err := NewEffects(pipeline.Post)
	if err != nil {
		t.Fatalf("NewEffects: %v", err)
	}
	if len(effects) != 1 {
		t.Errorf("effects count = %d, want 1", len(effects))
	}
}

func TestEEPresetNullStageConfig(t *testing.T) {
	raw := `{
		"output": {
			"convolver": null,
			"plugins_order": [
				"convolver"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}
	if len(pipeline.Post) != 0 {
		t.Fatalf("post stage count = %d, want 0", len(pipeline.Post))
	}
	if !eeTestContainsWarning(warnings, "convolver skipped: invalid stage JSON") {
		t.Errorf("expected warning for null stage JSON, got %v", warnings)
	}
}

func TestEEPresetAliases(t *testing.T) {
	raw := `{
		"output": {
			"crossfeed": {
				"cutoff": 800.0,
				"feed": 5.0
			},
			"convolver": {
				"kernel": "identity"
			},
			"bass-enhancer": {
				"amount": 3.0
			},
			"plugins_order": [
				"crossfeed",
				"convolver",
				"bass-enhancer"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}
	if len(pipeline.Post) != 3 {
		t.Fatalf("post stage count = %d, want 3", len(pipeline.Post))
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	if pipeline.Post[0].Params[CrossfeedCutoff] != 800.0 {
		t.Errorf("crossfeed cutoff = %v, want 800.0", pipeline.Post[0].Params[CrossfeedCutoff])
	}
	if pipeline.Post[1].Params[ConvolverKernel] != "identity" {
		t.Errorf("convolver kernel = %v, want identity", pipeline.Post[1].Params[ConvolverKernel])
	}
	if pipeline.Post[2].Kind != "bass-enhancer" {
		t.Errorf("bass-enhancer kind = %v, want bass-enhancer", pipeline.Post[2].Kind)
	}

	effects, err := NewEffects(pipeline.Post)
	if err != nil {
		t.Fatalf("NewEffects: %v", err)
	}
	if len(effects) != 3 {
		t.Errorf("effects count = %d, want 3", len(effects))
	}
}

func TestEEPresetMalformedLeftFallbackRight(t *testing.T) {
	raw := `{
		"output": {
			"equalizer": {
				"left": "not-a-map",
				"right": {
					"band0": {"frequency": 150.0, "gain": 2.5, "q": 1.2}
				}
			},
			"plugins_order": [
				"equalizer"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}
	if len(pipeline.Post) != 1 {
		t.Fatalf("post stage count = %d, want 1", len(pipeline.Post))
	}
	if !eeTestContainsWarning(warnings, "equalizer.left skipped: invalid bands object") {
		t.Errorf("expected warning for invalid left bands object, got %v", warnings)
	}

	eqParams := pipeline.Post[0].Params
	if eqParams[EqualizerBandFreqKey(0)] != 150.0 {
		t.Errorf("band0 freq = %v, want 150.0", eqParams[EqualizerBandFreqKey(0)])
	}
	if eqParams[EqualizerBandGainKey(0)] != 2.5 {
		t.Errorf("band0 gain = %v, want 2.5", eqParams[EqualizerBandGainKey(0)])
	}
}

func TestEEPresetUnknownStage(t *testing.T) {
	raw := `{
		"output": {
			"crossfeed": {
				"fcut": 700.0,
				"feed": 4.5
			},
			"pitch_shift#0": {
				"semitones": 2.0
			},
			"plugins_order": [
				"crossfeed",
				"pitch_shift#0"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}
	if len(pipeline.Post) != 1 {
		t.Fatalf("post stage count = %d, want 1", len(pipeline.Post))
	}
	if pipeline.Post[0].Kind != "crossfeed" {
		t.Errorf("stage Kind = %q, want crossfeed", pipeline.Post[0].Kind)
	}
	if !eeTestContainsWarning(warnings, "pitch_shift#0 skipped: unknown effect kind") {
		t.Errorf("expected warning for pitch_shift#0, got %v", warnings)
	}

	effects, err := NewEffects(pipeline.Post)
	if err != nil {
		t.Fatalf("NewEffects: %v", err)
	}
	if len(effects) != 1 {
		t.Errorf("effects count = %d, want 1", len(effects))
	}
}

func TestEEPresetMissingStageConfig(t *testing.T) {
	raw := `{
		"output": {
			"crossfeed": {
				"fcut": 700.0,
				"feed": 4.5
			},
			"plugins_order": [
				"crossfeed",
				"maximizer"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}
	if len(pipeline.Post) != 1 {
		t.Fatalf("post stage count = %d, want 1", len(pipeline.Post))
	}
	if !eeTestContainsWarning(warnings, "maximizer skipped: missing stage configuration in preset") {
		t.Errorf("expected warning for missing maximizer configuration, got %v", warnings)
	}
}

func TestEEPresetMalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"syntax_error", `{"output": { broken json `},
		{"missing_output", `{"input": {}}`},
		{"output_null", `{"output": null}`},
		{"output_string", `{"output": "invalid"}`},
		{"missing_plugins_order", `{"output": {}}`},
		{"plugins_order_not_array", `{"output": {"plugins_order": 123}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := LoadEasyEffectsPreset([]byte(tc.json))
			if err == nil {
				t.Fatalf("expected error for malformed preset %s", tc.name)
			}
			if !errors.Is(err, ErrInvalidPreset) {
				t.Errorf("expected ErrInvalidPreset, got %v", err)
			}
		})
	}
}

func TestEEPresetHandwrittenAllEffects(t *testing.T) {
	raw := `{
		"output": {
			"equalizer": {
				"num-bands": 2,
				"left": {
					"band0": {"frequency": 100.0, "gain": 3.0, "q": 1.4, "mode": "RLC (BT)"},
					"band1": {"frequency": 1000.0, "gain": -2.0, "q": 0.7, "mode": "RLC (BT)"}
				}
			},
			"convolver": {
				"ir-width": 80.0,
				"autogain": true
			},
			"bass_enhancer": {
				"amount": 4.0,
				"harmonics": 6.0,
				"scope": 90.0,
				"floor": 25.0,
				"floor-active": true,
				"blend": 0.2
			},
			"crossfeed": {
				"fcut": 750.0,
				"feed": 5.0
			},
			"maximizer": {
				"threshold": -4.0,
				"release": 5.0,
				"ceiling": 0.0
			},
			"limiter": {
				"threshold": -1.0,
				"attack": 2.0,
				"release": 10.0,
				"lookahead": 4.0,
				"stereo-link": 95.0,
				"mode": "Herm Thin",
				"oversampling": "None"
			},
			"plugins_order": [
				"equalizer",
				"convolver",
				"bass_enhancer",
				"crossfeed",
				"maximizer",
				"limiter"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}

	if len(pipeline.Post) != 6 {
		t.Fatalf("post stages count = %d, want 6", len(pipeline.Post))
	}

	if !eeTestContainsWarning(warnings, "equalizer.band0.mode skipped: RLC (BT) curve not representable") {
		t.Errorf("expected warning for RLC (BT) mode in band0, got %v", warnings)
	}

	effects, err := NewEffects(pipeline.Post)
	if err != nil {
		t.Fatalf("NewEffects: %v", err)
	}
	if len(effects) != 6 {
		t.Errorf("effects count = %d, want 6", len(effects))
	}
}

func TestEEPresetEqualizerBandHandling(t *testing.T) {
	raw := `{
		"output": {
			"equalizer": {
				"left": {
					"band0": {"frequency": 50.0, "gain": 1.0, "q": 1.0},
					"band2": {"frequency": 200.0, "gain": 2.0, "q": 1.0},
					"band12": {"frequency": 12000.0, "gain": 0.0, "q": 1.0},
					"not_a_band": {"gain": 0.0}
				}
			},
			"plugins_order": [
				"equalizer"
			]
		}
	}`

	pipeline, warnings, err := LoadEasyEffectsPreset([]byte(raw))
	if err != nil {
		t.Fatalf("LoadEasyEffectsPreset: %v", err)
	}

	eqStage := pipeline.Post[0]
	// Highest valid band is band2, so inferred num-bands should be 3.
	if eqStage.Params[EqualizerNumBands] != 3 {
		t.Errorf("inferred num-bands = %v, want 3", eqStage.Params[EqualizerNumBands])
	}

	if !eeTestContainsWarning(warnings, "equalizer.band12 skipped: band index out of range") {
		t.Errorf("expected warning for band12, got %v", warnings)
	}
	if !eeTestContainsWarning(warnings, "equalizer.not_a_band skipped: unsupported parameter") {
		t.Errorf("expected warning for not_a_band, got %v", warnings)
	}

	effects, err := NewEffects(pipeline.Post)
	if err != nil {
		t.Fatalf("NewEffects: %v", err)
	}
	if len(effects) != 1 {
		t.Errorf("effects count = %d, want 1", len(effects))
	}
}
