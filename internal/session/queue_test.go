package session

import (
	"errors"
	"testing"
	"time"

	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/playback"
)

// newQueueSession wires a session whose decoder opens a real fixture and whose
// device actually consumes, so end of track is reached by draining rather than
// by a timer.
func newQueueSession(t *testing.T) (*Session, *recorderFactory) {
	t.Helper()

	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}
	cfg := testConfig()
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	return s, factory
}

func TestPlayStartsTheTrack(t *testing.T) {
	s, _ := newQueueSession(t)
	path := fixture(t, "short_stereo.opus")

	if err := s.Play(path); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	snap := s.Snapshot()
	if snap.Path != path {
		t.Fatalf("Snapshot.Path = %q, want %q", snap.Path, path)
	}
	if snap.QueueIndex != 0 || snap.QueueLen != 1 {
		t.Fatalf("queue = %d of %d, want 0 of 1", snap.QueueIndex, snap.QueueLen)
	}
	if snap.Format != canonicalFormat {
		t.Fatalf("Snapshot.Format = %+v, want %+v", snap.Format, canonicalFormat)
	}
	if ev := waitTrackChanged(t, s, 0, 2*time.Second); ev.Path != path {
		t.Fatalf("TrackChanged = %+v, want path %q", ev, path)
	}
}

func TestPlayQueueSetsTheWholeQueue(t *testing.T) {
	s, _ := newQueueSession(t)
	a, b := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus")

	if err := s.PlayQueue([]string{a, b}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	got := s.Queue()
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("Queue() = %q, want [%q %q]", got, a, b)
	}
	if snap := s.Snapshot(); snap.QueueLen != 2 || snap.QueueIndex != 0 {
		t.Fatalf("queue = %d of %d, want 0 of 2", snap.QueueIndex, snap.QueueLen)
	}
}

func TestAutoAdvancesOnEndOfTrack(t *testing.T) {
	s, _ := newQueueSession(t)
	a, b := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus")

	if err := s.PlayQueue([]string{a, b}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// The first track ends and the second starts without any command.
	changed := waitTrackChanged(t, s, 1, 5*time.Second)
	if changed.Path != b {
		t.Fatalf("TrackChanged = %+v, want path %q", changed, b)
	}
	waitState(t, s, StateStopped, 5*time.Second)
	if got := s.Snapshot().QueueIndex; got != 1 {
		t.Fatalf("QueueIndex after the queue drains = %d, want 1", got)
	}
	if got := s.Snapshot().QueueLen; got != 2 {
		t.Fatalf("QueueLen = %d, want 2", got)
	}
}

func TestAutoAdvanceEmitsTrackEndedThenTrackChanged(t *testing.T) {
	s, _ := newQueueSession(t)
	a, b := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus")

	if err := s.PlayQueue([]string{a, b}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}

	// Collect the tail of the event stream and check that the end of track 0
	// is reported before the start of track 1.
	sawEnded := false
	deadline := time.After(6 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			switch ev := ev.(type) {
			case TrackEnded:
				sawEnded = true
			case TrackChanged:
				if ev.Index == 1 {
					if !sawEnded {
						t.Fatal("TrackChanged for the next track arrived before TrackEnded")
					}

					return
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for the track handoff")
		}
	}
}

func TestNextAndPrevMoveThroughTheQueue(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) { return &toneDecoder{value: 0.5, total: 1 << 40}, nil }
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"a", "b", "c"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	if got := s.Snapshot().QueueIndex; got != 0 {
		t.Fatalf("QueueIndex = %d, want 0", got)
	}

	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 2*time.Second, "index 1", func() bool { return s.Snapshot().QueueIndex == 1 })

	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 2*time.Second, "index 2", func() bool { return s.Snapshot().QueueIndex == 2 })

	// Next past the last track stops rather than wrapping.
	if err := s.Next(); err != nil {
		t.Fatalf("Next past the end: %v", err)
	}
	waitState(t, s, StateStopped, 2*time.Second)
	if got := s.Snapshot().QueueIndex; got != 2 {
		t.Fatalf("QueueIndex after stop = %d, want the last index 2", got)
	}

	// Prev from Stopped starts the previous track.
	if err := s.Prev(); err != nil {
		t.Fatalf("Prev: %v", err)
	}
	waitState(t, s, StatePlaying, 2*time.Second)
	eventually(t, 2*time.Second, "index 1", func() bool { return s.Snapshot().QueueIndex == 1 })
}

func TestEmptyQueueCommandsAreRejected(t *testing.T) {
	s, _ := newQueueSession(t)

	if err := s.Play(""); !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("Play(\"\") = %v, want ErrEmptyPath", err)
	}
	if err := s.PlayQueue(nil); !errors.Is(err, ErrEmptyQueue) {
		t.Fatalf("PlayQueue(nil) = %v, want ErrEmptyQueue", err)
	}
	if err := s.PlayQueue([]string{}); !errors.Is(err, ErrEmptyQueue) {
		t.Fatalf("PlayQueue(empty) = %v, want ErrEmptyQueue", err)
	}
	if err := s.Seek(-time.Second); !errors.Is(err, ErrNegativeSeek) {
		t.Fatalf("Seek(negative) = %v, want ErrNegativeSeek", err)
	}
}

func TestNextAndPrevOnAnEmptyQueueAreInert(t *testing.T) {
	s, _ := newQueueSession(t)

	if err := s.Next(); err != nil {
		t.Fatalf("Next on an empty queue: %v", err)
	}
	if err := s.Prev(); err != nil {
		t.Fatalf("Prev on an empty queue: %v", err)
	}
	if got := s.Snapshot().State; got != StateIdle {
		t.Fatalf("state = %s, want Idle", stateName(got))
	}
}

// TestInsertQueueKeepsTheCurrentTrackPlaying is the whole point of insertion:
// the track that is sounding must not be interrupted, and the inserted ref must
// be what the engine reaches next.
func TestInsertQueueKeepsTheCurrentTrackPlaying(t *testing.T) {
	s, factory := newQueueSession(t)
	a, b, c := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus"), fixture(t, "stereo_2s.opus")

	if err := s.PlayQueue([]string{a, b}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Insert at the current index + 1, which is what "play next" means.
	if err := s.InsertQueue(1, []string{c}); err != nil {
		t.Fatalf("InsertQueue: %v", err)
	}

	// The insert is asynchronous like every command, so wait for the queue it
	// produced rather than reading immediately.
	eventually(t, 2*time.Second, "queue length 3", func() bool {
		return s.Snapshot().QueueLen == 3
	})

	if got := s.Queue(); len(got) != 3 || got[0] != a || got[1] != c || got[2] != b {
		t.Fatalf("Queue() = %q, want [%q %q %q]", got, a, c, b)
	}

	// The current track did not move and is still sounding. A re-seat would
	// have opened a second device; an uninterrupted track opens exactly one.
	snap := s.Snapshot()
	if snap.Path != a || snap.QueueIndex != 0 || snap.State != StatePlaying {
		t.Fatalf("after insert: path=%q index=%d state=%s, want the same track still playing", snap.Path, snap.QueueIndex, stateName(snap.State))
	}
	if factory.count() != 1 {
		t.Fatalf("devices built = %d, want 1: the insert must not rebuild the track", factory.count())
	}

	// Advancing now reaches the inserted track, which is what "play next" was
	// asking for.
	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if changed := waitTrackChanged(t, s, 1, 3*time.Second); changed.Path != c {
		t.Fatalf("TrackChanged = %+v, want the inserted ref %q", changed, c)
	}
}

// TestInsertQueueAppendsWhenNoTrackIsLive pins the fallback: with nothing
// playing there is no "next", so the refs join the end.
func TestInsertQueueAppendsWhenNoTrackIsLive(t *testing.T) {
	s, _ := newQueueSession(t)

	if err := s.InsertQueue(0, []string{"ytm:a"}); err != nil {
		t.Fatalf("InsertQueue on an empty session: %v", err)
	}
	if err := s.InsertQueue(5, []string{"ytm:b"}); err != nil {
		t.Fatalf("InsertQueue past the end: %v", err)
	}
	eventually(t, 2*time.Second, "queue length 2", func() bool {
		return s.Snapshot().QueueLen == 2
	})

	if got := s.Queue(); len(got) != 2 || got[0] != "ytm:a" || got[1] != "ytm:b" {
		t.Fatalf("Queue() = %q, want [ytm:a ytm:b]", got)
	}
	// Nothing started: an insert is not a play.
	if got := s.Snapshot().State; got != StateIdle {
		t.Fatalf("state = %s, want Idle", stateName(got))
	}
}

// TestInsertQueueRejectsBadInput pins the synchronous validation, so a caller
// gets an immediate error rather than a silent no-op.
func TestInsertQueueRejectsBadInput(t *testing.T) {
	s, _ := newQueueSession(t)

	if err := s.InsertQueue(0, nil); !errors.Is(err, ErrEmptyQueue) {
		t.Fatalf("InsertQueue(nil) = %v, want ErrEmptyQueue", err)
	}
	if err := s.InsertQueue(0, []string{""}); !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("InsertQueue(empty ref) = %v, want ErrEmptyPath", err)
	}
}

// TestInsertQueueAndPlayStartsWhenIdle is the "play now" path: with nothing
// live the insert also starts the track, in one command.
func TestInsertQueueAndPlayStartsWhenIdle(t *testing.T) {
	s, _ := newQueueSession(t)
	a, b := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus")

	if err := s.InsertQueueAndPlay(0, []string{a}); err != nil {
		t.Fatalf("InsertQueueAndPlay: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	if snap := s.Snapshot(); snap.Path != a || snap.QueueIndex != 0 || snap.QueueLen != 1 {
		t.Fatalf("snapshot = %+v, want %q at 0 of 1", snap, a)
	}

	// A second ref joins the queue after it, and playing it would cut off the
	// live track, so it is refused rather than interrupting.
	if err := s.InsertQueueAndPlay(1, []string{b}); !errors.Is(err, ErrNoLiveTrack) {
		t.Fatalf("InsertQueueAndPlay over a live track = %v, want ErrNoLiveTrack", err)
	}
	if snap := s.Snapshot(); snap.Path != a || snap.QueueLen != 1 {
		t.Fatalf("a refused insert changed state: %+v", snap)
	}
}

// TestInsertQueueAndPlayDegradesWhenATrackStartedLate pins the control-loop
// re-check: the refusal on the caller's goroutine is only a fast path, and the
// decision is remade where the queue is owned, so a track that started in
// between must not be cut off.
func TestInsertQueueAndPlayDegradesWhenATrackStartedLate(t *testing.T) {
	s, _ := newQueueSession(t)
	a, b := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus")

	if err := s.PlayQueue([]string{a}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Enqueue the command directly, bypassing the caller-side refusal, to model
	// the race where liveness changed after the check. The control loop must
	// degrade it to a plain insert.
	s.enqueue(command{kind: cmdInsertQueue, index: 1, paths: []string{b}, play: true})
	eventually(t, 2*time.Second, "queue length 2", func() bool {
		return s.Snapshot().QueueLen == 2
	})

	snap := s.Snapshot()
	if snap.Path != a || snap.QueueIndex != 0 || snap.State != StatePlaying {
		t.Fatalf("a late insert cut the track off: %+v", snap)
	}
	if got := s.Queue(); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("Queue() = %q, want [%q %q]", got, a, b)
	}
}

// TestInsertQueueBeforeTheCurrentTrackAppends guards the ownership rule: the
// engine cannot re-seat a live streamer, so an insert that would move the
// current track is an append rather than a silent corruption of "next".
func TestInsertQueueBeforeTheCurrentTrackAppends(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"a", "b"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 3*time.Second, "second track live", func() bool {
		return s.Snapshot().QueueIndex == 1
	})

	if err := s.InsertQueue(0, []string{"x"}); err != nil {
		t.Fatalf("InsertQueue: %v", err)
	}
	eventually(t, 2*time.Second, "queue length 3", func() bool {
		return s.Snapshot().QueueLen == 3
	})

	got := s.Queue()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "x" {
		t.Fatalf("Queue() = %q, want [a b x]: an insert before the current track must append", got)
	}
	if snap := s.Snapshot(); snap.QueueIndex != 1 || snap.Path != "b" {
		t.Fatalf("current track moved: index=%d path=%q, want 1 and %q", snap.QueueIndex, snap.Path, "b")
	}
}
