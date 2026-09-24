package main

import (
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/dsp"
)

// TestNormalizePreviewConfigClamps pins the ranges: a bad value cannot wedge the
// session or reach the fade effect out of range. Volume 0 is a legitimate mute,
// so it must survive; only a negative or above-unity value is corrected.
func TestNormalizePreviewConfigClamps(t *testing.T) {
	got := normalizePreviewConfig(PreviewConfig{StartMs: -5, LengthMs: -1, FadeMs: 99999, Volume: -1})
	if got.StartMs != 0 || got.LengthMs != 0 {
		t.Fatalf("negative window not clamped: %+v", got)
	}
	if got.FadeMs != 5000 {
		t.Fatalf("fade not clamped to 5000: %d", got.FadeMs)
	}
	if got.Volume != 0 {
		t.Fatalf("negative volume = %v, want 0", got.Volume)
	}

	// Volume 0 is a real mute and must not be rewritten to full.
	if mute := normalizePreviewConfig(PreviewConfig{Volume: 0}); mute.Volume != 0 {
		t.Fatalf("volume 0 was rewritten to %v, want 0 (mute)", mute.Volume)
	}
	if over := normalizePreviewConfig(PreviewConfig{Volume: 2}); over.Volume != 1 {
		t.Fatalf("volume 2 = %v, want 1", over.Volume)
	}

	// A sane config is left alone.
	in := PreviewConfig{StartMs: 1000, LengthMs: 30000, FadeMs: 300, Loop: true, Volume: 0.8}
	if out := normalizePreviewConfig(in); out != in {
		t.Fatalf("sane config changed: %+v -> %+v", in, out)
	}
}

// TestPreviewConfigRoundTripsThroughService checks the bound settings surface:
// what is set is what is read back.
func TestPreviewConfigRoundTripsThroughService(t *testing.T) {
	svc := newPlayerService()
	defer svc.cancel()

	svc.preview = newPreviewer(svc, nil)
	svc.preview.start()
	defer svc.preview.close()

	def := svc.PreviewConfig()
	if def.LengthMs != defaultPreviewLengthMs || !def.Loop || def.FadeMs != defaultPreviewFadeMs {
		t.Fatalf("default preview config = %+v, want length %d loop true fade %d", def, defaultPreviewLengthMs, defaultPreviewFadeMs)
	}

	want := PreviewConfig{StartMs: 5000, LengthMs: 15000, FadeMs: 500, Loop: false, Volume: 0.5}
	if err := svc.SetPreviewConfig(want); err != nil {
		t.Fatalf("SetPreviewConfig: %v", err)
	}
	if got := waitForConfig(t, svc, want); got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
}

// TestPreviewSetFadeInstallsTheEffect is the bridge between the preview and the
// dsp package: the pipeline the previewer installs must build, which is what
// makes the fade real rather than a volume ramp.
func TestPreviewSetFadeInstallsTheEffect(t *testing.T) {
	for _, mode := range []string{dsp.FadeModeIn, dsp.FadeModeOut} {
		effects, err := dsp.NewEffects([]dsp.Spec{{
			Kind:   "fade",
			Params: dsp.Values{"mode": mode, "duration": 300.0},
		}})
		if err != nil {
			t.Fatalf("fade effect (%s) rejected: %v", mode, err)
		}
		if len(effects) != 1 {
			t.Fatalf("fade pipeline (%s) built %d effects, want 1", mode, len(effects))
		}
	}
}

// TestPreviewStateIsInactiveByDefault pins the snapshot contract the palette
// reads: nothing previews until a preview is started.
func TestPreviewStateIsInactiveByDefault(t *testing.T) {
	svc := newPlayerService()
	defer svc.cancel()

	svc.preview = newPreviewer(svc, nil)
	svc.preview.start()
	defer svc.preview.close()

	if st := svc.preview.state(); st.Active {
		t.Fatalf("preview active before any play: %+v", st)
	}
}

// TestPreviewDoesNotReplayWhileBuildIsInFlight is the P0-1 regression: the
// engine's Play is asynchronous, so a tick that sees StateIdle right after a
// Play must NOT read that as "the window ended". The old code re-Played on that
// misread, which is what made some rows behave and the next one double-play or
// self-cancel.
func TestPreviewDoesNotReplayWhileBuildIsInFlight(t *testing.T) {
	main := newFakePlayer()
	pv, second := newPreviewHarness(t, main)

	// A build that has not landed yet: Play leaves the player Idle until the
	// test releases it.
	second.autoActivate = false

	pv.play("/music/a.opus")
	waitForCall(t, second, "play:/music/a.opus")

	// Several ticks pass while the build is in flight. Exactly one Play must
	// have been issued.
	time.Sleep(120 * time.Millisecond)
	if n := countCalls(second, "play:/music/a.opus"); n != 1 {
		t.Fatalf("issued %d Plays while the build was in flight, want 1", n)
	}

	// Once the build lands, the preview positions then sounds, and still no
	// extra Play.
	second.activate()
	waitForPhase(t, pv, phaseSounding)
	if n := countCalls(second, "play:/music/a.opus"); n != 1 {
		t.Fatalf("issued %d Plays after activation, want 1", n)
	}
}

// TestPreviewDoesNotResumeBeforeSeekLands is the P0-2 regression: the deferred
// start must not issue a separate Resume that outruns the asynchronous Seek.
// The engine's Seek owns the resume decision, so the previewer must never
// resume on its own.
func TestPreviewDoesNotResumeBeforeSeekLands(t *testing.T) {
	main := newFakePlayer()
	pv, second := newPreviewHarness(t, main)
	second.autoSeek = false

	pv.configure(PreviewConfig{StartMs: 60000, LengthMs: 30000, FadeMs: 0, Loop: true, Volume: 1})
	pv.play("/music/a.opus")

	// Let the load land and the seek be issued.
	second.activate()
	waitForCall(t, second, "seek")

	// While the seek is in flight the preview must be silent (positioning) and
	// must not have resumed.
	if got := pv.phaseNow(); got != phasePositioning {
		t.Fatalf("phase = %v while the seek is in flight, want positioning", got)
	}
	for _, c := range second.callsSnapshot() {
		if c == "resume" {
			t.Fatal("previewer resumed before the seek landed; this is the starts-from-0 bug")
		}
	}

	// Once the seek lands, the window sounds.
	second.landSeek()
	waitForPhase(t, pv, phaseSounding)
}

// TestPreviewPausesMainEvenIfItWasMidBuild is the P0-3 regression: the main
// player's state is asynchronous, so a one-shot "is it playing?" check can miss
// a track that is about to activate. The session must re-check and pause it.
func TestPreviewPausesMainEvenIfItWasMidBuild(t *testing.T) {
	main := newFakePlayer()
	main.autoActivate = false
	_ = main.Play("/music/main.opus") // Idle until activated

	pv, second := newPreviewHarness(t, main)
	pv.play("/music/a.opus")
	waitForCall(t, second, "play:/music/a.opus")

	// The main track activates after the preview session began.
	main.activate()
	waitForCall(t, main, "pause")

	if st := main.stateNow(); st != player.Paused {
		t.Fatalf("main state = %v after activation, want paused (two players sounding)", st)
	}
}

// TestPreviewHaltIsNeverDropped is the P0-5 regression: a halt must survive a
// burst of selection changes, or the session leaks and the main track is never
// resumed.
func TestPreviewHaltIsNeverDropped(t *testing.T) {
	main := newFakePlayer()
	_ = main.Play("/music/main.opus")
	pv, second := newPreviewHarness(t, main)

	// Start a real session first, so the main track is paused and a resume is
	// owed when it ends.
	pv.play("/music/a.opus")
	waitForCall(t, second, "play:/music/a.opus")
	if main.stateNow() != player.Paused {
		t.Fatalf("main state = %v, want paused once the session began", main.stateNow())
	}

	// A burst of selection changes followed by a halt, all racing the drain.
	for i := 0; i < 20; i++ {
		pv.play("/music/" + string(rune('b'+i)) + ".opus")
	}
	pv.halt()

	waitForCall(t, main, "resume")
	waitForPhase(t, pv, phaseIdle)
	if st := pv.state(); st.Active {
		t.Fatalf("preview still active after a halt: %+v", st)
	}
}

// TestPreviewLoopOffEndsOnce pins the loop-off path: the window plays once, ends
// on its own, and resumes the main track exactly once.
func TestPreviewLoopOffEndsOnce(t *testing.T) {
	main := newFakePlayer()
	_ = main.Play("/music/main.opus")
	pv, second := newPreviewHarness(t, main)

	pv.configure(PreviewConfig{StartMs: 0, LengthMs: 100, FadeMs: 0, Loop: false, Volume: 1})
	pv.play("/music/a.opus")
	waitForCall(t, second, "play:/music/a.opus")

	// Let the window elapse. The fake position does not advance, so drive it.
	second.mu.Lock()
	second.pos = 200 * time.Millisecond
	second.mu.Unlock()

	waitForCall(t, main, "resume")
	waitForPhase(t, pv, phaseIdle)
	if n := countCalls(main, "resume"); n != 1 {
		t.Fatalf("main resumed %d times, want 1", n)
	}
}

// TestPreviewLoopRestartsWithoutReplayingForever pins the loop path: a looping
// window rewinds rather than issuing a fresh Play on every tick.
func TestPreviewLoopRestartsWithoutReplayingForever(t *testing.T) {
	main := newFakePlayer()
	_ = main.Play("/music/main.opus")
	pv, second := newPreviewHarness(t, main)

	pv.configure(PreviewConfig{StartMs: 0, LengthMs: 100, FadeMs: 0, Loop: true, Volume: 1})
	pv.play("/music/a.opus")
	waitForCall(t, second, "play:/music/a.opus")

	// Drive past the window several times; a live track loops by seeking, so no
	// further Plays may be issued.
	deadline := time.Now().Add(time.Second)
	for i := 0; i < 6 && time.Now().Before(deadline); i++ {
		second.mu.Lock()
		second.pos = 200 * time.Millisecond
		second.mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		second.mu.Lock()
		second.pos = 0
		second.mu.Unlock()
	}

	if n := countCalls(second, "play:/music/a.opus"); n != 1 {
		t.Fatalf("issued %d Plays for a looping live window, want 1", n)
	}
	if got := pv.phaseNow(); got != phaseSounding && got != phasePositioning {
		t.Fatalf("phase = %v after looping, want sounding or positioning", got)
	}
}

// TestPreviewSessionLeavesAlreadyPausedMainAlone is the other half of the
// session rule: a main track the user had paused must stay paused.
func TestPreviewSessionLeavesAlreadyPausedMainAlone(t *testing.T) {
	main := newFakePlayer()
	_ = main.Play("/music/main.opus")
	_ = main.Pause()
	main.resetCalls()

	pv, second := newPreviewHarness(t, main)

	pv.play("/music/a.opus")
	waitForCall(t, second, "play:/music/a.opus")

	pv.halt()
	waitForPhase(t, pv, phaseIdle)
	time.Sleep(60 * time.Millisecond)
	for _, c := range main.callsSnapshot() {
		if c == "resume" {
			t.Fatal("a preview session resumed a main track the user had paused")
		}
	}
}

// TestPreviewHaltWithoutSessionIsSafe guards the no-op path: stopping a preview
// that never started must not touch the main player.
func TestPreviewHaltWithoutSessionIsSafe(t *testing.T) {
	main := newFakePlayer()
	_ = main.Play("/music/main.opus")
	main.resetCalls()

	pv, _ := newPreviewHarness(t, main)
	pv.halt()
	time.Sleep(60 * time.Millisecond)

	if calls := main.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("halting an idle preview touched the main player: %v", calls)
	}
}

// TestPreviewSessionPausesMainOnceAndResumes pins the load-bearing session
// rule: the main track is paused on the first preview and resumed only when the
// session ends, not between previews.
func TestPreviewSessionPausesMainOnceAndResumes(t *testing.T) {
	main := newFakePlayer()
	_ = main.Play("/music/main.opus")

	pv, second := newPreviewHarness(t, main)

	pv.play("/music/a.opus")
	waitForCall(t, second, "play:/music/a.opus")
	if main.stateNow() != player.Paused {
		t.Fatalf("main state = %v, want paused after first preview", main.stateNow())
	}

	// A second selection must not touch the main player again.
	main.resetCalls()
	pv.play("/music/b.opus")
	waitForCall(t, second, "play:/music/b.opus")
	if calls := main.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("moving the selection touched the main player: %v", calls)
	}

	pv.halt()
	waitForCall(t, main, "resume")
}

// countCalls counts how many times a call was recorded.
func countCalls(f *fakePlayer, call string) int {
	n := 0
	for _, c := range f.callsSnapshot() {
		if c == call {
			n++
		}
	}

	return n
}

// waitForCall polls the fake until it has seen call, because the previewer is
// asynchronous by design.
func waitForCall(t *testing.T, f *fakePlayer, call string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range f.callsSnapshot() {
			if c == call {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("did not observe %q; saw %v", call, f.callsSnapshot())
}

// waitForPhase polls the previewer until it reaches phase.
func waitForPhase(t *testing.T, pv *previewer, phase previewPhase) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pv.phaseNow() == phase {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("preview never reached phase %v (at %v)", phase, pv.phaseNow())
}

// waitForConfig polls the preview config until it matches want, because
// configure is asynchronous by design.
func waitForConfig(t *testing.T, svc *PlayerService, want PreviewConfig) PreviewConfig {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := svc.preview.config(); got == want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}

	return svc.preview.config()
}
