package session

import (
	"testing"
	"time"
)

// TestDefaultCrossfadeIsOffWithReferenceValues pins the fresh-install state:
// the feature is off, so an untouched install behaves as it did before the
// feature existed, and the duration and same-album skip are the default values
// for when it is switched on.
func TestDefaultCrossfadeIsOffWithReferenceValues(t *testing.T) {
	got := DefaultCrossfade()
	if got.Enabled {
		t.Fatal("DefaultCrossfade().Enabled = true, want off")
	}
	if got.DurationMs != DefaultCrossfadeMs {
		t.Fatalf("DurationMs = %d, want %d", got.DurationMs, DefaultCrossfadeMs)
	}
	if !got.SkipSameAlbum {
		t.Fatal("SkipSameAlbum = false, want true")
	}
}

// TestValidateCrossfadeClampsTheDuration proves the range is enforced by
// clamping, not rejection, matching how the gain clamps a bad volume: the
// nearest bound is always a usable value.
func TestValidateCrossfadeClampsTheDuration(t *testing.T) {
	low := ValidateCrossfade(CrossfadeSettings{Enabled: true, DurationMs: 0})
	if low.DurationMs != MinCrossfadeMs {
		t.Fatalf("below-range duration = %d, want %d", low.DurationMs, MinCrossfadeMs)
	}

	high := ValidateCrossfade(CrossfadeSettings{Enabled: true, DurationMs: 999999})
	if high.DurationMs != MaxCrossfadeMs {
		t.Fatalf("above-range duration = %d, want %d", high.DurationMs, MaxCrossfadeMs)
	}

	// A duration already in range is untouched, and the other fields survive.
	mid := ValidateCrossfade(CrossfadeSettings{Enabled: true, DurationMs: 6000, SkipSameAlbum: false})
	if mid.DurationMs != 6000 || !mid.Enabled || mid.SkipSameAlbum {
		t.Fatalf("in-range settings were changed: %+v", mid)
	}
}

// TestSessionCrossfadeRoundTrips proves the settings are stored and read back
// with the same clamping a UI would rely on.
func TestSessionCrossfadeRoundTrips(t *testing.T) {
	s := newSession(t, testConfig())

	if got := s.Crossfade(); got != DefaultCrossfade() {
		t.Fatalf("initial Crossfade() = %+v, want the default", got)
	}

	want := CrossfadeSettings{Enabled: true, DurationMs: 7000, SkipSameAlbum: false}
	if err := s.SetCrossfade(want); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if got := s.Crossfade(); got != want {
		t.Fatalf("Crossfade() = %+v, want %+v", got, want)
	}

	// An out-of-range duration is clamped on the way in.
	if err := s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: -5}); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if got := s.Crossfade().DurationMs; got != MinCrossfadeMs {
		t.Fatalf("clamped duration = %d, want %d", got, MinCrossfadeMs)
	}
}

// TestSetCrossfadeDoesNotBlock pins the non-blocking command contract, the same
// one ApplySettings follows: no disk, no device, no control-loop round trip.
func TestSetCrossfadeDoesNotBlock(t *testing.T) {
	s := newSession(t, testConfig())

	done := make(chan error, 1)
	go func() {
		done <- s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: 5000})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetCrossfade: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SetCrossfade blocked; it must not wait on the engine")
	}
}
