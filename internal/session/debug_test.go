package session

import (
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/playback"
)

// describedToneDecoder is a synthetic decoder that also names the components it
// stands in for, so the session's diagnostics path is testable without a real
// codec or file.
type describedToneDecoder struct {
	toneDecoder
	decoder string
	parser  string
}

func (d *describedToneDecoder) DecoderName() string { return d.decoder }
func (d *describedToneDecoder) ParserName() string  { return d.parser }

// TestTrackChangeClearsStaleLabels proves the labels are per-track: while the
// replacement build is still opening, the snapshot must not keep describing the
// track that was just torn down.
func TestTrackChangeClearsStaleLabels(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	// Released on every exit path: a parked opener that outlives a failed
	// assertion would deadlock the session's shutdown.
	defer close(release)
	var once sync.Once

	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		if path == "slow" {
			once.Do(func() { close(entered) })
			<-release
		}

		return &describedToneDecoder{
			toneDecoder: toneDecoder{value: 0.5, total: 1 << 40},
			decoder:     "codec-" + path,
			parser:      "parser-" + path,
		}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("first"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "first track labels", func() bool {
		return s.Snapshot().Decoder == "codec-first"
	})

	if err := s.PlayQueue([]string{"slow", "next"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow opener never ran")
	}

	// The new track is current but its build is still parked. Its labels cannot
	// be known yet, and the previous track's must not stand in for them.
	snap := s.Snapshot()
	if snap.Path != "slow" {
		t.Fatalf("path = %q, want the new track", snap.Path)
	}
	if snap.Decoder != "" || snap.Parser != "" {
		t.Fatalf("stale labels survived a track change: decoder=%q parser=%q", snap.Decoder, snap.Parser)
	}
}

// TestSnapshotDescribesTheCurrentDecoder proves the labels reach the snapshot a
// UI polls.
func TestSnapshotDescribesTheCurrentDecoder(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &describedToneDecoder{
			toneDecoder: toneDecoder{value: 0.5, total: 1 << 40},
			decoder:     "alpha-codec",
			parser:      "alpha-parser",
		}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	eventually(t, 2*time.Second, "decoder labels", func() bool {
		return s.Snapshot().Decoder == "alpha-codec" && s.Snapshot().Parser == "alpha-parser"
	})
}

// TestSnapshotDescribesTheNewTrackAfterAChange is the load-bearing per-track
// assertion: after the queue moves, the labels must describe the streamer that
// is now live rather than the one that was replaced.
func TestSnapshotDescribesTheNewTrackAfterAChange(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &describedToneDecoder{
			toneDecoder: toneDecoder{value: 0.5, total: 1 << 40},
			decoder:     "codec-" + path,
			parser:      "parser-" + path,
		}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"a", "b"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "track a labels", func() bool {
		return s.Snapshot().Decoder == "codec-a"
	})

	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 3*time.Second, "track b labels", func() bool {
		snap := s.Snapshot()
		return snap.QueueIndex == 1 && snap.Decoder == "codec-b" && snap.Parser == "parser-b"
	})
}

// TestStaleBuildLeavesNoWrongLabels proves a build that completes after the
// queue moved on is discarded whole: its decoder labels must never be attached
// to the track that is actually playing.
func TestStaleBuildLeavesNoWrongLabels(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})

	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		if path == "slow" {
			close(entered)
			<-release
		}

		return &describedToneDecoder{
			toneDecoder: toneDecoder{value: 0.5, total: 1 << 40},
			decoder:     "codec-" + path,
			parser:      "parser-" + path,
		}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"slow", "fast"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow opener never ran")
	}

	// Move the queue on while the first build is parked, then let it finish.
	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 3*time.Second, "the replacement track", func() bool {
		return s.Snapshot().QueueIndex == 1 && s.Snapshot().Decoder == "codec-fast"
	})

	close(release)
	time.Sleep(100 * time.Millisecond)

	snap := s.Snapshot()
	if snap.Decoder != "codec-fast" || snap.Parser != "parser-fast" {
		t.Fatalf("stale build leaked labels: decoder=%q parser=%q", snap.Decoder, snap.Parser)
	}
}

// TestSeekedCarriesOriginAndElapsed proves a seek reports where it started, not
// just where it landed. The device is paused first so the origin is frozen and
// the exact assertion is deterministic rather than racing a live read.
func TestSeekedCarriesOriginAndElapsed(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, s, StatePaused, 2*time.Second)

	before := s.Snapshot().Position
	if err := s.Seek(500 * time.Millisecond); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	ev := waitEvent[Seeked](t, s, 2*time.Second)
	if ev.Position != 500*time.Millisecond {
		t.Fatalf("Seeked.Position = %v, want 500ms", ev.Position)
	}
	if ev.From != before {
		t.Fatalf("Seeked.From = %v, want the pre-seek position %v", ev.From, before)
	}
	if ev.Elapsed < 0 {
		t.Fatalf("Seeked.Elapsed = %v, want non-negative", ev.Elapsed)
	}
}
