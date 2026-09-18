package player_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/core"
)

// newTestPlayer builds a facade backed by the passive device registered in this
// package, so none of these tests touch an audio server.
func newTestPlayer(t *testing.T, opts ...player.Option) player.Player {
	t.Helper()

	all := append([]player.Option{player.WithBackend("facade-test")}, opts...)
	p, err := player.New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	return p
}

func TestWithDecoderIsReflectedInSettings(t *testing.T) {
	p := newTestPlayer(t, player.WithDecoder("opus-pion"))

	if got := p.Settings().Decoder; got != "opus-pion" {
		t.Fatalf("Settings().Decoder = %q, want opus-pion", got)
	}
}

func TestApplySettingsAcceptsAKnownDecoder(t *testing.T) {
	p := newTestPlayer(t)

	next := p.Settings()
	next.Decoder = "opus-libopusfile"
	if err := p.ApplySettings(next); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if got := p.Settings().Decoder; got != "opus-libopusfile" {
		t.Fatalf("Settings().Decoder = %q, want opus-libopusfile", got)
	}
}

func TestApplySettingsRejectsAnUnknownDecoderAndChangesNothing(t *testing.T) {
	p := newTestPlayer(t, player.WithDecoder("opus-pion"))
	before := p.Settings()

	next := before
	next.Decoder = "not-a-codec"
	err := p.ApplySettings(next)
	if !errors.Is(err, player.ErrInvalidSetting) {
		t.Fatalf("ApplySettings error = %v, want ErrInvalidSetting", err)
	}
	if got := p.Settings(); got != before {
		t.Fatalf("a rejected update changed state: before %+v, after %+v", before, got)
	}
}

func TestApplySettingsRejectsAnUnknownBackend(t *testing.T) {
	p := newTestPlayer(t)
	before := p.Settings()

	next := before
	next.Backend = "not-a-backend"
	if err := p.ApplySettings(next); !errors.Is(err, player.ErrInvalidSetting) {
		t.Fatalf("ApplySettings error = %v, want ErrInvalidSetting", err)
	}
	if got := p.Settings(); got != before {
		t.Fatalf("a rejected update changed state: %+v", got)
	}
}

func TestApplySettingsRejectsAnOutOfRangeVolume(t *testing.T) {
	p := newTestPlayer(t)
	before := p.Settings()

	next := before
	next.Volume = 2.5
	if err := p.ApplySettings(next); !errors.Is(err, player.ErrInvalidSetting) {
		t.Fatalf("ApplySettings error = %v, want ErrInvalidSetting", err)
	}
	if got := p.Settings(); got != before {
		t.Fatalf("a rejected update changed state: %+v", got)
	}
}

func TestApplySettingsRejectsAnUnknownProbeMode(t *testing.T) {
	p := newTestPlayer(t)
	before := p.Settings()

	next := before
	next.ProbeMode = core.DurationMode(99)
	if err := p.ApplySettings(next); !errors.Is(err, player.ErrInvalidSetting) {
		t.Fatalf("ApplySettings error = %v, want ErrInvalidSetting", err)
	}
	if got := p.Settings(); got != before {
		t.Fatalf("a rejected update changed state: %+v", got)
	}
}

// TestApplySettingsVolumeTakesEffectImmediately pins the one field that is not
// deferred: the gain is post-ring state, so it applies to the running audio.
func TestApplySettingsVolumeTakesEffectImmediately(t *testing.T) {
	p := newTestPlayer(t, player.WithVolume(1))

	next := p.Settings()
	next.Volume = 0.25
	if err := p.ApplySettings(next); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for p.Snapshot().Volume != 0.25 {
		if time.Now().After(deadline) {
			t.Fatalf("Snapshot().Volume = %v, want 0.25", p.Snapshot().Volume)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestApplySettingsDoesNotBlock(t *testing.T) {
	// The contract is that a settings change is synchronous validation only: no
	// disk, no device, no control-loop round trip. A generous bound catches a
	// regression that made it wait on the engine.
	p := newTestPlayer(t)

	next := p.Settings()
	next.Decoder = "opus-pion"
	next.Volume = 0.5

	done := make(chan error, 1)
	go func() { done <- p.ApplySettings(next) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ApplySettings: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ApplySettings blocked; it must not wait on the engine")
	}
}
