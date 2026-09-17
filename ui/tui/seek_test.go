package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// repeatKey builds an auto-repeat press the way a terminal reports a held key:
// the same message as keyPress, plus IsRepeat. It goes through the public Key
// shape because the model only ever sees KeyPressMsg.
func repeatKey(name string) tea.KeyPressMsg {
	msg := keyPress(name)
	msg.IsRepeat = true

	return msg
}

// seekFlush completes the coalescing window the way the runtime would: it feeds
// one flush stamped with the model's current sequence, which is what a tick
// delivers once the key stream has been quiet for one window.
func seekFlush(t *testing.T, m model) model {
	t.Helper()

	next, _ := m.Update(seekFlushMsg{seq: m.seekSeq})

	return next.(model)
}

// TestUpdateSeekAutoRepeatDoesNotSeek pins the first half of the fix: a key the
// terminal reports as auto-repeat must not add a seek, no matter how long it is
// held. The cost of a seek grows with the target, so even one repeat is waste.
func TestUpdateSeekAutoRepeatDoesNotSeek(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 10 * time.Second
	f.snap.Duration = 2 * time.Minute
	m := newModel(f)

	const presses = 50
	for range presses {
		next, _ := m.Update(repeatKey("right"))
		m = next.(model)
	}

	if got := f.callCountOf("Seek"); got != 0 {
		t.Fatalf("auto-repeat produced %d seeks, want 0", got)
	}
	if m.seekPending {
		t.Fatal("auto-repeat scheduled a seek batch")
	}

	// A flush that somehow outlives a pure repeat stream must stay a no-op.
	m = seekFlush(t, m)

	if got := f.callCountOf("Seek"); got != 0 {
		t.Fatalf("flush after auto-repeat produced %d seeks, want 0", got)
	}
}

// TestUpdateRapidSeekPressesCoalesce is the second half: distinct presses that
// arrive inside one window must collapse to a single engine call whose target
// is the sum of every delta. Terminals without the Kitty protocol report a held
// key as ordinary presses, so this path is what protects them.
func TestUpdateRapidSeekPressesCoalesce(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 10 * time.Second
	f.snap.Duration = 10 * time.Minute
	m := newModel(f)

	const presses = 40

	for range presses {
		next, _ := m.Update(keyPress("l"))
		m = next.(model)
	}

	if got := f.callCountOf("Seek"); got != 0 {
		t.Fatalf("burst seeked %d times before the window closed, want 0", got)
	}

	m = seekFlush(t, m)

	got := f.callCountOf("Seek")
	if got >= presses {
		t.Fatalf("coalescing did not help: %d seeks for %d presses", got, presses)
	}
	if got != 1 {
		t.Fatalf("batch produced %d seeks, want 1", got)
	}

	want := 10*time.Second + presses*seekStep
	if pos := f.Snapshot().Position; pos != want {
		t.Fatalf("batch landed at %v, want the sum %v", pos, want)
	}
}

// TestUpdateSeekPendingSurvivesDebounceWindow proves the accumulated target is
// not lost between presses: two presses inside one window stay pending, a flush
// stamped for an earlier batch is ignored rather than applied out of order, and
// the real flush lands exactly once on the sum.
func TestUpdateSeekPendingSurvivesDebounceWindow(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = time.Second
	f.snap.Duration = time.Minute
	m := newModel(f)

	next, cmd := m.Update(keyPress("l"))
	m = next.(model)
	if cmd == nil {
		t.Fatal("first seek press scheduled no flush")
	}

	next, cmd = m.Update(keyPress("l"))
	m = next.(model)
	if cmd != nil {
		t.Fatalf("second press inside the window scheduled a second timer: %T", cmd)
	}
	if !m.seekPending {
		t.Fatal("second press did not extend the batch")
	}

	next, _ = m.Update(seekFlushMsg{seq: m.seekSeq - 1})
	m = next.(model)
	if got := f.callCountOf("Seek"); got != 0 {
		t.Fatalf("stale flush seeked %d times, want 0", got)
	}

	m = seekFlush(t, m)

	if got := f.callCountOf("Seek"); got != 1 {
		t.Fatalf("batch landed %d seeks, want 1", got)
	}
	want := time.Second + 2*seekStep
	if pos := f.Snapshot().Position; pos != want {
		t.Fatalf("batch landed at %v, want %v", pos, want)
	}
	if m.seekPending {
		t.Fatal("batch stayed pending after its flush")
	}
}

// TestUpdateSingleSeekPressStillSeeks keeps the ordinary case honest: one tap
// still schedules and lands one seek, and the model is re-armed for the next.
func TestUpdateSingleSeekPressStillSeeks(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 10 * time.Second
	f.snap.Duration = time.Minute
	m := newModel(f)

	next, cmd := m.Update(keyPress("l"))
	m = next.(model)
	if cmd == nil {
		t.Fatal("single seek press scheduled no flush")
	}

	m = seekFlush(t, m)

	if got := f.callCountOf("Seek"); got != 1 {
		t.Fatalf("single press produced %d seeks, want 1", got)
	}
	if pos := f.Snapshot().Position; pos != 15*time.Second {
		t.Fatalf("single press landed at %v, want 15s", pos)
	}

	m = seekFlush(t, m)
	if got := f.callCountOf("Seek"); got != 1 {
		t.Fatalf("second flush of a spent batch seeked again: %d", got)
	}
}

// TestUpdateCoalescedForwardSeekClampsToDuration keeps the engine-safe clamp on
// the coalesced path: a burst of forward seeks near the end must stop at the
// known duration instead of aiming past it.
func TestUpdateCoalescedForwardSeekClampsToDuration(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 55 * time.Second
	f.snap.Duration = time.Minute
	m := newModel(f)

	for range 5 {
		next, _ := m.Update(keyPress("l"))
		m = next.(model)
	}

	m = seekFlush(t, m)

	if pos := f.Snapshot().Position; pos != time.Minute {
		t.Fatalf("coalesced forward seek = %v, want the 60s duration", pos)
	}
	if got := f.callCountOf("Seek"); got != 1 {
		t.Fatalf("coalesced clamped seek produced %d seeks, want 1", got)
	}
}

// TestUpdateIllegalKeyDoesNotSeek keeps the no-panic, no-effect guarantee for
// arbitrary input on the coalescing path.
func TestUpdateIllegalKeyDoesNotSeek(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 10 * time.Second
	f.snap.Duration = time.Minute
	m := newModel(f)

	next, cmd := m.Update(keyPress("x"))
	m = next.(model)

	if cmd != nil {
		t.Fatalf("illegal key returned a command: %T", cmd)
	}
	if f.callCount() != 0 {
		t.Fatalf("illegal key reached the engine: %v", f.calls)
	}
	if m.seekPending {
		t.Fatal("illegal key scheduled a seek")
	}
}

// TestUpdateSeekBurstSpansWindowsWithoutSeeking covers the timer firing in the
// middle of a burst, the realistic case of a user holding or mashing a key for
// longer than one window. Each mid-burst flush must re-arm instead of seeking,
// so a burst of any length reaches the engine exactly once when it goes quiet.
func TestUpdateSeekBurstSpansWindowsWithoutSeeking(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 10 * time.Second
	f.snap.Duration = 10 * time.Minute
	m := newModel(f)

	const bursts = 20

	for range bursts {
		// One key press, then let the window elapse while the user keeps
		// pressing: each flush sees a newer key and must not seek.
		next, _ := m.Update(keyPress("l"))
		m = next.(model)

		next, _ = m.Update(seekFlushMsg{seq: m.seekSeq - 1})
		m = next.(model)
	}

	if got := f.callCountOf("Seek"); got != 0 {
		t.Fatalf("mid-burst flushes seeked %d times, want 0", got)
	}
	if !m.seekPending {
		t.Fatal("burst lost its pending batch")
	}

	m = seekFlush(t, m)

	if got := f.callCountOf("Seek"); got != 1 {
		t.Fatalf("burst reached the engine %d times, want 1", got)
	}
	want := 10*time.Second + bursts*seekStep
	if pos := f.Snapshot().Position; pos != want {
		t.Fatalf("burst landed at %v, want the sum %v", pos, want)
	}
}

// TestUpdateSeekAnchorSurvivesPollClobber is the no-lost-seek guarantee across
// two batches. A full-cost seek can still be running when the 4 Hz poll next
// reads the engine, so the poll may report the pre-seek position. A later batch
// must accumulate from the last commanded target, not from that stale read, or
// the first step would be silently dropped.
func TestUpdateSeekAnchorSurvivesPollClobber(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 10 * time.Second
	f.snap.Duration = 10 * time.Minute
	m := newModel(f)

	next, _ := m.Update(keyPress("l"))
	m = next.(model)
	m = seekFlush(t, m)

	// The engine has not reported the new position yet.
	f.snap.Position = 10 * time.Second
	next, _ = m.Update(tickMsg(time.Now()))
	m = next.(model)

	next, _ = m.Update(keyPress("l"))
	m = next.(model)
	m = seekFlush(t, m)

	if got := f.callCountOf("Seek"); got != 2 {
		t.Fatalf("two batches produced %d seeks, want 2", got)
	}
	if pos := f.Snapshot().Position; pos != 20*time.Second {
		t.Fatalf("second batch landed at %v, want 20s", pos)
	}
}

// TestUpdateSeekedClearsAnchor proves the anchor is dropped once the engine
// confirms, so a later batch tracks the engine's real position again instead of
// drifting from the last command.
func TestUpdateSeekedClearsAnchor(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 10 * time.Second
	f.snap.Duration = 10 * time.Minute
	m := newModel(f)

	next, _ := m.Update(keyPress("l"))
	m = next.(model)
	m = seekFlush(t, m)

	next, _ = m.Update(seekedMsg{position: 15 * time.Second})
	m = next.(model)

	if m.seekAnchored {
		t.Fatal("seeked did not clear the anchor")
	}

	// The engine reports a position the command did not aim at (frame
	// rounding). A new batch must start from that truth, not from 15s.
	f.snap.Position = 14 * time.Second
	next, _ = m.Update(seekedMsg{position: 14 * time.Second})
	m = next.(model)
	next, _ = m.Update(keyPress("l"))
	m = next.(model)
	m = seekFlush(t, m)

	if pos := f.Snapshot().Position; pos != 19*time.Second {
		t.Fatalf("post-confirm batch landed at %v, want 19s", pos)
	}
}

// TestUpdateTrackChangeDropsPendingSeek covers the one way a batched target can
// go stale: the user seeks and then changes track before the window closes. The
// old target would be meaningless on the new track, so it must be discarded.
func TestUpdateTrackChangeDropsPendingSeek(t *testing.T) {
	f := newFakePlayer()
	f.snap.Position = 10 * time.Second
	f.snap.Duration = time.Minute
	m := newModel(f)

	next, _ := m.Update(keyPress("l"))
	m = next.(model)

	next, _ = m.Update(trackMsg{index: 1, path: "/music/b.opus"})
	m = next.(model)

	m = seekFlush(t, m)

	if got := f.callCountOf("Seek"); got != 0 {
		t.Fatalf("pending seek survived a track change: %d seeks", got)
	}
	if m.seekPending {
		t.Fatal("track change left a pending seek")
	}
}
