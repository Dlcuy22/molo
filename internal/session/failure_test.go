package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/playback"
)

func TestMissingFileFailsAndStops(t *testing.T) {
	s, _ := newQueueSession(t)

	if err := s.Play(filepath.Join(t.TempDir(), "missing.opus")); err != nil {
		t.Fatalf("Play returned %v, want nil: a bad path is discovered by the session, not validated up front", err)
	}
	ev := waitEvent[Failed](t, s, 3*time.Second)
	if ev.Err == nil {
		t.Fatal("Failed.Err = nil")
	}
	waitState(t, s, StateStopped, 3*time.Second)
}

func TestCorruptFileFailsAndStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.opus")
	if err := os.WriteFile(path, []byte("not an Ogg Opus stream"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	s, _ := newQueueSession(t)
	if err := s.Play(path); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitEvent[Failed](t, s, 3*time.Second)
	waitState(t, s, StateStopped, 3*time.Second)
}

func TestSessionIsUsableAfterAFailure(t *testing.T) {
	s, _ := newQueueSession(t)

	if err := s.Play(filepath.Join(t.TempDir(), "missing.opus")); err != nil {
		t.Fatalf("Play bad: %v", err)
	}
	waitEvent[Failed](t, s, 3*time.Second)
	waitState(t, s, StateStopped, 3*time.Second)

	// A later Play must work normally.
	if err := s.Play(fixture(t, "short_stereo.opus")); err != nil {
		t.Fatalf("Play after failure: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	if got := s.Snapshot().Path; got != fixture(t, "short_stereo.opus") {
		t.Fatalf("Path = %q, want the retried fixture", got)
	}
}

func TestDecoderErrorMidTrackFailsAndStops(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) { return &failingDecoder{}, nil }
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("boom"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	ev := waitEvent[Failed](t, s, 3*time.Second)
	if !errors.Is(ev.Err, errDecodeBoom) {
		t.Fatalf("Failed.Err = %v, want errDecodeBoom", ev.Err)
	}
	waitState(t, s, StateStopped, 3*time.Second)
}

func TestDeviceErrorBecomesFailedAndStops(t *testing.T) {
	dev := newRecordingDevice(nil)
	factory := &recorderFactory{build: func() playback.Device { return dev }}
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) { return &toneDecoder{value: 0.5, total: 1 << 40}, nil }
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	dev.setErr(errors.New("audio server died"))
	waitEvent[Failed](t, s, 3*time.Second)
	waitState(t, s, StateStopped, 3*time.Second)
}

// errDecodeBoom is the sentinel failingDecoder returns, so the test can prove
// the decoder's own error is what reached the Failed event.
var errDecodeBoom = errors.New("decode boom")

// failingDecoder reports a usable format and then fails on the first read.
type failingDecoder struct{}

func (d *failingDecoder) Info() core.StreamInfo {
	return core.StreamInfo{Format: canonical, TotalFrames: 1 << 20}
}

func (d *failingDecoder) ReadFrames([]float32) (int, error) { return 0, errDecodeBoom }

func (d *failingDecoder) Close() error { return nil }
