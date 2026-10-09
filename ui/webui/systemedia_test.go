package main

import (
	"testing"
	"time"
)

// TestSystemMediaTrackMapsSnapshot pins the snapshot-to-track mapping: the
// fields the desktop draws, the millisecond-to-duration conversion, and the
// state flags the MPRIS PlaybackStatus is derived from.
func TestSystemMediaTrackMapsSnapshot(t *testing.T) {
	fake := newFakePlayer()
	svc := &PlayerService{player: fake}

	snap := Snapshot{
		State:    "playing",
		Path:     "/music/a.flac",
		Title:    "A Song",
		Artist:   "An Artist",
		Album:    "An Album",
		Position: 1234,
		Duration: 200000,
		Volume:   0.5,
		Shuffled: true,
		Queue:    []QueueRow{{Index: 0}, {Index: 1}},
	}

	got := svc.systemMediaTrack(snap)

	if got.ID != "/music/a.flac" {
		t.Fatalf("ID = %q, want the path", got.ID)
	}
	if got.Title != "A Song" || got.Artist != "An Artist" || got.Album != "An Album" {
		t.Fatalf("tags = %q/%q/%q", got.Title, got.Artist, got.Album)
	}
	if got.Position != 1234*time.Millisecond || got.Duration != 200000*time.Millisecond {
		t.Fatalf("position/duration = %v/%v", got.Position, got.Duration)
	}
	if !got.Playing || got.Paused || got.Stopped {
		t.Fatalf("state flags = playing:%v paused:%v stopped:%v", got.Playing, got.Paused, got.Stopped)
	}
	if !got.Shuffle {
		t.Fatal("Shuffle = false, want the snapshot's")
	}
	if !got.CanControl || !got.CanSeek || !got.CanGoNext || !got.CanGoPrevious {
		t.Fatalf("capabilities = control:%v seek:%v next:%v prev:%v",
			got.CanControl, got.CanSeek, got.CanGoNext, got.CanGoPrevious)
	}
}

// TestSystemMediaTrackGatesSeekOnDuration pins the one capability that is not
// unconditional: a track whose length the engine has not learned yet must not
// offer a seek, matching the rule the seek bar already follows.
func TestSystemMediaTrackGatesSeekOnDuration(t *testing.T) {
	fake := newFakePlayer()
	svc := &PlayerService{player: fake}

	unknown := svc.systemMediaTrack(Snapshot{State: "playing", Path: "p", Title: "t", Duration: 0})
	if unknown.CanSeek {
		t.Fatal("CanSeek = true for an unknown duration, want false")
	}

	known := svc.systemMediaTrack(Snapshot{State: "playing", Path: "p", Title: "t", Duration: 1000})
	if !known.CanSeek {
		t.Fatal("CanSeek = false for a known duration, want true")
	}
}

// TestSystemMediaTrackNoQueueOffersNoAdvance pins the queue-dependent
// capabilities: with nothing to advance through, next and previous are refused
// rather than offered and ignored.
func TestSystemMediaTrackNoQueueOffersNoAdvance(t *testing.T) {
	fake := newFakePlayer()
	svc := &PlayerService{player: fake}

	got := svc.systemMediaTrack(Snapshot{State: "playing", Path: "p", Title: "t"})
	if got.CanGoNext || got.CanGoPrevious {
		t.Fatalf("advance = next:%v prev:%v, want false with an empty queue", got.CanGoNext, got.CanGoPrevious)
	}
}

// TestSystemMediaCommandsDriveTheEngine pins the inbound direction: each
// transport control reaches the engine method it stands for.
func TestSystemMediaCommandsDriveTheEngine(t *testing.T) {
	fake := newFakePlayer()
	svc := &PlayerService{player: fake}
	cmd := svc.systemMediaCommands()

	cmd.Play()
	if !fake.called("resume") {
		t.Fatal("Play did not resume the engine")
	}
	cmd.Pause()
	if !fake.called("pause") {
		t.Fatal("Pause did not pause the engine")
	}
	cmd.Stop()
	if !fake.called("stop") {
		t.Fatal("Stop did not stop the engine")
	}
	cmd.SetShuffle(true)
	if !fake.called("shuffle:true") {
		t.Fatal("SetShuffle did not reach the engine")
	}
	cmd.SetVolume(0.3)
	if !fake.called("volume") {
		t.Fatal("SetVolume did not reach the engine")
	}
}

// TestSystemMediaPlayPauseToggles pins that PlayPause maps to the same toggle
// the UI button uses, so a media key and a click do the same thing.
func TestSystemMediaPlayPauseToggles(t *testing.T) {
	fake := newFakePlayer()
	fake.state = 0 // molo.Idle; a toggle with nothing playing starts the row
	svc := &PlayerService{player: fake}

	svc.systemMediaCommands().PlayPause()
	// With no queue index the toggle is a no-op, which is what the UI does too;
	// the point is that it does not panic and does not invent a track.
	if fake.stateNow() != 0 {
		t.Fatalf("state = %v, want idle", fake.stateNow())
	}

	fake.state = 1 // molo.Playing
	svc.systemMediaCommands().PlayPause()
	if !fake.called("pause") {
		t.Fatal("PlayPause while playing did not pause")
	}
}

// TestSystemMediaSeekToClampsNegative pins the guard on the absolute seek: a
// negative position from the bus lands at the start rather than at a negative
// offset.
func TestSystemMediaSeekToClampsNegative(t *testing.T) {
	fake := newFakePlayer()
	svc := &PlayerService{player: fake}
	cmd := svc.systemMediaCommands()

	cmd.SeekTo(-5 * time.Second)
	if got := fake.Snapshot().Position; got != 0 {
		t.Fatalf("position = %v, want 0 after a negative seek", got)
	}

	cmd.SeekTo(2500 * time.Millisecond)
	if got := fake.Snapshot().Position; got != 2500*time.Millisecond {
		t.Fatalf("position = %v, want the target", got)
	}
}

// TestSystemMediaSeekResolvesRelativeOffset pins the relative seek the MPRIS
// Seek method sends: it is applied against the live position, not treated as an
// absolute one.
func TestSystemMediaSeekResolvesRelativeOffset(t *testing.T) {
	fake := newFakePlayer()
	fake.state = 1 // molo.Playing
	fake.pos = 30 * time.Second
	svc := &PlayerService{player: fake}

	svc.systemMediaCommands().Seek(-10 * time.Second)
	if got := fake.Snapshot().Position; got != 20*time.Second {
		t.Fatalf("position = %v, want 20s", got)
	}

	// Backwards past the start clamps to zero rather than going negative.
	fake.pos = 2 * time.Second
	svc.systemMediaCommands().Seek(-10 * time.Second)
	if got := fake.Snapshot().Position; got != 0 {
		t.Fatalf("position = %v, want 0 after seeking past the start", got)
	}
}

// TestSystemMediaSyncIsInertWithoutAManager pins the guard: the snapshot pump
// calls sync on every tick, so it must do nothing when the host has no
// transport, which is the case on this test run.
func TestSystemMediaSyncIsInertWithoutAManager(t *testing.T) {
	fake := newFakePlayer()
	svc := &PlayerService{player: fake}

	// Must not panic with a nil manager.
	svc.syncSystemMedia(Snapshot{State: "playing", Path: "p", Title: "t"})
	svc.systemMediaSeeked(time.Second)
}
