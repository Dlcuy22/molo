package session

import (
	"math"
	"testing"
	"time"

	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/playback"
)

func TestSetVolumeUpdatesTheSnapshotImmediately(t *testing.T) {
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if got := s.Snapshot().Volume; got != 1 {
		t.Fatalf("initial Volume = %v, want 1", got)
	}

	s.SetVolume(0.25)
	if got := s.Snapshot().Volume; got != 0.25 {
		t.Fatalf("Volume after SetVolume = %v, want 0.25", got)
	}

	// Out-of-range values clamp, matching the gain module's contract.
	s.SetVolume(5)
	if got := s.Snapshot().Volume; got != 1 {
		t.Fatalf("Volume after SetVolume(5) = %v, want the clamp to 1", got)
	}
	s.SetVolume(math.NaN())
	if got := s.Snapshot().Volume; got != 0 {
		t.Fatalf("Volume after SetVolume(NaN) = %v, want the clamp to 0", got)
	}
}

func TestVolumeChangesTheSamplesOnTheWire(t *testing.T) {
	// The gain is post-ring, applied by the session's provider on the way to
	// the device. This reads the provider directly, so the assertion is about
	// the audio the device would receive, not about the snapshot.
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 1.0, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return s.Snapshot().Stats.Buffered > 0 })

	buf := make([]float32, 256*canonical.Ch)
	s.SetVolume(1)
	if _, err := s.provider.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames at unity: %v", err)
	}
	for i, v := range buf {
		if v != 1 {
			t.Fatalf("sample %d = %v at unity, want 1", i, v)
		}
	}

	s.SetVolume(0.5)
	if _, err := s.provider.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames at half: %v", err)
	}
	for i, v := range buf {
		if v != 0.5 {
			t.Fatalf("sample %d = %v at half volume, want 0.5", i, v)
		}
	}

	s.SetVolume(0)
	if _, err := s.provider.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames at mute: %v", err)
	}
	for i, v := range buf {
		if v != 0 {
			t.Fatalf("sample %d = %v at mute, want silence", i, v)
		}
	}
}

func TestVolumeSurvivesATrackChange(t *testing.T) {
	// Volume lives on the gain module, not on the stream, so a track change
	// must not reset it.
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 1.0, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"a", "b"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	s.SetVolume(0.3)

	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 2*time.Second, "the next track", func() bool { return s.Snapshot().QueueIndex == 1 })
	if got := s.Snapshot().Volume; got != 0.3 {
		t.Fatalf("Volume after a track change = %v, want 0.3", got)
	}
}
