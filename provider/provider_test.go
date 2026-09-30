package provider

import (
	"context"
	"errors"
	"testing"
)

// TestLocalAudioClaimsEveryReference pins the catch-all contract: LocalAudio is
// the last-resort fallback, so it must claim anything an earlier provider left
// behind.
func TestLocalAudioClaimsEveryReference(t *testing.T) {
	var p LocalAudio
	if p.Name() != "local" {
		t.Fatalf("Name() = %q, want local", p.Name())
	}
	for _, ref := range []string{"", "song.flac", "ytm:abc123", "/no/such/path"} {
		if !p.Match(ref) {
			t.Fatalf("LocalAudio.Match(%q) = false, want true", ref)
		}
	}
}

// TestLocalAudioOpenHonoursContext proves Open respects a cancelled context,
// which is what keeps Close from hanging on a provider that blocks.
func TestLocalAudioOpenHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (LocalAudio{}).Open(ctx, "song.flac"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open on a cancelled context = %v, want context.Canceled", err)
	}
}

// TestLocalAudioLeavesMetaAndProbeUnset pins the deliberate gap: LocalAudio
// must not describe a track or probe a duration, because the session's own
// resolver and prober are exactly right for a local path and duplicating them
// here would fork the behaviour.
func TestLocalAudioLeavesMetaAndProbeUnset(t *testing.T) {
	src, err := (LocalAudio{}).Open(context.Background(), "song.flac")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if src.Meta.Path != "" || src.Meta.Codec != "" || src.Meta.Container != "" || src.Meta.Tags.Title != "" {
		t.Fatalf("LocalAudio supplied Meta %+v, want the zero value", src.Meta)
	}
	if src.Probe != nil {
		t.Fatal("LocalAudio supplied a Probe, want nil")
	}
	if src.Opener == nil {
		t.Fatal("LocalAudio supplied no Opener")
	}
}

// TestLocalAudioMarksItsSourceLocal pins the signal the session uses to keep the
// local path transparent. The session must not have to inspect the provider's
// identity: a value and a pointer provider must be indistinguishable.
func TestLocalAudioMarksItsSourceLocal(t *testing.T) {
	for _, p := range []AudioProvider{LocalAudio{}, &LocalAudio{}} {
		src, err := p.Open(context.Background(), "song.flac")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if !src.Local {
			t.Fatalf("%T did not set Source.Local, want true", p)
		}
	}
}
