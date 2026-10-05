package session

import (
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/playback"
)

// drainQueue reads everything currently buffered, for tests that only care
// that the stream still moves.
func drainQueue(ch <-chan Event) int {
	n := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return n
			}
			n++
		default:
			return n
		}
	}
}

func TestEventQueueKeepsTheNewestAndCountsTheDropped(t *testing.T) {
	// The push may never block, and the newest event must survive a full
	// buffer so a slow UI is not left on a stale state.
	q := newEventQueue[Event](2)
	for i := 0; i < 5; i++ {
		q.push(StateChanged{To: State(i + 1)})
	}
	if got := q.droppedCount(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}

	var got []Event
	for len(q.channel()) > 0 {
		got = append(got, <-q.channel())
	}
	if len(got) != 2 {
		t.Fatalf("kept %d events, want 2", len(got))
	}
	if last := got[len(got)-1].(StateChanged); last.To != State(5) {
		t.Fatalf("newest kept event = %+v, want To 5", last)
	}
}

func TestEventQueuePushesAreNeverBlocking(t *testing.T) {
	q := newEventQueue[Event](1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			q.push(TrackEnded{})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a push blocked; the engine would stall behind a slow consumer")
	}
	if got := q.droppedCount(); got != 999 {
		t.Fatalf("dropped = %d, want 999", got)
	}
}

func TestDropOldestDoesNotStallTheEngine(t *testing.T) {
	// A UI that never reads must not be able to wedge playback. The event
	// buffer is one deep here, so the first track's events overflow it while
	// the session keeps working.
	cfg := testConfig()
	cfg.EventBuffer = 1
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	a, b := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus")
	if err := s.PlayQueue([]string{a, b}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}

	// No consumer at all. The queue still has to advance and finish, which it
	// can only do if no emit ever blocked on the full channel.
	eventually(t, 6*time.Second, "the queue to advance", func() bool {
		return s.Snapshot().QueueIndex == 1
	})
	waitState(t, s, StateStopped, 6*time.Second)

	if got := s.Snapshot().DroppedEvents; got == 0 {
		t.Fatal("DroppedEvents = 0; the full buffer was never observed, so this test proves nothing")
	}
}

func TestPositionIsNotAnEvent(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) { return &toneDecoder{value: 0.5, total: 1 << 40}, nil }
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	_ = waitEvent[TrackChanged](t, s, 2*time.Second)
	// The start handshake may still have one event in flight; give it a moment
	// and consume whatever a fresh track legitimately emits.
	time.Sleep(30 * time.Millisecond)
	drainQueue(s.Events())

	// Position must advance, and doing so must produce no events at all.
	pos := s.Snapshot().Position
	eventually(t, 2*time.Second, "position to advance", func() bool { return s.Snapshot().Position > pos })

	if n := drainQueue(s.Events()); n != 0 {
		t.Fatalf("%d events arrived while the track was playing; position must be polled, not emitted", n)
	}
}

func TestControlLoopStaysResponsiveDuringASlowOpen(t *testing.T) {
	// Opening a decoder is the one slow step, and it must not run on the
	// control goroutine: a Stop issued while a file is opening has to be
	// applied immediately, not after the file is read.
	entered := make(chan struct{})
	release := make(chan struct{})

	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		if path == "slow" {
			close(entered)
			<-release
		}

		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("fast"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if err := s.Play("slow"); err != nil {
		t.Fatalf("Play slow: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow opener never ran")
	}

	// The opener is parked here. A Stop must still be applied while it is.
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitState(t, s, StateStopped, 1*time.Second)

	// Releasing the opener must not resurrect a track the user stopped.
	close(release)
	time.Sleep(100 * time.Millisecond)
	if got := s.Snapshot().State; got != StateStopped {
		t.Fatalf("state after a stale open completed = %s, want Stopped", stateName(got))
	}
}

func TestPlayReturnsBeforeTheTrackIsOpened(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		close(started)
		<-release
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	done := make(chan error, 1)
	go func() { done <- s.Play("slow") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Play: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Play blocked on the decoder opener; every command must be non-blocking")
	}

	// Play returned while the opener is still parked. Confirm the opener did
	// start, so this is a slow open rather than a no-op.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the decoder opener never ran")
	}

	close(release)
	waitState(t, s, StatePlaying, 3*time.Second)
}
