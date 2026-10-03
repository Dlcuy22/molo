package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/dsp"
)

// TestScriptedEffectRunsThroughTheEditor is the no-audio end-to-end for the
// feature: the bundled Lua effects are registered, one is added to the chain
// through the same editor surface the window calls, a parameter is set on it,
// and audio runs through it on the fake backend. The meters are printed, which
// is the only way to see the effect work without a speaker.
func TestScriptedEffectRunsThroughTheEditor(t *testing.T) {
	loadScripts()

	p, err := molo.New(molo.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	fx := p.Effects()

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
	for time.Now().Before(deadline) && p.Snapshot().State != molo.Stopped {
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

	p, err := molo.New(molo.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	fx := p.Effects()

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

// TestScriptedCompressorPublishesTelemetry is the no-audio end-to-end for the
// telemetry feature: the bundled Compressor is added through the editor, audio
// is played through it on the fake backend, and the stage must report both the
// gain-reduction reading it declared and the transfer curve it declared. It
// crosses the whole path: the Lua graph, the engine snapshot, and the webui
// bridge that the effect window reads.
func TestScriptedCompressorPublishesTelemetry(t *testing.T) {
	loadScripts()

	p, err := molo.New(molo.WithBackend("fake"))
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	defer p.Close()

	fx := p.Effects()

	// The bundled Compressor must be offered by the chooser, which is what
	// proves the embedded effects/compressor.lua was picked up.
	var found bool
	for _, k := range fx.EffectKindList() {
		if k.Kind == "script" && k.Impl == "Compressor" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Compressor not offered; kinds = %+v", fx.EffectKindList())
	}

	id, err := fx.AddEffect("script", "Compressor")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	// A low threshold and a hard ratio, so the fixture's level is well over it
	// and the reduction the meter reports is unambiguous.
	for key, v := range map[string]float64{"threshold": -36, "ratio": 10, "attack": 0.001, "release": 0.05, "makeup": 0, "mix": 1} {
		if err := fx.SetEffectParam(id, key, v); err != nil {
			t.Fatalf("SetEffectParam(%s): %v", key, err)
		}
	}

	if err := p.PlayQueue([]string{"../../decode/testdata/mono_1s.opus"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}

	// Sample the live gr while audio runs: the reduction returns toward zero as
	// the fixture decays, so reading once at the end can catch the tail. The
	// whole run is bounded so a player that never reports still fails.
	svc := &PlayerService{player: p}
	var grLive float32
	var sawGR bool
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && p.Snapshot().State != molo.Stopped {
		if chain, err := svc.EffectChain(); err == nil && len(chain.Stages) == 1 {
			if v, ok := chain.Stages[0].Meters["gr"]; ok && v < 0 {
				grLive, sawGR = v, true
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Read through the same bridge the effect window calls, so this asserts the
	// wire shape and not only the engine's own view.
	chain, err := svc.EffectChain()
	if err != nil {
		t.Fatalf("EffectChain: %v", err)
	}
	if len(chain.Stages) != 1 {
		t.Fatalf("chain has %d stages, want 1", len(chain.Stages))
	}
	st := chain.Stages[0]
	t.Logf("compressor stage %s: %d readings, visuals=%+v, meters=%v", st.ID, len(st.Readings), st.Visuals, st.Meters)

	// The declared reading: key "gr", the words "Gain Reduction", and the
	// gain-reduction kind, so the panel can draw it as a reduction rather than a
	// fixed level.
	var gr *EffectReadingInfo
	for i := range st.Readings {
		if st.Readings[i].Key == "gr" {
			gr = &st.Readings[i]
		}
	}
	if gr == nil {
		t.Fatalf("stage reports no gr reading: %+v", st.Readings)
	}
	if gr.Label != "Gain Reduction" || gr.Unit != "dB" || gr.Kind != string(dsp.ReadingGainReduction) {
		t.Fatalf("gr reading metadata lost over the bridge: %+v", gr)
	}
	if gr.Min != -30 || gr.Max != 0 {
		t.Fatalf("gr range = [%v, %v], want [-30, 0]", gr.Min, gr.Max)
	}

	// The declared visuals: a transfer curve derived from the compressor params,
	// and a scrolling dynamics display over the same readings.
	var transfer *EffectVisualInfo
	var dynamics *EffectVisualInfo
	for i := range st.Visuals {
		switch st.Visuals[i].Kind {
		case string(dsp.VisualTransfer):
			transfer = &st.Visuals[i]
		case string(dsp.VisualDynamics):
			dynamics = &st.Visuals[i]
		}
	}
	if transfer == nil {
		t.Fatalf("stage reports no transfer visual: %+v", st.Visuals)
	}
	if got := strings.Join(transfer.Params, ","); got != "threshold,ratio,makeup,knee" {
		t.Fatalf("visual params = %q, want threshold,ratio,makeup,knee", got)
	}
	if got := strings.Join(transfer.Overlays, ","); got != "in,gr" {
		t.Fatalf("visual overlays = %q, want in,gr", got)
	}
	if dynamics == nil {
		t.Fatalf("stage reports no dynamics visual: %+v", st.Visuals)
	}
	if got := strings.Join(dynamics.Overlays, ","); got != "in,out,gr" {
		t.Fatalf("dynamics overlays = %q, want in,out,gr", got)
	}

	// The reading must carry a live number, not just metadata: the fixture is
	// loud enough to trigger the compressor, so a negative gr must be observed
	// at some point during playback.
	t.Logf("compressor gr reading during playback = %v dB", grLive)
	if !sawGR {
		t.Fatal("never observed a negative gr reading while the compressor ran")
	}
}
