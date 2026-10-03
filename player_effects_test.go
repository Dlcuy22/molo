package molo_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/dsp"
)

// newEffectsPlayer builds a facade over the "fake" playback backend, so the
// editor tests run the real engine without opening a sound card. The fake paces
// at real time, so a test that wants meters waits rather than assuming.
func newEffectsPlayer(t *testing.T, opts ...molo.Option) molo.Player {
	t.Helper()

	all := append([]molo.Option{molo.WithBackend("fake")}, opts...)
	p, err := molo.New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	return p
}

// effectsView narrows a Player to the Effects surface the way a UI does.
func effectsView(t *testing.T, p molo.Player) molo.Effects {
	t.Helper()

	return p.Effects()
}

// stageIDs names the stages in order, for a readable failure.
func stageIDs(stages []molo.EffectStage) []string {
	ids := make([]string, 0, len(stages))
	for _, st := range stages {
		ids = append(ids, st.ID)
	}

	return ids
}

// TestEffectsEditorAddRemoveMove proves the facade editor drives the chain and
// that EffectChain reflects the order after each change.
func TestEffectsEditorAddRemoveMove(t *testing.T) {
	p := newEffectsPlayer(t)
	e := effectsView(t, p)

	xf, err := e.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect(crossfeed): %v", err)
	}
	fade, err := e.AddEffect("fade", "")
	if err != nil {
		t.Fatalf("AddEffect(fade): %v", err)
	}

	waitChainLen(t, e, 2)
	if got := stageIDs(e.EffectChain().Stages); len(got) != 2 || got[0] != xf || got[1] != fade {
		t.Fatalf("after add = %v, want [%s %s]", got, xf, fade)
	}
	t.Logf("chain after add: %v", stageIDs(e.EffectChain().Stages))

	if err := e.MoveEffect(fade, 0); err != nil {
		t.Fatalf("MoveEffect: %v", err)
	}
	waitChainLen(t, e, 2)
	if got := stageIDs(e.EffectChain().Stages); got[0] != fade || got[1] != xf {
		t.Fatalf("after move = %v, want [%s %s]", got, fade, xf)
	}
	t.Logf("chain after move: %v", stageIDs(e.EffectChain().Stages))

	if err := e.RemoveEffect(xf); err != nil {
		t.Fatalf("RemoveEffect: %v", err)
	}
	waitChainLen(t, e, 1)
	if got := stageIDs(e.EffectChain().Stages); len(got) != 1 || got[0] != fade {
		t.Fatalf("after remove = %v, want [%s]", got, fade)
	}
	t.Logf("chain after remove: %v", stageIDs(e.EffectChain().Stages))
}

// TestEffectsSetParamAndBypass proves a parameter and the bypass flag reach the
// chain through the facade, and that the detailed kinds list carries the pairs
// a chooser needs while the plain Player.EffectKinds still works.
func TestEffectsSetParamAndBypass(t *testing.T) {
	p := newEffectsPlayer(t)
	e := effectsView(t, p)

	id, err := e.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitChainLen(t, e, 1)
	waitStageLive(t, e, id)

	if err := e.SetEffectParam(id, dsp.CrossfeedCutoff, 1200.0); err != nil {
		t.Fatalf("SetEffectParam: %v", err)
	}
	st := stageByID(t, e, id)
	if got, _ := st.Values[dsp.CrossfeedCutoff].(float64); got != 1200.0 {
		t.Fatalf("cutoff = %v, want 1200", st.Values[dsp.CrossfeedCutoff])
	}

	if err := e.SetEffectBypass(id, true); err != nil {
		t.Fatalf("SetEffectBypass: %v", err)
	}
	if st := stageByID(t, e, id); !st.Bypassed {
		t.Fatal("Bypassed = false after SetEffectBypass(true)")
	}

	// Both lists are reachable: the plain names on Player, the detailed pairs
	// on Effects.
	if !containsString(p.EffectKinds(), "crossfeed") {
		t.Fatalf("Player.EffectKinds() = %v, want crossfeed", p.EffectKinds())
	}
	var impl string
	for _, k := range e.EffectKindList() {
		if k.Kind == "crossfeed" {
			impl = k.Impl
		}
	}
	if impl == "" {
		t.Fatalf("Effects.EffectKindList() = %+v, want a crossfeed impl", e.EffectKindList())
	}
	t.Logf("plain kinds include crossfeed; detailed crossfeed impl=%q", impl)
}

// TestEffectsEditorRejectsBadInputs proves the facade surfaces the engine's
// clear errors for an unknown ID, an unknown parameter and an out-of-range
// move.
func TestEffectsEditorRejectsBadInputs(t *testing.T) {
	p := newEffectsPlayer(t)
	e := effectsView(t, p)

	id, err := e.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	if _, err := e.AddEffect("fade", ""); err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitChainLen(t, e, 2)
	waitStageLive(t, e, id)

	if err := e.RemoveEffect("no-such-id"); err == nil {
		t.Fatal("RemoveEffect(unknown) returned nil, want an error")
	}
	if err := e.MoveEffect("no-such-id", 0); err == nil {
		t.Fatal("MoveEffect(unknown) returned nil, want an error")
	}
	if err := e.SetEffectParam(id, "not-a-param", 1.0); !errors.Is(err, dsp.ErrUnknownParam) {
		t.Fatalf("SetEffectParam(unknown key) = %v, want ErrUnknownParam", err)
	}
	if err := e.MoveEffect(id, 5); err == nil {
		t.Fatal("MoveEffect(out of range) returned nil, want an error")
	}
}

// TestEffectsMetersAreFinite is the print-don't-listen check through the
// facade: it runs audio through the fake backend with a metered crossfeed and
// logs the in and out dB, which must be finite.
func TestEffectsMetersAreFinite(t *testing.T) {
	p := newEffectsPlayer(t)
	e := effectsView(t, p)

	id, err := e.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	if err := p.PlayQueue([]string{"decode/testdata/mono_1s.opus"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}

	var in, out float32
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		st := stageByID(t, e, id)
		if st.Meters != nil {
			in, out = st.Meters[dsp.MeterIn], st.Meters[dsp.MeterOut]
			if in > -120 && out > -120 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("crossfeed meters: in=%.2f dB out=%.2f dB", in, out)
	if math.IsInf(float64(in), 0) || math.IsNaN(float64(in)) || math.IsInf(float64(out), 0) || math.IsNaN(float64(out)) {
		t.Fatalf("meters are not finite: in=%v out=%v", in, out)
	}
	if in <= -120 || out <= -120 {
		t.Fatalf("meters never registered audio: in=%v out=%v", in, out)
	}
}

// waitChainLen polls until the chain has the expected stage count, because the
// install is enqueued to the control loop and is not synchronous.
func waitChainLen(t *testing.T, e molo.Effects, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.EffectChain().Stages) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("chain never reached %d stages, has %v", want, stageIDs(e.EffectChain().Stages))
}

// waitStageLive polls until the stage is paired with a live effect, which is
// what makes a parameter change safe: a non-nil Meters map only comes from an
// installed effect, so it is the signal that the enqueued install has landed.
func waitStageLive(t *testing.T, e molo.Effects, id string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range e.EffectChain().Stages {
			if st.ID == id && st.Meters != nil {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("stage %s never installed", id)
}

// stageByID finds one stage or fails.
func stageByID(t *testing.T, e molo.Effects, id string) molo.EffectStage {
	t.Helper()

	for _, st := range e.EffectChain().Stages {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("stage %s missing from %v", id, stageIDs(e.EffectChain().Stages))

	return molo.EffectStage{}
}
