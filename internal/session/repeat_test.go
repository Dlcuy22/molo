package session

import (
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/playback"
)

// repeatConfig wires a session whose synthetic tracks have a known duration and
// whose device consumes at real-time pace, so the end-of-track rule can be
// driven end to end without a fixture or an audio server.
func repeatConfig(frames int64) Config {
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.25, total: frames}, nil
	}
	cfg.probeStream = staticProbe(core.StreamInfo{Format: canonical, TotalFrames: frames})
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new

	return cfg
}

// waitTrackChangedN waits for the n-th TrackChanged at one index, discarding the
// events that belong to other tracks. It is what distinguishes a restart (the
// same index reaches a track change twice) from an advance.
func waitTrackChangedN(t *testing.T, s *Session, index, n int, timeout time.Duration) TrackChanged {
	t.Helper()

	seen := 0
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events channel closed while waiting for TrackChanged %d #%d", index, n)
			}
			if tc, ok := ev.(TrackChanged); ok && tc.Index == index {
				seen++
				if seen == n {
					return tc
				}
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for TrackChanged %d #%d", index, n)

	return TrackChanged{}
}

// TestRepeatOneRestartsTheSameTrack proves repeat one plays the current track
// again rather than advancing: the queue index does not move and a second
// TrackChanged reports the same index.
func TestRepeatOneRestartsTheSameTrack(t *testing.T) {
	const frames = 48000 // one second, so the restart lands quickly
	s := newSession(t, repeatConfig(frames))

	if err := s.SetRepeat(RepeatOne); err != nil {
		t.Fatalf("SetRepeat: %v", err)
	}
	if err := s.PlayQueue([]string{"one"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitTrackChangedN(t, s, 0, 1, 3*time.Second)

	// The track ends and is started again: the same index reaches a track
	// change a second time.
	again := waitTrackChangedN(t, s, 0, 2, 5*time.Second)
	if again.Path != "one" {
		t.Fatalf("TrackChanged = %+v, want path one", again)
	}
	if got := s.Snapshot().QueueIndex; got != 0 {
		t.Fatalf("QueueIndex after repeat one = %d, want 0", got)
	}
}

// TestRepeatAllWrapsFromTheLastTrackToTheFirst proves the queue wraps: the last
// track's end starts the first track again instead of stopping.
func TestRepeatAllWrapsFromTheLastTrackToTheFirst(t *testing.T) {
	const frames = 48000
	s := newSession(t, repeatConfig(frames))

	if err := s.SetRepeat(RepeatAll); err != nil {
		t.Fatalf("SetRepeat: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitTrackChangedN(t, s, 0, 1, 3*time.Second)

	// The first track advances to the second, which then wraps back to the
	// first rather than stopping.
	waitTrackChangedN(t, s, 1, 1, 5*time.Second)
	wrapped := waitTrackChangedN(t, s, 0, 2, 5*time.Second)
	if wrapped.Path != "one" {
		t.Fatalf("wrapped TrackChanged = %+v, want path one", wrapped)
	}
	if got := s.Snapshot().QueueIndex; got != 0 {
		t.Fatalf("QueueIndex after the wrap = %d, want 0", got)
	}
}

// TestRepeatOffStopsAtTheEnd pins today's behaviour: with repeat off the queue
// drains and the session stops at the last track.
func TestRepeatOffStopsAtTheEnd(t *testing.T) {
	const frames = 48000
	s := newSession(t, repeatConfig(frames))

	if err := s.SetRepeat(RepeatOff); err != nil {
		t.Fatalf("SetRepeat: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitTrackChangedN(t, s, 0, 1, 3*time.Second)
	waitTrackChangedN(t, s, 1, 1, 5*time.Second)

	waitState(t, s, StateStopped, 5*time.Second)
	if got := s.Snapshot().QueueIndex; got != 1 {
		t.Fatalf("QueueIndex after stop = %d, want the last index 1", got)
	}
}

// TestRepeatAllWrapsInsideTheShuffledOrder proves the wrap follows the order in
// force rather than the order the refs were queued in. Playback starts on a
// track that is not the queue's first, so shuffling pins that track to the head
// of the shuffled list; the last position then wraps to it. Because the pin
// moves a non-leading ref to the front, the shuffled head and the original head
// are different refs, so the assertion cannot pass on the wrong order.
func TestRepeatAllWrapsInsideTheShuffledOrder(t *testing.T) {
	const frames = 24000 // half a second each, so three tracks stay short
	s := newSession(t, repeatConfig(frames))

	original := []string{"a", "b", "c"}
	if err := s.PlayQueue(original); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitTrackChangedN(t, s, 0, 1, 3*time.Second)

	// Move off the head, so the ref that shuffle pins to the front is "b" and
	// not the ref the original order already has first.
	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	waitTrackChangedN(t, s, 1, 1, 3*time.Second)

	s.shuffleSeedOverride = 7
	if err := s.SetShuffle(true); err != nil {
		t.Fatalf("SetShuffle: %v", err)
	}
	eventually(t, 2*time.Second, "shuffle on", func() bool { return s.Shuffled() })

	shuffled := s.Queue()
	if len(shuffled) != 3 || shuffled[0] != "b" {
		t.Fatalf("shuffled queue = %q, want the playing track %q first", shuffled, "b")
	}
	// The premise of the test: the shuffled head differs from the original head,
	// so a wrap that ignored the shuffle would land on a different ref.
	if shuffled[0] == original[0] {
		t.Fatalf("shuffled head %q equals the original head %q; the test cannot tell the two orders apart", shuffled[0], original[0])
	}

	if err := s.SetRepeat(RepeatAll); err != nil {
		t.Fatalf("SetRepeat: %v", err)
	}
	eventually(t, 2*time.Second, "repeat all", func() bool { return s.Repeat() == RepeatAll })

	// Walk to the last shuffled position, then wrap to the head of the shuffled
	// order. The re-seat shuffle performs emits no TrackChanged, so the earlier
	// waits already drained the events before this point.
	waitTrackChangedN(t, s, 1, 1, 5*time.Second)
	waitTrackChangedN(t, s, 2, 1, 5*time.Second)
	wrapped := waitTrackChangedN(t, s, 0, 1, 5*time.Second)
	if wrapped.Path != shuffled[0] {
		t.Fatalf("wrapped TrackChanged path = %q, want the shuffled head %q", wrapped.Path, shuffled[0])
	}
	if wrapped.Path == original[0] {
		t.Fatalf("wrapped TrackChanged path = %q, the original head: the wrap ignored the shuffled order", wrapped.Path)
	}
}

// TestRepeatOneSetDuringARampDoesNotDoubleAdvance proves the defensive branch in
// feedCrossfadeDone: a ramp that was in flight when the mode flipped to repeat
// one must not be promoted into the next track. If it were, the outgoing
// track's own exhaustion would then restart the current track, skipping a track
// and leaving the queue index on the wrong entry. The check pins that no
// TrackChanged for the next index ever arrives, the index stays put, and a live
// streamer is left playing rather than a parked device with nothing to read.
func TestRepeatOneSetDuringARampDoesNotDoubleAdvance(t *testing.T) {
	// One second per track with a one second ramp, so the ramp starts as soon as
	// the first track plays and the test has the whole second to arm repeat one.
	const frames = 48000
	s := newSession(t, repeatConfig(frames))

	if err := s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: MinCrossfadeMs}); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitTrackChangedN(t, s, 0, 1, 3*time.Second)

	// Catch the ramp while index is still 0, so repeat one is set with a blend
	// genuinely in flight.
	eventually(t, 3*time.Second, "a ramp in flight on track 0", func() bool {
		return s.provider.xfade.Load() != nil && s.Snapshot().QueueIndex == 0
	})
	if err := s.SetRepeat(RepeatOne); err != nil {
		t.Fatalf("SetRepeat: %v", err)
	}
	eventually(t, 2*time.Second, "repeat one", func() bool { return s.Repeat() == RepeatOne })

	// The outgoing track restarts. No TrackChanged for index 1 may have slipped
	// through ahead of it: that would be the skipped track.
	restart := waitTrackChangedN(t, s, 0, 2, 6*time.Second)
	if restart.Path != "one" {
		t.Fatalf("restart TrackChanged = %+v, want path one", restart)
	}

	if got := s.Snapshot().QueueIndex; got != 0 {
		t.Fatalf("QueueIndex after the restart = %d, want 0 (a skip would have moved it to 1)", got)
	}
	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after the restart = %s, want Playing", stateName(got))
	}
	if s.live == nil {
		t.Fatal("no live streamer after the restart: the device was left parked with nothing to read")
	}
	if x := s.provider.xfade.Load(); x != nil {
		t.Fatalf("a crossfade survived the restart: %+v", x)
	}
}

// TestRepeatOneNeverStartsACrossfadePrefetch proves repeat one suppresses the
// blend: with crossfade on and a track long enough to blend, no ramp starts
// because the prefetch always targeted the wrong track.
func TestRepeatOneNeverStartsACrossfadePrefetch(t *testing.T) {
	const frames = 48000 // one second, and a one second ramp
	s := newSession(t, repeatConfig(frames))

	if err := s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: MinCrossfadeMs}); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if err := s.SetRepeat(RepeatOne); err != nil {
		t.Fatalf("SetRepeat: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitTrackChangedN(t, s, 0, 1, 3*time.Second)

	// Let the track run to its end and restart: the blend would have started at
	// the one second mark had repeat one not suppressed it.
	waitTrackChangedN(t, s, 0, 2, 5*time.Second)
	if got := s.xfadeStarts.Load(); got != 0 {
		t.Fatalf("xfadeStarts = %d, want 0 (repeat one must not blend)", got)
	}
}

// TestSetRepeatStoresOffForAnOutOfRangeValue proves an unknown mode falls back
// to off, so a stale or hand-set value cannot wedge the queue.
func TestSetRepeatStoresOffForAnOutOfRangeValue(t *testing.T) {
	s := newSession(t, testConfig())

	if err := s.SetRepeat(RepeatMode(99)); err != nil {
		t.Fatalf("SetRepeat: %v", err)
	}
	eventually(t, 2*time.Second, "repeat off", func() bool { return s.Repeat() == RepeatOff })
}

// TestRepeatChangedIsEmittedOnlyOnARealChange pins the event discipline: a set
// that changes the mode emits RepeatChanged, and a same-value set does not.
func TestRepeatChangedIsEmittedOnlyOnARealChange(t *testing.T) {
	s := newSession(t, testConfig())

	if err := s.SetRepeat(RepeatAll); err != nil {
		t.Fatalf("SetRepeat(RepeatAll): %v", err)
	}
	if ev := waitEvent[RepeatChanged](t, s, 2*time.Second); ev.Mode != RepeatAll {
		t.Fatalf("RepeatChanged = %+v, want RepeatAll", ev)
	}

	// A same-value set must change nothing and emit nothing.
	if err := s.SetRepeat(RepeatAll); err != nil {
		t.Fatalf("SetRepeat(RepeatAll again): %v", err)
	}
	select {
	case ev := <-s.Events():
		if rc, ok := ev.(RepeatChanged); ok {
			t.Fatalf("a same-value set emitted RepeatChanged: %+v", rc)
		}
	case <-time.After(150 * time.Millisecond):
	}
}
