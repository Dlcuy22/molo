//go:build linux

package systemedia

import (
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// TestPlaybackStatusMapsTheThreeStates pins the MPRIS enum: playing, paused,
// and everything else as Stopped, which is the spec's "no track is playing".
func TestPlaybackStatusMapsTheThreeStates(t *testing.T) {
	cases := []struct {
		track TrackInfo
		want  string
	}{
		{TrackInfo{Playing: true}, "Playing"},
		{TrackInfo{Paused: true}, "Paused"},
		{TrackInfo{Stopped: true}, "Stopped"},
		{TrackInfo{}, "Stopped"},
	}
	for _, c := range cases {
		if got := playbackStatus(c.track); got != c.want {
			t.Fatalf("playbackStatus(%+v) = %q, want %q", c.track, got, c.want)
		}
	}
}

// TestLoopNameNormalizes pins the loop mapping: only the two non-default spec
// values survive, and anything unknown falls back to None rather than reaching
// the bus as an invalid string.
func TestLoopNameNormalizes(t *testing.T) {
	cases := map[string]string{
		"":          "None",
		"None":      "None",
		"Track":     "Track",
		"Playlist":  "Playlist",
		"bogus":     "None",
		"tracklist": "None",
	}
	for in, want := range cases {
		if got := loopName(in); got != want {
			t.Fatalf("loopName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTrackPathIsStableAndValid pins the mpris:trackid contract: a valid D-Bus
// object path, stable for one reference and distinct across references, with
// the empty reference mapping to the spec's NoTrack sentinel.
func TestTrackPathIsStableAndValid(t *testing.T) {
	a := trackPath("ytm:abc")
	if a != trackPath("ytm:abc") {
		t.Fatal("trackPath is not stable for the same reference")
	}
	if a == trackPath("ytm:def") {
		t.Fatal("trackPath collides across references")
	}
	if !a.IsValid() {
		t.Fatalf("trackPath produced an invalid object path %q", a)
	}
	if got := trackPath(""); got != noTrack {
		t.Fatalf("trackPath(\"\") = %q, want NoTrack", got)
	}
}

// TestMetadataMapCarriesTheTaggedFields pins the xesam mapping: present fields
// only, the artist as a string array, the length in microseconds, and the
// artwork as mpris:artUrl when there is one.
func TestMetadataMapCarriesTheTaggedFields(t *testing.T) {
	track := TrackInfo{
		ID:       "/music/a.flac",
		Title:    "A Song",
		Artist:   "An Artist",
		Album:    "An Album",
		Duration: 90 * time.Second,
	}
	meta := metadataMap(track, "file:///tmp/a.png")

	if got := meta["xesam:title"].Value(); got != "A Song" {
		t.Fatalf("xesam:title = %v", got)
	}
	artists, ok := meta["xesam:artist"].Value().([]string)
	if !ok || len(artists) != 1 || artists[0] != "An Artist" {
		t.Fatalf("xesam:artist = %v, want a one-element string array", meta["xesam:artist"].Value())
	}
	if got := meta["xesam:album"].Value(); got != "An Album" {
		t.Fatalf("xesam:album = %v", got)
	}
	if got := meta["mpris:length"].Value(); got != int64(90_000_000) {
		t.Fatalf("mpris:length = %v, want microseconds", got)
	}
	if got := meta["mpris:artUrl"].Value(); got != "file:///tmp/a.png" {
		t.Fatalf("mpris:artUrl = %v", got)
	}
	if got := meta["mpris:trackid"].Value(); got != trackPath(track.ID) {
		t.Fatalf("mpris:trackid = %v, want the track's path", got)
	}
}

// TestMetadataMapOmitsUnknownFields pins the omission rule: a missing key means
// "unknown" to a client, while an empty string would be a real blank value, so
// untagged and duration-less tracks must not carry those keys.
func TestMetadataMapOmitsUnknownFields(t *testing.T) {
	meta := metadataMap(TrackInfo{ID: "p"}, "")

	for _, key := range []string{"xesam:title", "xesam:artist", "xesam:album", "mpris:length", "mpris:artUrl"} {
		if _, ok := meta[key]; ok {
			t.Fatalf("%s present for an untagged, duration-less track", key)
		}
	}
	if _, ok := meta["mpris:trackid"]; !ok {
		t.Fatal("mpris:trackid missing; the spec requires it for any current track")
	}
}

// TestSetPositionRejectsAStaleTrack pins the race guard the spec calls for: a
// position aimed at a track that is no longer current is ignored, so a seek
// cannot land on the wrong track.
func TestSetPositionRejectsAStaleTrack(t *testing.T) {
	called := make(chan time.Duration, 1)
	b := &backend{
		cmd:     Commands{SeekTo: func(d time.Duration) { called <- d }},
		started: true,
		trackID: trackPath("current"),
	}
	v := playerView{b}

	// A stale id is a silent no-op.
	if err := v.SetPosition(trackPath("stale"), 5_000_000); err != nil {
		t.Fatalf("SetPosition(stale) returned %v", err)
	}
	select {
	case d := <-called:
		t.Fatalf("a stale SetPosition reached the engine with %v", d)
	case <-time.After(20 * time.Millisecond):
	}

	// The current id is applied.
	if err := v.SetPosition(trackPath("current"), 5_000_000); err != nil {
		t.Fatalf("SetPosition(current) returned %v", err)
	}
	select {
	case d := <-called:
		if d != 5*time.Second {
			t.Fatalf("seek target = %v, want 5s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("SetPosition for the current track did not reach the engine")
	}
}

// TestDispatchDropsCommandsAfterStop pins the shutdown guard: a command that
// arrives once the transport is stopped is refused, because the application it
// targets is being torn down. The return value is what a setter reports to the
// client.
func TestDispatchDropsCommandsAfterStop(t *testing.T) {
	ran := make(chan struct{}, 1)
	b := &backend{started: false}

	if b.dispatch(func() { ran <- struct{}{} }) {
		t.Fatal("dispatch reported success after stop")
	}
	select {
	case <-ran:
		t.Fatal("dispatch ran a command after stop")
	case <-time.After(20 * time.Millisecond):
	}

	// A started transport runs it and reports success.
	b.started = true
	if !b.dispatch(func() { ran <- struct{}{} }) {
		t.Fatal("dispatch reported failure while started")
	}
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("dispatch dropped a command while started")
	}
}

// TestDispatchRefusesANilCommand pins the other refusal path: a control the
// engine does not offer is reported as not controllable rather than accepted
// and ignored.
func TestDispatchRefusesANilCommand(t *testing.T) {
	b := &backend{started: true}
	if b.dispatch(nil) {
		t.Fatal("dispatch accepted a nil command")
	}
}

// TestSetPositionPassesTheNoTrackSentinelThrough is the boundary of the stale
// guard: the NoTrack id is a valid current value when nothing is published, so
// it is compared rather than special-cased.
func TestSetPositionPassesTheNoTrackSentinelThrough(t *testing.T) {
	b := &backend{started: true, trackID: noTrack}
	v := playerView{b}

	// NoTrack matches NoTrack, so the (nil) command is dispatched harmlessly.
	if err := v.SetPosition(noTrack, 1_000_000); err != nil {
		t.Fatalf("SetPosition(noTrack) returned %v", err)
	}

	var _ dbus.ObjectPath = noTrack
}
