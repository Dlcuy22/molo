package session

import (
	"runtime"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/playback"
)

func TestCloseIsIdempotent(t *testing.T) {
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play(fixture(t, "short_stereo.opus")); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCloseBeforeAnyTrack(t *testing.T) {
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCloseDuringPlaybackDoesNotLeakGoroutines(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	waitEvent[TrackChanged](t, s, 2*time.Second)

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	before := runtime.NumGoroutine()

	// Close while audio is in flight: it must return promptly and leave no
	// goroutine behind.
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close during playback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close during playback did not return")
	}

	eventually(t, 3*time.Second, "goroutines to settle", func() bool {
		runtime.GC()

		return runtime.NumGoroutine() <= before
	})
}

func TestClosedSessionAcceptsCommandsWithoutPanic(t *testing.T) {
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Commands after Close are a caller bug, but they must not panic or wedge.
	// Play validates synchronously, so it reports the closed session.
	if err := s.Play(""); err == nil {
		t.Fatal("Play(\"\") after Close returned nil, want a validation error")
	}
	_ = s.Next()
	_ = s.Prev()
	_ = s.Pause()
	_ = s.Resume()
	_ = s.Stop()
	_ = s.Seek(time.Second)
	s.SetVolume(0.5)
	_ = s.Queue()
	_ = s.Snapshot()
}
