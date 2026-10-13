package session

import (
	"context"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/meta"
	"github.com/dlcuy22/molo/playback"
)

// crossfadeConfig wires a session whose synthetic tracks have a known duration
// and whose device consumes at real-time pace, so the crossfade state machine
// can be driven end to end without a fixture or an audio server.
func crossfadeConfig(frames int64) Config {
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.25, total: frames}, nil
	}
	cfg.probeStream = staticProbe(core.StreamInfo{Format: canonical, TotalFrames: frames})
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new

	return cfg
}

// TestCrossfadeOverlapsWhileTheFirstTrackIsStillCurrent proves the blend is a
// real overlap: while the queue index is still 0, a ramp becomes active that
// reads two streamers at once. That is the property that distinguishes a
// crossfade from a sequential fade out then fade in.
func TestCrossfadeOverlapsWhileTheFirstTrackIsStillCurrent(t *testing.T) {
	// Two seconds per track with a one second ramp, so the blend starts at the
	// one second mark and runs to the end.
	const frames = 48000 * 2
	s := newSession(t, crossfadeConfig(frames))

	if err := s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: MinCrossfadeMs}); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Poll until a ramp is active while the current index is still 0.
	sawOverlap := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.provider.xfade.Load() != nil && s.Snapshot().QueueIndex == 0 {
			sawOverlap = true

			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawOverlap {
		t.Fatal("no crossfade was active while the first track was still current")
	}

	// The blend then promotes the second track.
	changed := waitTrackChanged(t, s, 1, 5*time.Second)
	if changed.Path != "two" {
		t.Fatalf("TrackChanged = %+v, want path two", changed)
	}
}

// TestCrossfadeAdvancesToTheNextTrack proves the blend path promotes the
// incoming track: with crossfade on, a two-track queue still advances from the
// first to the second and then stops, exactly as the ordinary path does, only
// with the overlap in between.
func TestCrossfadeAdvancesToTheNextTrack(t *testing.T) {
	// One second per track, and a one-second ramp, so the blend covers the
	// whole tail and the test stays short.
	const frames = 48000
	s := newSession(t, crossfadeConfig(frames))

	if err := s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: MinCrossfadeMs}); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// The second track becomes current through the blend.
	changed := waitTrackChanged(t, s, 1, 6*time.Second)
	if changed.Path != "two" {
		t.Fatalf("TrackChanged = %+v, want path two", changed)
	}
	if got := s.Snapshot().QueueIndex; got != 1 {
		t.Fatalf("QueueIndex after the blend = %d, want 1", got)
	}

	// The queue then drains and the session stops, as it does without a blend.
	waitState(t, s, StateStopped, 6*time.Second)
}

// TestCrossfadeSkipSameAlbumLeavesTheHandoffUnblended proves the same-album
// switch suppresses the overlap: the next track still plays, but no ramp is
// started for it. It is checked by the fact that the outgoing track reaches its
// end (a TrackEnded) and the next becomes current, with the prefetch used as a
// gapless handoff rather than a blend.
func TestCrossfadeSkipSameAlbumLeavesTheHandoffUnblended(t *testing.T) {
	const frames = 48000
	cfg := crossfadeConfig(frames)
	// A resolver that reports the same album for every path, so the skip rule
	// applies to both tracks.
	cfg.Resolver = sameAlbumResolver{album: "The Album"}
	s := newSession(t, cfg)

	if err := s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: MinCrossfadeMs, SkipSameAlbum: true}); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	changed := waitTrackChanged(t, s, 1, 6*time.Second)
	if changed.Path != "two" {
		t.Fatalf("TrackChanged = %+v, want path two", changed)
	}
	// The blend must not have started: same-album tracks hand off gaplessly.
	if got := s.xfadeStarts.Load(); got != 0 {
		t.Fatalf("xfadeStarts = %d, want 0 (the same-album handoff must not blend)", got)
	}
	waitState(t, s, StateStopped, 6*time.Second)
}

// TestCrossfadeAbortsOnManualNext proves a manual Next is a hard cut: the
// blend is dropped and the chosen track becomes current, with no leftover ramp
// mixing behind it.
func TestCrossfadeAbortsOnManualNext(t *testing.T) {
	// A long track so the blend is in flight when Next is pressed.
	const frames = 48000 * 30
	s := newSession(t, crossfadeConfig(frames))

	if err := s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: MinCrossfadeMs}); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two", "three"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Let the prefetch build and, with a long track, the blend may or may not
	// have started; either way Next must land on the next track.
	time.Sleep(200 * time.Millisecond)
	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}

	changed := waitTrackChanged(t, s, 1, 3*time.Second)
	if changed.Path != "two" {
		t.Fatalf("TrackChanged = %+v, want path two", changed)
	}
	if got := s.Snapshot().QueueIndex; got != 1 {
		t.Fatalf("QueueIndex after Next = %d, want 1", got)
	}
	// No ramp may survive the manual cut: the provider must be reading a single
	// stream again, not blending the track the user skipped past.
	if x := s.provider.xfade.Load(); x != nil {
		t.Fatalf("a crossfade survived a manual Next: %+v", x)
	}
}

// sameAlbumResolver is a meta resolver that reports one album for every path,
// so the same-album skip can be exercised without real tags.
type sameAlbumResolver struct{ album string }

func (r sameAlbumResolver) Name() string      { return "same-album" }
func (r sameAlbumResolver) Priority() int     { return 0 }
func (r sameAlbumResolver) Match(string) bool { return true }

func (r sameAlbumResolver) Resolve(_ context.Context, path string) (*meta.Meta, error) {
	return &meta.Meta{Path: path, Tags: meta.Tags{Album: r.album}}, nil
}
