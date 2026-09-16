package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/playback"
)

func TestSeekPausesThenRepositionsThenResumes(t *testing.T) {
	log := &orderLog{}
	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40, log: log}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(log) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	log.reset()
	if err := s.Seek(500 * time.Millisecond); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	ev := waitEvent[Seeked](t, s, 2*time.Second)
	if ev.Position != 500*time.Millisecond {
		t.Fatalf("Seeked.Position = %v, want 500ms", ev.Position)
	}

	if got, want := log.snapshot(), []string{"pause", "seek", "resume"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order = %v, want %v", got, want)
	}
}

func TestSeekFromPauseDoesNotResume(t *testing.T) {
	log := &orderLog{}
	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40, log: log}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(log) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, s, StatePaused, 2*time.Second)

	log.reset()
	if err := s.Seek(250 * time.Millisecond); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if ev := waitEvent[Seeked](t, s, 2*time.Second); ev.Position != 250*time.Millisecond {
		t.Fatalf("Seeked.Position = %v, want 250ms", ev.Position)
	}

	// A seek from Pause must not start audio again.
	if got, want := log.snapshot(), []string{"pause", "seek"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order = %v, want %v", got, want)
	}
	if got := s.Snapshot().State; got != StatePaused {
		t.Fatalf("state after seek from pause = %s, want Paused", stateName(got))
	}
}

func TestSeekEmitsSeekedAndUpdatesPosition(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	_ = waitEvent[TrackChanged](t, s, 2*time.Second)

	if err := s.Seek(1 * time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	ev := waitEvent[Seeked](t, s, 2*time.Second)
	if ev.Position != time.Second {
		t.Fatalf("Seeked.Position = %v, want 1s", ev.Position)
	}
}

func TestSeekOnAnIdleSessionIsInert(t *testing.T) {
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Seek(time.Second); err != nil {
		t.Fatalf("Seek before any track: %v", err)
	}
	if got := s.Snapshot().State; got != StateIdle {
		t.Fatalf("state = %s, want Idle", stateName(got))
	}
}

func TestSeekReportsButSurvivesAFailedReposition(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40, seekErr: errors.New("cannot seek")}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if err := s.Seek(time.Second); err != nil {
		t.Fatalf("Seek returned a validation error for an engine failure: %v", err)
	}
	if ev := waitEvent[Failed](t, s, 2*time.Second); ev.Err == nil {
		t.Fatal("Failed event carried a nil error")
	}
	// A failed seek is not fatal, so the track keeps playing.
	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a failed seek = %s, want Playing", stateName(got))
	}
}
