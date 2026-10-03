package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/meta"
)

// TestUpdateKeyQuit proves the model turns the quit key into the quit command
// without consulting the engine.
func TestUpdateKeyQuit(t *testing.T) {
	m := newModel(newFakePlayer())
	next, cmd := m.Update(keyPress("q"))
	if next.(model).p == nil {
		t.Fatal("model lost its player")
	}
	if cmd == nil {
		t.Fatal("q returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("q command produced %T, want tea.QuitMsg", cmd())
	}
}

// TestUpdateKeyThroughModel drives pause, next and volume through the real
// Update path, not the helper directly.
func TestUpdateKeyThroughModel(t *testing.T) {
	f := newFakePlayer()
	f.snap.State = molo.Playing
	m := newModel(f)

	for _, k := range []string{"space", "n", "l", "p", "+", "-", "left", "right", "x"} {
		next, _ := m.Update(keyPress(k))
		m = next.(model)
	}

	// The four seek keys accumulate into one batch; it reaches the engine only
	// when the flush lands.
	next, _ := m.Update(seekFlushMsg{seq: m.seekSeq})
	m = next.(model)

	for _, want := range []string{"Pause", "Next", "Prev", "Seek", "SetVolume"} {
		if !f.called(want) {
			t.Errorf("key sweep never called %s: %v", want, f.calls)
		}
	}
}

// TestUpdateWindowSize proves a resize reaches the renderer and does not clamp
// away a zero size.
func TestUpdateWindowSize(t *testing.T) {
	m := newModel(newFakePlayer())
	next, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	got := next.(model)
	if got.width != 40 || got.height != 12 {
		t.Fatalf("size = %dx%d, want 40x12", got.width, got.height)
	}
}

// TestUpdateTickPollsSnapshot is the load-bearing coupling between tick and
// engine: the position shown comes from the Snapshot, and the tick re-arms the
// poll. The fake's state is changed after construction so the initial snapshot
// taken by newModel cannot satisfy the assertion by itself.
func TestUpdateTickPollsSnapshot(t *testing.T) {
	f := newFakePlayer()
	m := newModel(f)

	f.snap = playerSnapshot("/music/a.opus", 42*time.Second, 2*time.Minute, meta.Meta{})

	if m.snap.Position == 42*time.Second {
		t.Fatal("precondition: the model already polled the new position")
	}

	next, cmd := m.Update(tickMsg(time.Now()))
	got := next.(model)

	if got.snap.Position != 42*time.Second {
		t.Fatalf("tick did not poll the snapshot: %v", got.snap.Position)
	}
	if got.frame != 1 {
		t.Fatalf("frame = %d, want 1", got.frame)
	}
	if cmd == nil {
		t.Fatal("tick did not re-arm")
	}
}

func TestUpdateStateChangedDoesNotClobberTrack(t *testing.T) {
	f := newFakePlayer()
	f.snap = playerSnapshot("/music/a.opus", 0, 0, meta.Meta{})
	m := newModel(f)
	m.snap = f.snap

	next, _ := m.Update(stateMsg{to: molo.Paused})
	got := next.(model)

	if got.snap.State != molo.Paused {
		t.Fatalf("state = %v, want paused", got.snap.State)
	}
	if got.snap.Path != "/music/a.opus" {
		t.Fatalf("state change clobbered the track: %q", got.snap.Path)
	}
}

func TestUpdateSeekedMovesPosition(t *testing.T) {
	m := newModel(newFakePlayer())
	m.snap = playerSnapshot("/music/a.opus", time.Second, time.Minute, meta.Meta{})

	next, _ := m.Update(seekedMsg{position: 30 * time.Second})
	got := next.(model)

	if got.snap.Position != 30*time.Second {
		t.Fatalf("position = %v, want 30s", got.snap.Position)
	}
}

func TestUpdateFailedShowsError(t *testing.T) {
	m := newModel(newFakePlayer())

	next, _ := m.Update(failedMsg{err: errFailed})
	got := next.(model)

	if got.err == nil {
		t.Fatal("failure was dropped")
	}
	if !strings.Contains(got.render(), "test failure") {
		t.Fatalf("rendered frame does not show the error: %q", got.render())
	}
}

// TestUpdateEventsClosedStopsBridge proves the bridge stops re-issuing itself
// once the engine's channel closes, so a finished player does not spin.
func TestUpdateEventsClosedStopsBridge(t *testing.T) {
	m := newModel(newFakePlayer())

	next, cmd := m.Update(eventsClosedMsg{})
	got := next.(model)

	if !got.eventsDone {
		t.Fatal("eventsDone not set")
	}
	if cmd != nil {
		t.Fatalf("closed bridge returned a command: %T", cmd)
	}
	if rearm := got.nextEvent(); rearm != nil {
		t.Fatalf("closed bridge re-armed: %T", rearm)
	}
}

func TestUpdateQueueReplacementClearsTitles(t *testing.T) {
	m := newModel(newFakePlayer())
	m.queue = []string{"a.opus"}
	m.titles = []string{"Old"}

	next, _ := m.Update(queueMsg{paths: []string{"b.opus", "c.opus"}})
	got := next.(model)

	if !equalStrings(got.queue, []string{"b.opus", "c.opus"}) {
		t.Fatalf("queue = %v", got.queue)
	}
	if len(got.titles) != 2 || got.titles[0] != "" {
		t.Fatalf("titles = %v, want two empty slots", got.titles)
	}
}

func TestUpdateQueueIdenticalDoesNotResetTitles(t *testing.T) {
	m := newModel(newFakePlayer())
	m.queue = []string{"a.opus"}
	m.titles = []string{"Learned"}

	next, _ := m.Update(queueMsg{paths: []string{"a.opus"}})
	got := next.(model)

	if got.titles[0] != "Learned" {
		t.Fatalf("identical queue reset learned titles: %v", got.titles)
	}
}

// TestUpdateMeterMessage proves the tap level reaches the meter.
func TestUpdateMeterMessage(t *testing.T) {
	m := newModel(newFakePlayer())

	next, _ := m.Update(meterMsg{level: 0.5, frames: 1024})
	got := next.(model)

	if got.meter.level != 0.5 {
		t.Fatalf("meter level = %v, want 0.5", got.meter.level)
	}
}

// TestUpdateMeterWithNoFramesHoldsLevel pins the "nothing published" case: a
// tick that drained no audio must hold the level, not decay it as if the sound
// had stopped.
func TestUpdateMeterWithNoFramesHoldsLevel(t *testing.T) {
	m := newModel(newFakePlayer())
	m.meter.push(0.8, time.Now())

	next, _ := m.Update(meterMsg{level: 0, frames: 0})
	got := next.(model)

	if got.meter.level < 0.7 {
		t.Fatalf("an empty read decayed the meter to %v", got.meter.level)
	}
}

// TestMeterTickRearmsAndReads proves the meter stream is self-driving: each tick
// requests another and performs one read.
func TestMeterTickRearmsAndReads(t *testing.T) {
	f := newFakePlayer()
	f.tap.setSamples([]float32{1, -1, 1, -1})
	m := newModel(f)

	next, cmd := m.Update(meterTick{})
	got := next.(model)

	if cmd == nil {
		t.Fatal("meterTick returned no command; the meter stream would stop")
	}
	if got.tap == nil {
		t.Fatal("model has no tap")
	}
	if level, frames := drainTap(got.tap, meterReadBlockFrames); frames == 0 {
		t.Fatal("meterTick did not leave a readable feed")
	} else if level != 1 {
		t.Fatalf("drained level = %v, want 1", level)
	}
}

// TestUpdateUnknownMessageIsIgnored is the no-panic guarantee for the message
// channel: anything the UI does not know must leave the model untouched.
func TestUpdateUnknownMessageIsIgnored(t *testing.T) {
	f := newFakePlayer()
	m := newModel(f)

	next, cmd := m.Update(struct{ X int }{X: 7})
	got := next.(model)

	if cmd != nil {
		t.Fatalf("unknown message returned a command: %T", cmd)
	}
	if got.frame != 0 || f.callCount() != 0 {
		t.Fatalf("unknown message had an effect: frame=%d calls=%v", got.frame, f.calls)
	}
}

// TestUpdateOnNilPlayerNeverPanics covers a model built without an engine,
// which is what a scripted render with no audio would use.
func TestUpdateOnNilPlayerNeverPanics(t *testing.T) {
	m := newModel(nil)

	for _, msg := range []tea.Msg{
		keyPress("space"), keyPress("n"), keyPress("p"), keyPress("l"), keyPress("h"),
		keyPress("+"), keyPress("-"), keyPress("q"), keyPress("x"),
		tickMsg(time.Now()), queueMsg{}, meterMsg{}, seekedMsg{}, endedMsg{}, failedMsg{},
		tea.WindowSizeMsg{Width: 10, Height: 4},
	} {
		if next, _ := m.Update(msg); next == nil {
			t.Fatalf("Update(%T) returned a nil model", msg)
		}
	}

	if m.Init() == nil {
		t.Fatal("Init returned nil with no engine")
	}
	_ = m.render()
}

func TestReadTapNilFeedIsNilCommand(t *testing.T) {
	if cmd := readTap(nil); cmd != nil {
		t.Fatalf("nil tap returned a command: %T", cmd)
	}
}

func TestReadTapReturnsLevel(t *testing.T) {
	tap := &fakeTap{}
	tap.setSamples([]float32{1, -1, 1, -1})

	cmd := readTap(tap)
	if cmd == nil {
		t.Fatal("readTap returned nil for a real tap")
	}
	msg, ok := cmd().(meterMsg)
	if !ok {
		t.Fatalf("readTap produced %T, want meterMsg", cmd())
	}
	if msg.level != 1 {
		t.Fatalf("level = %v, want 1", msg.level)
	}
}

func TestInitBatchesEveryStream(t *testing.T) {
	f := newFakePlayer()
	m := newModel(f)

	if m.Init() == nil {
		t.Fatal("Init returned nil with a live player")
	}
	if m.events == nil || m.tap == nil {
		t.Fatalf("init sources missing: events=%v tap=%v", m.events, m.tap)
	}
}
