package meta

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFilenameAlwaysMatches(t *testing.T) {
	f := NewFilename()
	for _, path := range []string{
		"a.opus",
		"/no/such/dir/anything.xyz",
		"",
		"file",
	} {
		if !f.Match(path) {
			t.Fatalf("Match(%q) = false; the filename resolver is the last resort and must always match", path)
		}
	}
}

func TestFilenameDerivesTitleFromTheBaseName(t *testing.T) {
	f := NewFilename()
	m, err := f.Resolve(context.Background(), filepath.Join("/music", "Some Song.opus"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Tags.Title != "Some Song" {
		t.Fatalf("Title = %q, want %q", m.Tags.Title, "Some Song")
	}
	if m.Path != filepath.Join("/music", "Some Song.opus") {
		t.Fatalf("Path = %q, want the input path preserved", m.Path)
	}
	if m.Source != "filename" {
		t.Fatalf("Source = %q, want %q", m.Source, "filename")
	}
}

func TestFilenameKeepsDotfulNamesIntact(t *testing.T) {
	m, err := NewFilename().Resolve(context.Background(), "artist - title.flac")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Tags.Title != "artist - title" {
		t.Fatalf("Title = %q, want %q", m.Tags.Title, "artist - title")
	}
}

func TestFilenameHandlesNoExtension(t *testing.T) {
	m, err := NewFilename().Resolve(context.Background(), "/music/untitled")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Tags.Title != "untitled" {
		t.Fatalf("Title = %q, want %q", m.Tags.Title, "untitled")
	}
}

func TestFilenameDerivesContainerFromExtension(t *testing.T) {
	m, err := NewFilename().Resolve(context.Background(), "song.opus")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Container != "ogg" {
		t.Fatalf("Container = %q, want %q", m.Container, "ogg")
	}
}

func TestFilenameIsLowestPriority(t *testing.T) {
	if NewFilename().Priority() >= NewEmbeddedTags().Priority() {
		t.Fatal("the fallback resolver must have a lower priority than the embedded-tag resolver")
	}
}

func TestFilenameSatisfiesResolver(t *testing.T) {
	var _ Resolver = (*Filename)(nil)
}

func TestContainerForExtensionIsShared(t *testing.T) {
	// Both resolvers must agree on what extension means, so a tagless file and
	// a tagged one do not disagree about their container.
	cases := map[string]string{
		".opus": "ogg",
		".ogg":  "ogg",
		".oga":  "ogg",
		".flac": "flac",
		".mp3":  "mp3",
		".m4a":  "mp4",
		".dsf":  "dsf",
		".m4A":  "mp4",
	}
	for ext, want := range cases {
		if got := containerForExt(ext); got != want {
			t.Fatalf("containerForExt(%q) = %q, want %q", ext, got, want)
		}
	}
}
