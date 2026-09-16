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
	cfg.openDecoder = func(string) (decode.Decoder, error) { return &toneDecoder{value: 0.5, total: 1 << 40}, nil }
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
