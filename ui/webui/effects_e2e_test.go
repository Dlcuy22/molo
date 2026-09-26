package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/dsp"
)

// TestScriptedEffectRunsThroughTheEditor is the no-audio end-to-end for the
// feature: the bundled Lua effects are registered, one is added to the chain
// through the same editor surface the window calls, a parameter is set on it,
// and audio runs through it on the fake backend. The meters are printed, which
// is the only way to see the effect work without a speaker.
func TestScriptedEffectRunsThroughTheEditor(t *testing.T) {
	loadScripts()

	p, err := player.New(player.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	fx, ok := p.(player.Effects)
	if !ok {
		t.Fatal("player has no effect editor")
	}

	// The bundled Tremolo must be offered by the chooser.
	var found bool
	for _, k := range fx.EffectKindList() {
		if k.Kind == "script" && k.Impl == "Tremolo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Tremolo not offered; kinds = %+v", fx.EffectKindList())
	}

	id, err := fx.AddEffect("script", "Tremolo")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	// The stage is live on return, so a parameter can be set immediately.
	if err := fx.SetEffectParam(id, "rate", 8.0); err != nil {
		t.Fatalf("SetEffectParam: %v", err)
	}
	if err := fx.SetEffectParam(id, "depth", 0.9); err != nil {
		t.Fatalf("SetEffectParam: %v", err)
	}

	chain := fx.EffectChain()
	if len(chain.Stages) != 1 {
		t.Fatalf("chain has %d stages, want 1", len(chain.Stages))
	}
	st := chain.Stages[0]
	t.Logf("stage %s kind=%s impl=%s label=%q schema=%d params", st.ID, st.Kind, st.Impl, st.Label, len(st.Schema))
	if st.Label != "Tremolo" {
		t.Errorf("label = %q, want Tremolo", st.Label)
	}
	// The standard three are prepended by the engine, so a script cannot hide
	// them and the panel always has its base controls.
	for _, key := range []string{"bypass", "input-gain", "output-gain"} {
		if !hasParamKey(st.Schema, key) {
			t.Errorf("schema is missing the standard param %q", key)
		}
	}
	if v := st.Values["rate"]; v != 8.0 {
		t.Errorf("rate value = %v, want 8", v)
	}

	if err := p.PlayQueue([]string{"../../decode/testdata/mono_1s.opus"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}

	// Run to the end, then read the meters. The effect measures in and out, so
	// a non-floor reading proves audio crossed it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && p.Snapshot().State != player.Stopped {
		time.Sleep(25 * time.Millisecond)
	}

	chain = fx.EffectChain()
	if len(chain.Stages) != 1 {
		t.Fatalf("chain changed mid-run: %d stages", len(chain.Stages))
	}
	m := chain.Stages[0].Meters
	t.Logf("tremolo meters after playback: %v", m)
	if m == nil {
		t.Fatal("scripted effect reported no meters")
	}
	in, out := m[dsp.MeterIn], m[dsp.MeterOut]
	if in <= -120 || out <= -120 {
		t.Fatalf("meters never registered audio: in=%v out=%v", in, out)
	}
	t.Logf("final state=%v position=%v backend=%s", p.Snapshot().State, p.Snapshot().Position, p.Snapshot().Backend)
}

// TestEq20IsExposedThroughTheEditor proves the bundled 20-band EQ reaches the
// editor surface the window calls: it is offered by the chooser, all twenty
// bands are in the schema, and each one can be set. The band order is the
// loader's sorted order, which is what the UI lays out left to right.
func TestEq20IsExposedThroughTheEditor(t *testing.T) {
	loadScripts()

	p, err := player.New(player.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	fx := p.(player.Effects)

	var found bool
	for _, k := range fx.EffectKindList() {
		if k.Kind == "script" && k.Impl == "20-Band EQ" {
			found = true
		}
	}
	if !found {
		t.Fatalf("20-Band EQ not offered; kinds = %+v", fx.EffectKindList())
	}

	id, err := fx.AddEffect("script", "20-Band EQ")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}

	svc := &PlayerService{player: p}
	chain, err := svc.EffectChain()
	if err != nil {
		t.Fatalf("EffectChain: %v", err)
	}
	if len(chain.Stages) != 1 {
		t.Fatalf("chain has %d stages, want 1", len(chain.Stages))
	}
	st := chain.Stages[0]

	bands := 0
	for _, param := range st.Schema {
		if strings.HasPrefix(param.Key, "gain") {
			bands++
		}
	}
	if bands != 20 {
		t.Fatalf("schema exposes %d bands, want 20", bands)
	}
	t.Logf("20-Band EQ: %d params, %d bands", len(st.Schema), bands)

	// Every band must be settable, and the last one must read back.
	for i := 1; i <= 20; i++ {
		key := fmt.Sprintf("gain%02d", i)
		if err := fx.SetEffectParam(id, key, float64(i%7-3)); err != nil {
			t.Fatalf("SetEffectParam(%s): %v", key, err)
		}
	}
	after, err := svc.EffectChain()
	if err != nil {
		t.Fatalf("EffectChain after sets: %v", err)
	}
	if got := after.Stages[0].Values["gain20"]; got != float64(20%7-3) {
		t.Fatalf("gain20 = %v, want %v", got, 20%7-3)
	}
}

// TestEffectWindowBridgeMapsTheChain checks the bridge's own mapping on a real
// engine stage, so the wire shape is exercised against a live effect and not
// only a fixture.
func TestEffectWindowBridgeMapsTheChain(t *testing.T) {
	loadScripts()

	p, err := player.New(player.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	fx := p.(player.Effects)
	if _, err := fx.AddEffect("crossfeed", ""); err != nil {
		t.Fatalf("AddEffect: %v", err)
	}

	svc := &PlayerService{player: p}
	chain, err := svc.EffectChain()
	if err != nil {
		t.Fatalf("EffectChain: %v", err)
	}
	if len(chain.Stages) != 1 {
		t.Fatalf("wire chain has %d stages, want 1", len(chain.Stages))
	}
	w := chain.Stages[0]
	if w.ID == "" || w.Kind != "crossfeed" {
		t.Fatalf("wire stage = %+v", w)
	}
	if len(w.Schema) == 0 {
		t.Fatal("wire stage has no schema")
	}
	for _, p := range w.Schema {
		if p.Key == "" {
			t.Errorf("wire param has an empty key: %+v", p)
		}
		if p.Options == nil {
			t.Errorf("wire param %q has nil options, want an array", p.Key)
		}
	}
	t.Logf("wire stage %s: %d params, meters=%v", w.ID, len(w.Schema), w.Meters)
}

func hasParamKey(schema []dsp.Param, key string) bool {
	for _, p := range schema {
		if p.Key == key {
			return true
		}
	}

	return false
}
