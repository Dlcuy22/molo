package meta

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedMatchesBySupportedExtension(t *testing.T) {
	e := NewEmbeddedTags()
	supported := []string{"a.opus", "a.ogg", "a.oga", "a.flac", "a.mp3", "a.m4a", "a.m4b", "a.m4p", "a.dsf", "a.OPUS"}
	for _, path := range supported {
		if !e.Match(path) {
			t.Fatalf("Match(%q) = false, want true", path)
		}
	}
	unsupported := []string{"a.wav", "a.aiff", "a.txt", "noextension", "dir.opus/child"}
	for _, path := range unsupported {
		if e.Match(path) {
			t.Fatalf("Match(%q) = true, want false", path)
		}
	}
}

func TestEmbeddedReadsTagsAndCoverArt(t *testing.T) {
	m, err := NewEmbeddedTags().Resolve(context.Background(), fixturePath(t, "tagged.opus"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := Tags{
		Title:       "Tagged Title",
		Artist:      "Tag Artist",
		Album:       "Tag Album",
		AlbumArtist: "Tag Album Artist",
		Track:       3,
		Disc:        1,
		Year:        2021,
	}
	if m.Tags.Title != want.Title || m.Tags.Artist != want.Artist || m.Tags.Album != want.Album {
		t.Fatalf("tags = %+v, want title/artist/album from the file", m.Tags)
	}
	if m.Tags.AlbumArtist != want.AlbumArtist {
		t.Fatalf("AlbumArtist = %q, want %q", m.Tags.AlbumArtist, want.AlbumArtist)
	}
	if m.Tags.Track != want.Track || m.Tags.Disc != want.Disc || m.Tags.Year != want.Year {
		t.Fatalf("numeric tags = track %d disc %d year %d, want %d/%d/%d",
			m.Tags.Track, m.Tags.Disc, m.Tags.Year, want.Track, want.Disc, want.Year)
	}
	if m.Container != "ogg" {
		t.Fatalf("Container = %q, want %q", m.Container, "ogg")
	}
	if m.Codec != "opus" {
		t.Fatalf("Codec = %q, want %q", m.Codec, "opus")
	}
	if m.Source != "embedded-tags" {
		t.Fatalf("Source = %q, want %q", m.Source, "embedded-tags")
	}
	if m.Tags.CoverMIME != "image/png" {
		t.Fatalf("CoverMIME = %q, want %q", m.Tags.CoverMIME, "image/png")
	}

	cover, err := os.ReadFile(fixturePath(t, "cover.png"))
	if err != nil {
		t.Fatalf("read expected cover: %v", err)
	}
	if !bytes.Equal(m.Tags.Cover, cover) {
		t.Fatalf("Cover is %d bytes, want the %d byte PNG embedded in the fixture", len(m.Tags.Cover), len(cover))
	}
}

func TestEmbeddedTreatsAnUntaggedFileAsNoMatch(t *testing.T) {
	// The resolver must step aside rather than answer with an empty title, so
	// the chain can still derive one from the filename.
	_, err := NewEmbeddedTags().Resolve(context.Background(), filepath.Join("..", "decode", "testdata", "stereo_2s.opus"))
	if !errors.Is(err, ErrNoTags) {
		t.Fatalf("Resolve error = %v, want ErrNoTags", err)
	}
}

func TestEmbeddedFailsOnCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.opus")
	if err := os.WriteFile(path, []byte("this is not an Ogg Opus stream at all"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	if _, err := NewEmbeddedTags().Resolve(context.Background(), path); err == nil {
		t.Fatal("Resolve on a corrupt file returned nil error")
	}
}

func TestEmbeddedFailsOnMissingFile(t *testing.T) {
	_, err := NewEmbeddedTags().Resolve(context.Background(), filepath.Join(t.TempDir(), "missing.opus"))
	if err == nil {
		t.Fatal("Resolve on a missing file returned nil error")
	}
}

func TestEmbeddedHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewEmbeddedTags().Resolve(ctx, fixturePath(t, "tagged.opus")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve error = %v, want context.Canceled", err)
	}
}

func TestEmbeddedPriorityBeatsFilename(t *testing.T) {
	m, err := Default().Resolve(context.Background(), fixturePath(t, "tagged.opus"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Source != "embedded-tags" {
		t.Fatalf("Source = %q, want embedded-tags", m.Source)
	}
}

func TestEmbeddedSatisfiesResolver(t *testing.T) {
	var _ Resolver = (*EmbeddedTags)(nil)
}
