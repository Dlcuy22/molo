package session

import (
	"testing"
	"time"

	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/playback"
)

func stateName(s State) string {
	switch s {
	case StateIdle:
		return "Idle"
	case StatePlaying:
		return "Playing"
	case StatePaused:
		return "Paused"
	case StateStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

func TestCanTransitionTable(t *testing.T) {
	legal := []struct{ from, to State }{
		{StateIdle, StatePlaying},
		{StateIdle, StateStopped},
		{StatePlaying, StatePaused},
		{StatePlaying, StateStopped},
		{StatePaused, StatePlaying},
		{StatePaused, StateStopped},
		{StateStopped, StatePlaying},
	}
	for _, c := range legal {
		if !canTransition(c.from, c.to) {
			t.Errorf("canTransition(%s, %s) = false, want true", stateName(c.from), stateName(c.to))
		}
	}

	// A transition to the same state is a no-op, not a legal edge: the
	// controller must not emit a StateChanged for it.
	same := []State{StateIdle, StatePlaying, StatePaused, StateStopped}
	for _, s := range same {
		if canTransition(s, s) {
			t.Errorf("canTransition(%s, %s) = true, want false for a self edge", stateName(s), stateName(s))
		}
	}

	illegal := []struct{ from, to State }{
		{StateIdle, StatePaused},
		{StateStopped, StatePaused},
		{StatePaused, StateIdle},
		{StatePlaying, StateIdle},
		{StateStopped, StateIdle},
	}
	for _, c := range illegal {
		if canTransition(c.from, c.to) {
			t.Errorf("canTransition(%s, %s) = true, want false", stateName(c.from), stateName(c.to))
		}
	}
}

func TestIllegalCommandsAreRejectedWithoutPanic(t *testing.T) {
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if s.Snapshot().State != StateIdle {
		t.Fatalf("a new session starts in %s, want Idle", stateName(s.Snapshot().State))
	}

	// Pause, Resume and Stop are all illegal from Idle. None may panic, and
	// none may move the state or emit an event.
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause from Idle returned %v, want nil (a rejected command is not a caller error)", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume from Idle returned %v, want nil", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop from Idle returned %v, want nil", err)
	}

	// Drain anything that was (wrongly) emitted; a rejected command emits none.
	select {
	case ev := <-s.Events():
		t.Fatalf("a rejected command emitted %T", ev)
	case <-time.After(50 * time.Millisecond):
	}

	if got := s.Snapshot().State; got != StateIdle {
		t.Fatalf("state after rejected commands = %s, want Idle", stateName(got))
	}
}

func TestStateChangedFiresOnEveryLegalTransition(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) { return &toneDecoder{value: 0.5, total: 1 << 40}, nil }
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if ev := waitEvent[StateChanged](t, s, 2*time.Second); ev.To != StatePlaying {
		t.Fatalf("first StateChanged = %+v, want To Playing", ev)
	}

	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if ev := waitEvent[StateChanged](t, s, 2*time.Second); ev.From != StatePlaying || ev.To != StatePaused {
		t.Fatalf("pause StateChanged = %+v, want Playing -> Paused", ev)
	}

	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if ev := waitEvent[StateChanged](t, s, 2*time.Second); ev.From != StatePaused || ev.To != StatePlaying {
		t.Fatalf("resume StateChanged = %+v, want Paused -> Playing", ev)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if ev := waitEvent[StateChanged](t, s, 2*time.Second); ev.From != StatePlaying || ev.To != StateStopped {
		t.Fatalf("stop StateChanged = %+v, want Playing -> Stopped", ev)
	}
}
