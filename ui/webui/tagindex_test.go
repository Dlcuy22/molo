package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTagIndexResolvesAndCaches pins the index contract: a queued track is
// resolved once, the result is cached, and a second reconcile does not open it
// again.
func TestTagIndexResolvesAndCaches(t *testing.T) {
	svc := newPlayerService()
	defer svc.cancel()

	// A tagged fixture from the engine's own meta testdata, so the test proves
	// the palette reads real embedded tags rather than a filename guess.
	tagged := filepath.Join("..", "..", "meta", "testdata", "tagged.opus")
	svc.indexQueue([]string{tagged})

	got := waitForTags(t, svc, tagged)
	if got.Title != "Tagged Title" || got.Artist != "Tag Artist" || got.Album != "Tag Album" {
		t.Fatalf("indexed tags = %+v, want the fixture's embedded tags", got)
	}
}

// TestTagIndexFallsBackToFilename is the fallback contract: an untagged file
// still gets a searchable title, taken from its name by the meta chain.
func TestTagIndexFallsBackToFilename(t *testing.T) {
	svc := newPlayerService()
	defer svc.cancel()

	path := filepath.Join(t.TempDir(), "No Tags Here.flac")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.indexQueue([]string{path})

	got := waitForTags(t, svc, path)
	if got.Title != "No Tags Here" {
		t.Fatalf("title = %q, want the file name without the extension", got.Title)
	}
}

// TestTagIndexDropsDepartedTracks proves a replaced queue does not leave stale
// entries the palette could still jump to.
func TestTagIndexDropsDepartedTracks(t *testing.T) {
	svc := newPlayerService()
	defer svc.cancel()

	dir := t.TempDir()
	a := filepath.Join(dir, "a.flac")
	b := filepath.Join(dir, "b.flac")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	svc.indexQueue([]string{a, b})
	waitForTags(t, svc, a)
	waitForTags(t, svc, b)

	svc.indexQueue([]string{a})
	if svc.index.resolved(b) {
		t.Fatalf("departed track %q still indexed", b)
	}
	if !svc.index.resolved(a) {
		t.Fatalf("kept track %q was dropped", a)
	}
}

// TestQueueRowsCarryResolvedTags pins the payload the palette searches: the
// tags ride on the row, and an unresolved row falls back to its name.
func TestQueueRowsCarryResolvedTags(t *testing.T) {
	svc := newPlayerService()
	defer svc.cancel()

	tagged := filepath.Join("..", "..", "meta", "testdata", "tagged.opus")
	svc.indexQueue([]string{tagged})
	waitForTags(t, svc, tagged)

	rows := svc.queueRows([]string{tagged}, 0)
	if rows[0].Title != "Tagged Title" || rows[0].Artist != "Tag Artist" {
		t.Fatalf("row tags = %+v, want the resolved tags", rows[0])
	}
	if rows[0].Name != "tagged.opus" {
		t.Fatalf("row name = %q, want the file name", rows[0].Name)
	}
	if rows[0].CoverID == "" {
		t.Fatal("row has no cover id, want the id of the fixture's embedded art")
	}
}

// TestQueueCoverReturnsEmbeddedArt checks the palette's lazy fetch: a queued
// track's art is served as a data URL, and a track with none answers empty.
func TestQueueCoverReturnsEmbeddedArt(t *testing.T) {
	svc := newPlayerService()
	defer svc.cancel()

	tagged := filepath.Join("..", "..", "meta", "testdata", "tagged.opus")
	svc.indexQueue([]string{tagged})
	waitForTags(t, svc, tagged)

	url := svc.QueueCover(tagged)
	// The cover package re-encodes opaque art as JPEG, so the fixture's PNG
	// comes back under a JPEG mime even though its source was PNG.
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("QueueCover = %q, want the fixture's art as a data URL", url[:min(len(url), 40)])
	}

	// The fixture's art is cached under its content id, so a second ask is a
	// cache hit and returns the identical value.
	if again := svc.QueueCover(tagged); again != url {
		t.Fatalf("second QueueCover differs: %q vs %q", again[:min(len(again), 40)], url[:min(len(url), 40)])
	}

	// An untagged file has no art, so the answer is empty rather than an error.
	untagged := filepath.Join("..", "..", "decode", "testdata", "stereo_2s.opus")
	svc.indexQueue([]string{untagged})
	waitForTags(t, svc, untagged)
	if got := svc.QueueCover(untagged); got != "" {
		t.Fatalf("QueueCover on an artless track = %q, want empty", got)
	}
}

// waitForTags polls the index until the path resolves. Resolution is
// asynchronous by design, so the test waits rather than reaching into the
// worker.
func waitForTags(t *testing.T, svc *PlayerService, path string) trackTags {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if svc.index.resolved(path) {
			return svc.index.lookup(path)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tags for %q did not resolve in time", path)

	return trackTags{}
}
