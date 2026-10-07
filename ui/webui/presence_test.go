package main

import (
	"testing"

	ytm "github.com/dlcuy22/ytm-go"
)

// TestPresenceTrackRemoteCarriesBothButtons pins the remote mapping: the title
// is the details line, the artist the state line, the album is the cover
// tooltip, and the two links point at the track and the project.
func TestPresenceTrackRemoteCarriesBothButtons(t *testing.T) {
	snap := Snapshot{
		Path:     "ytm:abc123",
		Title:    "A Song",
		Artist:   "An Artist",
		Album:    "An Album",
		Duration: 200000,
	}

	got := presenceTrack(snap, "https://cover", "https://avatar")

	if got.Title != "A Song" || got.Artist != "An Artist" {
		t.Fatalf("title/artist = %q/%q, want the track's", got.Title, got.Artist)
	}
	if got.LargeImage != "https://cover" || got.SmallImage != "https://avatar" {
		t.Fatalf("images = %q/%q, want the supplied urls", got.LargeImage, got.SmallImage)
	}
	if got.LargeText != "An Album" || got.SmallText != "An Artist" {
		t.Fatalf("tooltips = %q/%q, want album/artist", got.LargeText, got.SmallText)
	}
	if got.DurationMs != 200000 {
		t.Fatalf("DurationMs = %d, want 200000", got.DurationMs)
	}
	if got.ActivityType != 2 {
		t.Fatalf("ActivityType = %d, want 2 (Listening)", got.ActivityType)
	}
	if len(got.Buttons) != 2 {
		t.Fatalf("buttons = %d, want 2", len(got.Buttons))
	}
	if got.Buttons[0].URL != "https://music.youtube.com/watch?v=abc123" {
		t.Fatalf("listen url = %q", got.Buttons[0].URL)
	}
}

// TestPresenceTrackLocalHasNoListenButton pins the local case: a file has no
// watch page, so only the project link is offered.
func TestPresenceTrackLocalHasNoListenButton(t *testing.T) {
	snap := Snapshot{Path: "/music/a.opus", Title: "Local", Artist: "Me"}

	got := presenceTrack(snap, "", "")

	if len(got.Buttons) != 1 {
		t.Fatalf("buttons = %d, want 1", len(got.Buttons))
	}
	if got.Buttons[0].URL != projectURL {
		t.Fatalf("button url = %q, want the project url", got.Buttons[0].URL)
	}
}

// TestYtmFirstArtistIDSkipsBlanks pins the avatar key: the first credit with an
// id wins, so an unnamed credit before it does not blank the lookup.
func TestYtmFirstArtistIDSkipsBlanks(t *testing.T) {
	if got := ytmFirstArtistID(nil); got != "" {
		t.Fatalf("empty = %q, want \"\"", got)
	}

	artists := []ytm.Artist{{Name: "No Id"}, {ID: "UC123", Name: "Has Id"}}
	if got := ytmFirstArtistID(artists); got != "UC123" {
		t.Fatalf("first id = %q, want UC123", got)
	}
}

// TestDiscordEnabledRoundTrips pins the persistence: the choice survives a
// restart, and a fresh install defaults to off.
func TestDiscordEnabledRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if loadDiscordEnabled() {
		t.Fatal("default should be off")
	}
	if err := saveDiscordEnabled(true); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !loadDiscordEnabled() {
		t.Fatal("enabled state did not persist")
	}
	if err := saveDiscordEnabled(false); err != nil {
		t.Fatalf("save off: %v", err)
	}
	if loadDiscordEnabled() {
		t.Fatal("disabled state did not persist")
	}
}
