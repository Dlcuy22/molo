package main

import (
	"context"
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

// TestPresenceTrackTrimsTopicSuffix pins the yt-dlp fix: an auto-generated
// "- Topic" channel name is trimmed from both the artist line and its tooltip.
func TestPresenceTrackTrimsTopicSuffix(t *testing.T) {
	snap := Snapshot{
		Path:   "/music/Doppelganger - kessoku band.opus",
		Title:  "Doppelganger",
		Artist: "kessoku band - Topic",
		Album:  "Doppelganger / Re:Re:",
	}

	got := presenceTrack(snap, "", "")

	if got.Artist != "kessoku band" {
		t.Fatalf("Artist = %q, want the suffix trimmed", got.Artist)
	}
	if got.SmallText != "kessoku band" {
		t.Fatalf("SmallText = %q, want the suffix trimmed", got.SmallText)
	}
}

// TestTrimArtistTopic pins the suffix rule across the shapes it must handle.
func TestTrimArtistTopic(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"kessoku band - Topic", "kessoku band"},
		{"kessoku band - topic", "kessoku band"},
		{"  kessoku band - Topic  ", "kessoku band"},
		{"kessoku band", "kessoku band"},
		{"Eve", "Eve"},
		// A real name that merely contains the word is left alone.
		{"Topic", "Topic"},
		{"A Topic Band", "A Topic Band"},
		// A name that is only the suffix would trim to empty, so it is kept.
		{"- Topic", "- Topic"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := trimArtistTopic(tc.in); got != tc.want {
			t.Errorf("trimArtistTopic(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAlbumForCachesTheLookup pins the album resolver: it calls the watch
// endpoint once, caches the name, and does not call again.
func TestAlbumForCachesTheLookup(t *testing.T) {
	index := newYTMIndex()
	index.put(ytmTrack{ID: "abc123", Title: "A Song", Artist: "An Artist"})

	calls := 0
	p := &ytmProvider{
		index: index,
		loadSong: func(_ context.Context, _ string) (*ytm.Song, error) {
			calls++

			return &ytm.Song{ID: "abc123", Album: &ytm.Playlist{Name: "An Album"}}, nil
		},
	}

	if got := p.albumFor(context.Background(), "ytm:abc123"); got != "An Album" {
		t.Fatalf("albumFor = %q, want An Album", got)
	}
	if got := p.albumFor(context.Background(), "ytm:abc123"); got != "An Album" {
		t.Fatalf("second albumFor = %q, want An Album", got)
	}
	if calls != 1 {
		t.Fatalf("loadSong calls = %d, want 1 (cached)", calls)
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
