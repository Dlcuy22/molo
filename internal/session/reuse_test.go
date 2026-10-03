package session

import (
	"testing"
	"time"

	"github.com/dlcuy22/molo/playback"
)

func TestDeviceIsReusedAcrossATrackChange(t *testing.T) {
	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}
	cfg := testConfig()
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	a, b := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus")
	if err := s.PlayQueue([]string{a, b}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	eventually(t, 6*time.Second, "the queue to advance to the second track", func() bool {
		return s.Snapshot().QueueIndex == 1
	})

	if got := factory.count(); got != 1 {
		t.Fatalf("the track change built %d devices, want exactly 1 reused device", got)
	}
	if dev := factory.first().(*pumpDevice); dev == nil {
		t.Fatal("the reused device is missing")
	}
}

func TestDeviceIsReusedAcrossStopThenPlay(t *testing.T) {
	// Stop then Play shares most, but not all, of the track-change path: the
	// streamer is torn down and the device is paused rather than re-bound. If
	// the reuse mechanism only worked on a natural track change, this would
	// build a second device.
	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}
	cfg := testConfig()
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	a := fixture(t, "mono_1s.opus")
	if err := s.Play(a); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitState(t, s, StateStopped, 3*time.Second)

	if err := s.Play(a); err != nil {
		t.Fatalf("second Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if got := factory.count(); got != 1 {
		t.Fatalf("Stop then Play built %d devices, want exactly 1 reused device", got)
	}
}
