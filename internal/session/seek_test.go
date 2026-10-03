package session

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/playback"
)

func TestSeekPausesThenRepositionsThenResumes(t *testing.T) {
	log := &orderLog{}
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
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

	if got, want := log.snapshot(), []string{"pause", "flush", "seek", "resume"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order = %v, want %v", got, want)
	}
}

func TestSeekFromPauseDoesNotResume(t *testing.T) {
	log := &orderLog{}
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
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

	// A seek from Pause must not start audio again, and it must still drop the
	// queued pre-seek audio.
	if got, want := log.snapshot(), []string{"pause", "flush", "seek"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order = %v, want %v", got, want)
	}
	if got := s.Snapshot().State; got != StatePaused {
		t.Fatalf("state after seek from pause = %s, want Paused", stateName(got))
	}
}

func TestSeekEmitsSeekedAndUpdatesPosition(t *testing.T) {
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
	_ = waitEvent[TrackChanged](t, s, 2*time.Second)

	if err := s.Seek(1 * time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	ev := waitEvent[Seeked](t, s, 2*time.Second)
	if ev.Position != time.Second {
		t.Fatalf("Seeked.Position = %v, want 1s", ev.Position)
	}
}

// TestSeekFlushesTheDeviceBeforeRepositioning pins the fix for audio bleeding
// across a seek. Pausing stops the backend from consuming, but the backend still
// holds what it already read; resuming would replay that, so the queued audio
// must be dropped as part of the seek. Flush has to come after the pause (so no
// read is in flight) and before the reposition (so nothing refills the buffer
// with pre-seek audio).
func TestSeekFlushesTheDeviceBeforeRepositioning(t *testing.T) {
	log := &orderLog{}
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
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
	if ev := waitEvent[Seeked](t, s, 2*time.Second); ev.Position != 500*time.Millisecond {
		t.Fatalf("Seeked.Position = %v, want 500ms", ev.Position)
	}

	if got, want := log.snapshot(), []string{"pause", "flush", "seek", "resume"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order = %v, want %v", got, want)
	}
}

// TestPauseThenSeekStillFlushes covers the paused case: the caller is not
// resuming, but the stale queue must still go, or the audio heard after the
// eventual resume is the pre-seek position.
func TestPauseThenSeekStillFlushes(t *testing.T) {
	log := &orderLog{}
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
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

	if got, want := log.snapshot(), []string{"pause", "flush", "seek"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order = %v, want %v", got, want)
	}
}

// TestTrackChangeFlushesTheDevice covers the same bleed on a track boundary:
// the previous track's queued audio must not leak into the next one.
func TestTrackChangeFlushesTheDevice(t *testing.T) {
	log := &orderLog{}
	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(log) }}
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"a", "b"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	log.reset()
	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 3*time.Second, "the second track to play", func() bool {
		return s.Snapshot().QueueIndex == 1
	})

	ops := log.snapshot()
	if !slices.Contains(ops, "flush") {
		t.Fatalf("a track change did not flush the device; operations = %v", ops)
	}
	pauseAt, flushAt := slices.Index(ops, "pause"), slices.Index(ops, "flush")
	if pauseAt < 0 || flushAt != pauseAt+1 {
		t.Fatalf("flush must directly follow the pause that parks the device; operations = %v", ops)
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
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
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
