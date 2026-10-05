package main

import (
	"os"
	"testing"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/dsp"
)

// jpopPresetPath is the real EasyEffects preset used as the import fixture. It
// is a user file, so the test skips when it is absent rather than failing.
const jpopPresetPath = "/home/kasaki/Downloads/Jpop bass boosted.json"

// TestImportEasyEffectsPresetInstallsTheChain drives the importer the effect
// window calls: a real EasyEffects preset is parsed and its stages replace the
// chain, in the preset's own order. It is the no-audio end-to-end for the
// feature, on the fake backend.
func TestImportEasyEffectsPresetInstallsTheChain(t *testing.T) {
	data, err := os.ReadFile(jpopPresetPath)
	if err != nil {
		t.Skipf("preset fixture not present: %v", err)
	}

	p, err := molo.New(molo.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	svc := &PlayerService{player: p}

	res, err := svc.ImportEasyEffectsPresetData(string(data))
	if err != nil {
		t.Fatalf("ImportEasyEffectsPresetData: %v", err)
	}
	if res.Stages != 5 {
		t.Fatalf("imported %d stages, want 5", res.Stages)
	}
	t.Logf("imported %d stages, %d warnings", res.Stages, len(res.Warnings))

	// The chain must now hold the preset's stages, in plugins_order.
	chain, err := svc.EffectChain()
	if err != nil {
		t.Fatalf("EffectChain: %v", err)
	}
	got := make([]string, 0, len(chain.Stages))
	for _, st := range chain.Stages {
		got = append(got, st.Kind)
	}
	want := []string{"equalizer", "convolver", "bass-enhancer", "crossfeed", "maximizer"}
	if len(got) != len(want) {
		t.Fatalf("chain kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain kinds = %v, want %v", got, want)
		}
	}

	// The equalizer must carry the preset's band gains, not defaults, so the
	// import is proved to have applied values and not only built empty stages.
	var eq *EffectStageInfo
	for i := range chain.Stages {
		if chain.Stages[i].Kind == "equalizer" {
			eq = &chain.Stages[i]
		}
	}
	if eq == nil {
		t.Fatal("no equalizer stage in the imported chain")
	}
	if v, ok := eq.Values["band0-gain"]; !ok || v != 4.0 {
		t.Fatalf("band0-gain = %v, want 4", eq.Values["band0-gain"])
	}
}

// TestImportEasyEffectsPresetRejectsBadJSON proves a malformed preset is an
// error and does not disturb the chain already in force.
func TestImportEasyEffectsPresetRejectsBadJSON(t *testing.T) {
	p, err := molo.New(molo.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	fx := p.Effects()
	if _, err := fx.AddEffect("crossfeed", ""); err != nil {
		t.Fatalf("AddEffect: %v", err)
	}

	svc := &PlayerService{player: p}
	if _, err := svc.ImportEasyEffectsPresetData("{not json"); err == nil {
		t.Fatal("ImportEasyEffectsPresetData accepted malformed JSON")
	}

	chain, err := svc.EffectChain()
	if err != nil {
		t.Fatalf("EffectChain: %v", err)
	}
	if len(chain.Stages) != 1 || chain.Stages[0].Kind != "crossfeed" {
		t.Fatalf("a rejected import changed the chain: %+v", chain.Stages)
	}
}

// TestImportEasyEffectsPresetWithKernel proves a preset that names a convolver
// kernel loads and wires it once the kernel is registered, which is the path a
// user takes after adding their impulse response.
func TestImportEasyEffectsPresetWithKernel(t *testing.T) {
	data, err := os.ReadFile(jpopPresetPath)
	if err != nil {
		t.Skipf("preset fixture not present: %v", err)
	}
	irPath := "/home/kasaki/Downloads/Razor Surround ((48k Z-Edition)) 2.Stereo +20 bass.irs"
	if _, err := os.Stat(irPath); err != nil {
		t.Skipf("IR fixture not present: %v", err)
	}

	const name = "Razor Surround ((48k Z-Edition)) 2.Stereo +20 bass"
	if err := dsp.RegisterKernel(name, irPath); err != nil {
		t.Fatalf("RegisterKernel: %v", err)
	}

	p, err := molo.New(molo.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	svc := &PlayerService{player: p}
	if _, err := svc.ImportEasyEffectsPresetData(string(data)); err != nil {
		t.Fatalf("ImportEasyEffectsPresetData: %v", err)
	}

	chain, err := svc.EffectChain()
	if err != nil {
		t.Fatalf("EffectChain: %v", err)
	}
	var conv *EffectStageInfo
	for i := range chain.Stages {
		if chain.Stages[i].Kind == "convolver" {
			conv = &chain.Stages[i]
		}
	}
	if conv == nil {
		t.Fatal("no convolver stage")
	}
	if v, ok := conv.Values["kernel-name"]; !ok || v != name {
		t.Fatalf("kernel-name = %v, want %q", conv.Values["kernel-name"], name)
	}
}
