package session

import (
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/playback"
)

func TestSnapshotDurationIsZeroUntilTheProbeAnswers(t *testing.T) {
	release := make(chan struct{})
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.probeStream = func(string, decode.ProbeOptions) (core.StreamInfo, error) {
		<-release

		return core.StreamInfo{Format: canonical, TotalFrames: 96000}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// The probe is parked, so the duration is unknown and must report 0.
	if got := s.Snapshot().Duration; got != 0 {
		t.Fatalf("Duration while the probe is pending = %v, want 0", got)
	}

	close(release)
	eventually(t, 2*time.Second, "the probe duration", func() bool {
		return s.Snapshot().Duration == 2*time.Second
	})
}

func TestProbeFailureLeavesDurationUnknown(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.probeStream = func(string, decode.ProbeOptions) (core.StreamInfo, error) {
		return core.StreamInfo{Format: canonical, TotalFrames: -1}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Give the probe worker time to run; it reports unknown, which stays 0.
	time.Sleep(100 * time.Millisecond)
	if got := s.Snapshot().Duration; got != 0 {
		t.Fatalf("Duration after an unknown probe = %v, want 0", got)
	}
}

func TestRealProbeFillsDurationFromTheFixture(t *testing.T) {
	// No probe seam: the real decode registry probes the real file.
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play(fixture(t, "stereo_2s.opus")); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 3*time.Second, "the real probe duration", func() bool {
		d := s.Snapshot().Duration

		return d >= 1900*time.Millisecond && d <= 2100*time.Millisecond
	})
}

func TestMetaResolvesAsynchronously(t *testing.T) {
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	path := fixture(t, "stereo_2s.opus")
	if err := s.Play(path); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	eventually(t, 3*time.Second, "the meta resolver to answer", func() bool {
		return s.Snapshot().Meta.Path == path && s.Snapshot().Meta.Tags.Title != ""
	})
}
