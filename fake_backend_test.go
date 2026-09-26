package player_test

import (
	"slices"
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/dsp"
	"github.com/dlcuy22/player/playback"
)

// TestFakeBackendRunsTheWholeEngine is the no-audio integration check: the
// registered "fake" device consumes the real session, so a test can drive
// playback, decode and the effect chain without opening a sound card and
// without any risk of leaking audio to the machine's speakers.
//
// It asserts on state and position rather than sound, which is the only way an
// agent or CI runner can verify the audio path.
func TestFakeBackendRunsTheWholeEngine(t *testing.T) {
	if !slices.Contains(playback.Names(), "fake") {
		t.Fatalf("fake backend is not registered: %v", playback.Names())
	}

	p, err := player.New(player.WithBackend("fake"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	// A non-empty chain, so the pipeline path and the meters have work.
	if err := p.ApplyPipeline(dsp.Pipeline{Post: []dsp.Spec{
		{ID: "x1", Kind: "crossfeed", Params: dsp.Values{"cutoff": 700.0, "feed": 4.5}},
	}}); err != nil {
		t.Fatalf("ApplyPipeline: %v", err)
	}
	if got := len(p.Pipeline().Post); got != 1 {
		t.Fatalf("pipeline has %d post stages, want 1", got)
	}

	if err := p.PlayQueue([]string{"decode/testdata/mono_1s.opus"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}

	// The fake device paces at real time, so a one second fixture needs about
	// that long to drain. Poll for the natural end rather than sleeping a
	// fixed amount.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if p.Snapshot().State == player.Stopped {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	snap := p.Snapshot()
	t.Logf("final state=%v backend=%s position=%v duration=%v", snap.State, snap.Backend, snap.Position, snap.Duration)

	if snap.Backend != "fake" {
		t.Errorf("Backend = %q, want %q", snap.Backend, "fake")
	}
	if snap.State != player.Stopped {
		t.Errorf("State = %v, want Stopped after the fixture drained", snap.State)
	}
	if snap.Position <= 0 {
		t.Errorf("Position = %v, want the device to have consumed the stream", snap.Position)
	}
}
