package session

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/playback"
)

// effectsSession wires a session over the fake playback backend. The fake
// consumes the Provider exactly like a real device and needs no sound card, so
// the tests here run audio through the real chain and print what the meters
// read. The pace is zeroed so a short buffer drains immediately instead of in
// real time.
func effectsSession(t *testing.T, pipeline dsp.Pipeline) *Session {
	t.Helper()

	cfg := testConfig()
	cfg.Backend = "fake"
	cfg.Pipeline = pipeline
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = func(string) (playback.Device, error) {
		d := playback.NewFakeDevice()
		d.SetPace(0)

		return d, nil
	}

	return newSession(t, cfg)
}

// waitInstalled waits until the live chain has the same length as the pipeline
// description, which is how a settled install reads. Reading EffectStages
// before that would exercise the mid-install path, not the normal one.
func waitInstalled(t *testing.T, s *Session, stages int) {
	t.Helper()

	eventually(t, 2*time.Second, "the chain to install", func() bool {
		return len(s.chain.Effects()) == stages
	})
}

// stageByID finds one stage in the returned slice.
func stageByID(stages []EffectStage, id string) (EffectStage, bool) {
	for _, st := range stages {
		if st.ID == id {
			return st, true
		}
	}

	return EffectStage{}, false
}

// TestEffectStagesSkipsPairingMidInstall pins the generation guard. It changes
// the accepted description without letting the control loop install it, which
// is exactly the window a rebuild opens, and proves the editor reports the
// description with no meters rather than pairing a new stage ID with the old
// chain's effect. That wrong pairing would show one effect's meters under
// another's identity.
func TestEffectStagesSkipsPairingMidInstall(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	// Publish a new description under a different ID without installing it,
	// standing in for the gap between ApplyPipeline and the control loop.
	s.runtime.setPipeline(dsp.Pipeline{Post: []dsp.Spec{{ID: "fade-99", Kind: "fade"}}})

	stages := s.EffectStages()
	if len(stages) != 1 || stages[0].ID != "fade-99" {
		t.Fatalf("mid-install stages = %v, want the new description", stageIDs(stages))
	}
	if stages[0].Meters != nil {
		t.Fatalf("mid-install stage carried meters: %+v", stages[0].Meters)
	}

	// A Set on the old ID, or on an ID that only the new description knows,
	// must refuse rather than write to the wrong effect.
	if err := s.SetEffectParam(id, dsp.CrossfeedCutoff, 900.0); !errors.Is(err, ErrUnknownEffect) {
		t.Fatalf("SetEffectParam mid-install = %v, want ErrUnknownEffect", err)
	}
	if err := s.SetEffectParam("fade-99", dsp.FadeDuration, 100.0); !errors.Is(err, ErrUnknownEffect) {
		t.Fatalf("SetEffectParam(new id) mid-install = %v, want ErrUnknownEffect", err)
	}
}

// TestEffectChainAddRemoveMoveReflectsOrder drives the editor end to end: it
// adds the built-in crossfeed and fade, and each add, move and remove must be
// reflected in EffectStages in processing order.
func TestEffectChainAddRemoveMoveReflectsOrder(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})

	xf, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect(crossfeed): %v", err)
	}
	fade, err := s.AddEffect("fade", "")
	if err != nil {
		t.Fatalf("AddEffect(fade): %v", err)
	}
	waitInstalled(t, s, 2)

	stages := s.EffectStages()
	if len(stages) != 2 || stages[0].ID != xf || stages[1].ID != fade {
		t.Fatalf("after add order = %v, want [%s %s]", stageIDs(stages), xf, fade)
	}
	t.Logf("after add: %v", describeStages(stages))

	// Moving the second stage to the front reverses the pair.
	if err := s.MoveEffect(fade, 0); err != nil {
		t.Fatalf("MoveEffect: %v", err)
	}
	waitInstalled(t, s, 2)
	stages = s.EffectStages()
	if stages[0].ID != fade || stages[1].ID != xf {
		t.Fatalf("after move order = %v, want [%s %s]", stageIDs(stages), fade, xf)
	}
	t.Logf("after move: %v", describeStages(stages))

	// Removing the crossfeed leaves only the fade.
	if err := s.RemoveEffect(xf); err != nil {
		t.Fatalf("RemoveEffect: %v", err)
	}
	waitInstalled(t, s, 1)
	stages = s.EffectStages()
	if len(stages) != 1 || stages[0].ID != fade {
		t.Fatalf("after remove order = %v, want [%s]", stageIDs(stages), fade)
	}
	t.Logf("after remove: %v", describeStages(stages))
}

// TestSetEffectParamKeepsTheLiveInstance is the no-rebuild check. A rebuild
// constructs a new effect and Configure+Reset wipes its state, so the proof
// that a parameter change did not rebuild is that the effect pointer is
// unchanged and a value the effect held survives. The crossfeed's cutoff is
// read back through EffectStages to show the new value landed.
func TestSetEffectParamKeepsTheLiveInstance(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	before := s.chain.Effects()[0]
	if err := s.SetEffectParam(id, dsp.CrossfeedCutoff, 1500.0); err != nil {
		t.Fatalf("SetEffectParam: %v", err)
	}
	after := s.chain.Effects()[0]

	if before != after {
		t.Fatalf("SetEffectParam rebuilt the effect: %T %p -> %T %p", before, before, after, after)
	}

	stages := s.EffectStages()
	st, ok := stageByID(stages, id)
	if !ok {
		t.Fatalf("stage %s missing after SetEffectParam: %v", id, stageIDs(stages))
	}
	if got, _ := st.Values[dsp.CrossfeedCutoff].(float64); got != 1500.0 {
		t.Fatalf("cutoff = %v, want 1500", st.Values[dsp.CrossfeedCutoff])
	}
	t.Logf("cutoff now %v, instance unchanged", st.Values[dsp.CrossfeedCutoff])
}

// TestSetEffectBypassTogglesTheLiveEffect proves bypass is just the standard
// parameter: it flips the flag the chain reports without rebuilding.
func TestSetEffectBypassTogglesTheLiveEffect(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	before := s.chain.Effects()[0]
	if err := s.SetEffectBypass(id, true); err != nil {
		t.Fatalf("SetEffectBypass: %v", err)
	}
	if s.chain.Effects()[0] != before {
		t.Fatal("SetEffectBypass rebuilt the effect")
	}

	st, ok := stageByID(s.EffectStages(), id)
	if !ok {
		t.Fatalf("stage %s missing", id)
	}
	if !st.Bypassed {
		t.Fatal("Bypassed = false after SetEffectBypass(true)")
	}
	t.Logf("bypass now %v", st.Bypassed)
}

// TestSetEffectParamDoesNotResetEffectState proves a parameter change leaves
// the running effect's accumulated state intact. A rebuild would construct a
// fresh effect whose meters start at the floor; here the meters are showing
// audio before the change, and they must still be showing it after, with the
// same instance in place.
func TestSetEffectParamDoesNotResetEffectState(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Let audio cross the stage so both meters hold a real reading. The wait
	// must cover BOTH: Process pushes `in` at entry and `out` at exit, so a
	// wait that only checks `in` can succeed on the first block, before `out`
	// has been pushed even once. Asserting both after such a wait would then
	// fail on a loaded machine, which is exactly what -race exposed.
	eventually(t, 3*time.Second, "both meters to register audio", func() bool {
		st, ok := stageByID(s.EffectStages(), id)
		if !ok || st.Meters == nil {
			return false
		}

		return st.Meters[dsp.MeterIn] > -120 && st.Meters[dsp.MeterOut] > -120
	})

	before := s.chain.Effects()[0]
	if err := s.SetEffectParam(id, dsp.CrossfeedFeed, 6.0); err != nil {
		t.Fatalf("SetEffectParam: %v", err)
	}
	after := s.chain.Effects()[0]
	if before != after {
		t.Fatal("SetEffectParam replaced the live effect")
	}

	// The reading right after the change must not be the floor a fresh
	// instance would report.
	st, ok := stageByID(s.EffectStages(), id)
	if !ok {
		t.Fatalf("stage %s missing", id)
	}
	if st.Meters[dsp.MeterIn] <= -120 || st.Meters[dsp.MeterOut] <= -120 {
		t.Fatalf("state was reset by the parameter change: meters = %+v", st.Meters)
	}
	t.Logf("same instance, meters survived: in=%.2f out=%.2f", st.Meters[dsp.MeterIn], st.Meters[dsp.MeterOut])
}

// TestEffectEditorRejectsUnknownID proves every command that addresses a stage
// reports a clear error for an ID that is not in the chain, rather than a panic
// or a silent no-op.
func TestEffectEditorRejectsUnknownID(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	if _, err := s.AddEffect("crossfeed", ""); err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	if err := s.RemoveEffect("no-such-id"); !errors.Is(err, ErrUnknownEffect) {
		t.Fatalf("RemoveEffect(unknown) = %v, want ErrUnknownEffect", err)
	}
	if err := s.MoveEffect("no-such-id", 0); !errors.Is(err, ErrUnknownEffect) {
		t.Fatalf("MoveEffect(unknown) = %v, want ErrUnknownEffect", err)
	}
	if err := s.SetEffectParam("no-such-id", dsp.CrossfeedCutoff, 800.0); !errors.Is(err, ErrUnknownEffect) {
		t.Fatalf("SetEffectParam(unknown id) = %v, want ErrUnknownEffect", err)
	}
	if err := s.SetEffectBypass("no-such-id", true); !errors.Is(err, ErrUnknownEffect) {
		t.Fatalf("SetEffectBypass(unknown id) = %v, want ErrUnknownEffect", err)
	}
}

// TestEffectEditorRejectsUnknownParamKey proves an unknown key on a real stage
// is the effect's own error, not a silent no-op.
func TestEffectEditorRejectsUnknownParamKey(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	if err := s.SetEffectParam(id, "not-a-param", 1.0); !errors.Is(err, dsp.ErrUnknownParam) {
		t.Fatalf("SetEffectParam(unknown key) = %v, want ErrUnknownParam", err)
	}
}

// TestMoveEffectRejectsOutOfRangeIndex proves a move past either end is an
// error and leaves the chain alone.
func TestMoveEffectRejectsOutOfRangeIndex(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	first, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	if _, err := s.AddEffect("fade", ""); err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 2)

	for _, to := range []int{-1, 2} {
		if err := s.MoveEffect(first, to); !errors.Is(err, ErrEffectIndexOutOfRange) {
			t.Fatalf("MoveEffect(to=%d) = %v, want ErrEffectIndexOutOfRange", to, err)
		}
	}

	stages := s.EffectStages()
	if stages[0].ID != first {
		t.Fatalf("a rejected move changed the chain: %v", stageIDs(stages))
	}
}

// TestEffectKindsListsImplementations proves the chooser list carries the
// (kind, impl, label, weight) a UI needs, and that the built-in crossfeed is
// present.
func TestEffectKindsListsImplementations(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})

	kinds := s.EffectKinds()
	var crossfeed *EffectKindInfo
	for i := range kinds {
		if kinds[i].Kind == "crossfeed" {
			crossfeed = &kinds[i]

			break
		}
	}
	if crossfeed == nil {
		t.Fatalf("EffectKinds() = %+v, want a crossfeed entry", kinds)
	}
	if crossfeed.Impl == "" || crossfeed.Label == "" {
		t.Fatalf("crossfeed entry incomplete: %+v", *crossfeed)
	}
	if crossfeed.Scripted {
		t.Fatalf("crossfeed marked scripted: %+v", *crossfeed)
	}
	t.Logf("crossfeed impl=%q label=%q weight=%d", crossfeed.Impl, crossfeed.Label, crossfeed.Weight)

	// The plain name list still works beside the detailed one.
	if !containsString(s.EffectKindNames(), "crossfeed") {
		t.Fatalf("EffectKindNames() = %v, want crossfeed", s.EffectKindNames())
	}
}

// TestEffectStagesReportLiveMeters is the print-don't-listen check: it runs a
// tone through the fake backend with a metered crossfeed and logs the in and
// out dB the effect reports. The readings must be finite and above the meter
// floor, which is what proves audio actually crossed the stage.
func TestEffectStagesReportLiveMeters(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// The fake device consumes on its own goroutine, so polls for a reading
	// that is finite and above the floor rather than assuming one arrives
	// immediately.
	var in, out float32
	eventually(t, 3*time.Second, "the meters to register audio", func() bool {
		st, ok := stageByID(s.EffectStages(), id)
		if !ok || st.Meters == nil {
			return false
		}
		in, out = st.Meters[dsp.MeterIn], st.Meters[dsp.MeterOut]

		return !math.IsInf(float64(in), 0) && !math.IsNaN(float64(in)) &&
			!math.IsInf(float64(out), 0) && !math.IsNaN(float64(out)) &&
			in > -120 && out > -120
	})

	t.Logf("crossfeed meters: in=%.2f dB out=%.2f dB", in, out)

	if math.IsInf(float64(in), 0) || math.IsNaN(float64(in)) || math.IsInf(float64(out), 0) || math.IsNaN(float64(out)) {
		t.Fatalf("meters are not finite: in=%v out=%v", in, out)
	}
}

// stageIDs names the stages in order, for a readable failure.
func stageIDs(stages []EffectStage) []string {
	ids := make([]string, 0, len(stages))
	for _, st := range stages {
		ids = append(ids, st.ID)
	}

	return ids
}

// describeStages renders the chain compactly for a log line.
func describeStages(stages []EffectStage) string {
	out := ""
	for i, st := range stages {
		if i > 0 {
			out += " -> "
		}
		out += st.Kind + "(" + st.ID + ")"
	}

	return out
}

// TestAddEffectIsImmediatelySettable pins the editor's synchronous install: a
// stage returned by AddEffect must already be live, so the obvious flow of
// adding a stage and setting its first parameter works without polling. Before
// the wait, SetEffectParam failed every time because the install was still in
// flight.
func TestAddEffectIsImmediatelySettable(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})

	for i := 0; i < 5; i++ {
		id, err := s.AddEffect("crossfeed", "")
		if err != nil {
			t.Fatalf("AddEffect: %v", err)
		}
		if err := s.SetEffectParam(id, dsp.CrossfeedFeed, 6.0); err != nil {
			t.Fatalf("SetEffectParam immediately after AddEffect: %v", err)
		}
	}
}

// TestConcurrentEditorCommandsDoNotLoseStages pins the serialization of the
// editor's read-modify-write. Without it two concurrent adds start from the
// same list and the later install drops the other, which is a lost update that
// -race cannot see because it is a logical race, not a memory race.
func TestConcurrentEditorCommandsDoNotLoseStages(t *testing.T) {
	const n = 8
	for trial := 0; trial < 10; trial++ {
		s := effectsSession(t, dsp.Pipeline{})

		start := make(chan struct{})
		done := make(chan struct{}, n)
		for i := 0; i < n; i++ {
			go func() {
				<-start
				_, _ = s.AddEffect("crossfeed", "")
				done <- struct{}{}
			}()
		}
		close(start)
		for i := 0; i < n; i++ {
			<-done
		}

		if got := len(s.EffectStages()); got != n {
			t.Fatalf("trial %d: chain has %d stages after %d concurrent adds, want %d", trial, got, n, n)
		}
	}
}

// TestEffectIDsStayUniqueAcrossCallerIDs pins the ID minting against a
// caller-supplied ID. A mint that collides would make RemoveEffect drop both
// matches while liveEffect addresses the first, so the same ID would mean two
// different stages.
func TestEffectIDsStayUniqueAcrossCallerIDs(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	if err := s.ApplyPipeline(dsp.Pipeline{Post: []dsp.Spec{{ID: "crossfeed-1", Kind: "crossfeed"}}}); err != nil {
		t.Fatalf("ApplyPipeline: %v", err)
	}

	id, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	if id == "crossfeed-1" {
		t.Fatalf("AddEffect minted the caller's ID %q", id)
	}

	before := len(s.EffectStages())
	if err := s.RemoveEffect("crossfeed-1"); err != nil {
		t.Fatalf("RemoveEffect: %v", err)
	}
	if after := len(s.EffectStages()); before-after != 1 {
		t.Fatalf("RemoveEffect removed %d stages, want exactly 1", before-after)
	}
}

// TestDuplicateStageIDsAreRejected pins the pipeline boundary: two stages
// sharing an ID cannot be addressed unambiguously, so the pipeline is refused
// whole rather than installed and mis-edited later.
func TestDuplicateStageIDsAreRejected(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	err := s.ApplyPipeline(dsp.Pipeline{Post: []dsp.Spec{
		{ID: "dup", Kind: "crossfeed"},
		{ID: "dup", Kind: "fade"},
	}})
	if err == nil {
		t.Fatal("ApplyPipeline accepted two stages with the same ID")
	}
	if len(s.EffectStages()) != 0 {
		t.Fatalf("a rejected pipeline changed the chain: %v", stageIDs(s.EffectStages()))
	}
}

// TestConcurrentAddThenSetNeverRefused pins the strongest form of the editor's
// contract: a parameter set immediately after an add must succeed even when two
// editors run at once. A second add can advance the pipeline generation before
// the first caller's set, and a guard that only looked at the published chain
// would refuse it as mid-install. Resolving against the pending chain closes
// that window, so this must hold for every trial.
func TestConcurrentAddThenSetNeverRefused(t *testing.T) {
	const trials = 100
	for trial := 0; trial < trials; trial++ {
		s := effectsSession(t, dsp.Pipeline{})

		var wg sync.WaitGroup
		var mu sync.Mutex
		var failures []error
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id, err := s.AddEffect("crossfeed", "")
				if err != nil {
					mu.Lock()
					failures = append(failures, fmt.Errorf("AddEffect: %w", err))
					mu.Unlock()

					return
				}
				if err := s.SetEffectParam(id, dsp.CrossfeedFeed, 6.0); err != nil {
					mu.Lock()
					failures = append(failures, fmt.Errorf("SetEffectParam(%s): %w", id, err))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if len(failures) > 0 {
			t.Fatalf("trial %d: %v", trial, failures)
		}
	}
}

// TestEffectStagesPairsByIdentityNotIndex pins the resolution rule. A description
// and the live chain can come from different accepted changes, so a stage is
// paired by its ID and kind, never by its position. Here a description reuses a
// live stage's ID for a different kind; pairing by ID alone would hand it the
// live crossfeed's meters under a fade's identity.
func TestEffectStagesPairsByIdentityNotIndex(t *testing.T) {
	s := effectsSession(t, dsp.Pipeline{})
	id, err := s.AddEffect("crossfeed", "")
	if err != nil {
		t.Fatalf("AddEffect: %v", err)
	}
	waitInstalled(t, s, 1)

	// A new description reuses the live ID for a different kind, accepted but
	// not installed.
	s.runtime.setPipeline(dsp.Pipeline{Post: []dsp.Spec{{ID: id, Kind: "fade"}}})

	stages := s.EffectStages()
	if len(stages) != 1 {
		t.Fatalf("stages = %v, want one", stageIDs(stages))
	}
	if stages[0].Kind != "fade" {
		t.Fatalf("stage kind = %q, want the description's fade", stages[0].Kind)
	}
	if stages[0].Meters != nil {
		t.Fatalf("a fade description was paired with the live crossfeed's meters: %+v", stages[0].Meters)
	}
}
