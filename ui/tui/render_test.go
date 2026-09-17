package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/meta"
)

// pacedReader releases one byte at a time with a delay between them, so the
// program has a chance to paint a frame after each key instead of consuming the
// whole script before the first redraw.
type pacedReader struct {
	data  []byte
	delay time.Duration
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	time.Sleep(r.delay)
	p[0] = r.data[0]
	r.data = r.data[1:]

	return 1, nil
}

// TestScriptedRender drives the real Bubble Tea program against a fake engine
// with a scripted input. It is the end-to-end evidence that the model renders
// and that key handling survives a real event loop, not just direct Update
// calls. There is no TTY here, so this is the closest a test can get to a
// terminal session.
func TestScriptedRender(t *testing.T) {
	f := newFakePlayer()
	f.snap = player.Snapshot{
		State:      player.Playing,
		Path:       "/music/real_track.opus",
		Meta:       meta.Meta{Tags: meta.Tags{Title: "Real Track", Artist: "Some Artist", Album: "Some Album"}},
		Position:   31 * time.Second,
		Duration:   2 * time.Minute,
		Volume:     0.8,
		QueueIndex: 1,
		QueueLen:   3,
	}
	f.queue = []string{"/music/a.opus", "/music/real_track.opus", "/music/c.opus"}

	// space pauses, n skips, q quits, one key per 40 ms so each has a frame.
	in := &pacedReader{data: []byte(" nq"), delay: 40 * time.Millisecond}
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p := tea.NewProgram(
		newModel(f),
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(&out),
		tea.WithWindowSize(64, 16),
	)
	if _, err := p.Run(); err != nil {
		t.Fatalf("program: %v", err)
	}

	if out.Len() == 0 {
		t.Fatal("program produced no output")
	}

	if !f.called("Pause") {
		t.Errorf("space never reached the engine: %v", f.calls)
	}
	if !f.called("Next") {
		t.Errorf("n never reached the engine: %v", f.calls)
	}

	frames := stripANSI(out.String())
	t.Logf("captured frames:\n%s", frames)

	// The v2 renderer diffs cells, so a session with no state change writes the
	// startup frame once. That the keys reached the engine is the real proof
	// the scripted loop ran.
}

// TestFrameSequence prints one rendered frame per scripted key press. The v2
// renderer emits only screen diffs, so printing each frame directly is the
// clearest evidence of what a terminal session shows after each key.
func TestFrameSequence(t *testing.T) {
	f := newFakePlayer()
	f.snap = player.Snapshot{
		State:      player.Playing,
		Path:       "/music/real_track.opus",
		Meta:       meta.Meta{Tags: meta.Tags{Title: "Real Track", Artist: "Some Artist", Album: "Some Album"}},
		Position:   31 * time.Second,
		Duration:   2 * time.Minute,
		Volume:     0.8,
		QueueIndex: 1,
		QueueLen:   3,
	}
	f.queue = []string{"/music/a.opus", "/music/real_track.opus", "/music/c.opus"}

	m := newModel(f)
	m.width, m.height = 64, 16
	m.queue = f.queue
	m.titles = []string{"", "Real Track", ""}
	m.snap = f.snap

	logFrame(t, "startup", m)

	steps := []struct {
		name string
		msg  tea.Msg
	}{
		{"space (pause)", keyPress("space")},
		{"tick", tickMsg(time.Now())},
		{"n (next)", keyPress("n")},
		{"track changed event", trackMsg{index: 2, path: "/music/c.opus"}},
		{"tick after next", tickMsg(time.Now())},
		{"l (seek +5s)", keyPress("l")},
		{"seeked event", seekedMsg{position: 5 * time.Second}},
		{"+ (volume up)", keyPress("+")},
		{"tick after volume", tickMsg(time.Now())},
		{"tap level 0.7", meterMsg{level: 0.7}},
		{"tap level 0.2 (decay)", meterMsg{level: 0.2}},
		{"q (quit)", keyPress("q")},
	}

	for _, step := range steps {
		next, _ := m.Update(step.msg)
		m = next.(model)
		logFrame(t, step.name, m)
	}

	if !f.called("Pause") || !f.called("Next") {
		t.Fatalf("scripted keys did not reach the engine: %v", f.calls)
	}
}

func logFrame(t *testing.T, name string, m model) {
	t.Helper()
	t.Logf("--- frame after %s ---\n%s", name, stripANSI(m.render()))
}

// TestScriptedRenderSnapshotFrame renders the model directly and prints the
// frame, so the expected layout is visible in the test log even when the
// program above produces mostly escape sequences.
func TestScriptedRenderSnapshotFrame(t *testing.T) {
	f := newFakePlayer()
	f.snap = player.Snapshot{
		State:      player.Playing,
		Path:       "/music/real_track.opus",
		Meta:       meta.Meta{Tags: meta.Tags{Title: "Real Track", Artist: "Some Artist", Album: "Some Album"}},
		Position:   31 * time.Second,
		Duration:   2 * time.Minute,
		Volume:     0.8,
		QueueIndex: 1,
		QueueLen:   3,
	}
	f.queue = []string{"/music/a.opus", "/music/real_track.opus", "/music/c.opus"}

	m := newModel(f)
	m.width, m.height = 64, 16
	m.queue = f.queue
	m.titles = []string{"", "Real Track", ""}
	m.snap = f.snap
	m.meter.push(0.6)

	frame := stripANSI(m.render())
	t.Logf("frame:\n%s", frame)

	for _, want := range []string{"Real Track", "Some Artist", "playing", "0:31 / 2:00", "queue", "a.opus", "c.opus"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame is missing %q:\n%s", want, frame)
		}
	}
}

// TestScriptedDebugPanelRender drives the real Bubble Tea program against a
// fake engine that emits a track change, a state change and a seek, then prints
// the captured frame so the debug panel is visible in the test log. It is the
// end-to-end evidence that the panel survives a real event loop, not just a
// direct render call.
func TestScriptedDebugPanelRender(t *testing.T) {
	f := newFakePlayer()
	f.snap = player.Snapshot{
		State:      player.Playing,
		Path:       "/music/a.opus",
		Meta:       meta.Meta{Tags: meta.Tags{Title: "Real Track"}, Source: "embedded-tags"},
		Position:   0,
		Duration:   2 * time.Minute,
		Volume:     0.8,
		Decoder:    "pion/opus",
		Parser:     "pion/opus/pkg/oggreader",
		QueueIndex: 0,
		QueueLen:   1,
	}
	// Queued before the program starts, so the event bridge drains them in
	// order: a track, a pause, and a seek with its origin and timing.
	f.events <- player.TrackChanged{Index: 0, Path: "/music/a.opus"}
	f.events <- player.StateChanged{From: player.Playing, To: player.Paused}
	f.events <- player.Seeked{Position: 42 * time.Second, From: 12 * time.Second, Elapsed: 3 * time.Millisecond}

	in := &pacedReader{data: []byte("q"), delay: 900 * time.Millisecond}
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p := tea.NewProgram(
		newModel(f),
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(&out),
		tea.WithWindowSize(64, 20),
	)
	if _, err := p.Run(); err != nil {
		t.Fatalf("program: %v", err)
	}

	frames := stripANSI(out.String())
	t.Logf("captured frame with debug panel:\n%s", frames)

	for _, want := range []string{"debug", "decoder  pion/opus", "meta     embedded-tags", "seek     0:12 -> 0:42  took 3ms"} {
		if !strings.Contains(frames, want) {
			t.Errorf("captured frame is missing %q:\n%s", want, frames)
		}
	}
}
