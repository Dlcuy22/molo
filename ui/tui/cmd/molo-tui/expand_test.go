package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// ext returns the extensions the test treats as playable. The real list comes
// from the facade; these tests only care about the directory rules, so they
// pass an explicit set and never depend on which codecs are registered.
func testExts() map[string]bool {
	return map[string]bool{".flac": true, ".mp3": true, ".opus": true}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExpandArgsFilesPassThrough proves a plain file argument is returned as
// given, in the order it was written: an existing queue must not be reordered
// by the folder support.
func TestExpandArgsFilesPassThrough(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.flac")
	b := filepath.Join(dir, "b.mp3")
	writeFile(t, a, "x")
	writeFile(t, b, "x")

	got, err := expandArgs([]string{b, a}, testExts())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{b, a}) {
		t.Fatalf("expandArgs = %v, want the two files in order", got)
	}
}

// TestExpandArgsDirectoryIsScanned is the feature: a directory becomes the
// playable files directly inside it, sorted so a run is reproducible.
func TestExpandArgsDirectoryIsScanned(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "c.flac"), "x")
	writeFile(t, filepath.Join(dir, "a.mp3"), "x")
	writeFile(t, filepath.Join(dir, "b.opus"), "x")

	got, err := expandArgs([]string{dir}, testExts())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "a.mp3"),
		filepath.Join(dir, "b.opus"),
		filepath.Join(dir, "c.flac"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expandArgs = %v, want %v", got, want)
	}
}

// TestExpandArgsDirectoryFindsNestedFiles pins the recursive behaviour: a
// music library is usually album folders under one root, and a user naming the
// root expects the albums to be found.
func TestExpandArgsDirectoryFindsNestedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "album one", "01.flac"), "x")
	writeFile(t, filepath.Join(root, "album two", "01.mp3"), "x")
	writeFile(t, filepath.Join(root, "album two", "02.opus"), "x")

	got, err := expandArgs([]string{root}, testExts())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expandArgs found %d files, want 3: %v", len(got), got)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("nested results are not sorted: %v", got)
	}
}

// TestExpandArgsSkipsUnsupportedFiles proves a stray file in the folder is not
// queued: only extensions the engine can decode are collected.
func TestExpandArgsSkipsUnsupportedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "song.flac"), "x")
	writeFile(t, filepath.Join(dir, "cover.jpg"), "x")
	writeFile(t, filepath.Join(dir, "notes.txt"), "x")
	writeFile(t, filepath.Join(dir, "playlist.m3u"), "x")

	got, err := expandArgs([]string{dir}, testExts())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "song.flac" {
		t.Fatalf("expandArgs = %v, want only song.flac", got)
	}
}

// TestExpandArgsMixedKeepsOrderAndExpands covers the real invocation: files and
// folders interleaved. Explicitly named files keep their place; a folder
// expands where it appears.
func TestExpandArgsMixedKeepsOrderAndExpands(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.flac")
	writeFile(t, first, "x")

	album := filepath.Join(dir, "album")
	writeFile(t, filepath.Join(album, "a.mp3"), "x")
	writeFile(t, filepath.Join(album, "b.mp3"), "x")

	last := filepath.Join(dir, "last.opus")
	writeFile(t, last, "x")

	got, err := expandArgs([]string{first, album, last}, testExts())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		first,
		filepath.Join(album, "a.mp3"),
		filepath.Join(album, "b.mp3"),
		last,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expandArgs = %v, want %v", got, want)
	}
}

// TestExpandArgsEmptyFolderIsAnError guards the confusing case: naming a folder
// with nothing playable in it must say so, not start an empty player.
func TestExpandArgsEmptyFolderIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "readme.txt"), "x")

	if _, err := expandArgs([]string{dir}, testExts()); err == nil {
		t.Fatal("an empty folder returned no error")
	}
}

// TestExpandArgsMissingPathIsAnError covers a typo, which must fail loudly
// rather than silently drop the argument.
func TestExpandArgsMissingPathIsAnError(t *testing.T) {
	if _, err := expandArgs([]string{filepath.Join(t.TempDir(), "nope.flac")}, testExts()); err == nil {
		t.Fatal("a missing path returned no error")
	}
}

// TestExpandArgsDeduplicates proves the same file named twice (once directly,
// once through its folder) appears once, so the queue has no repeat.
func TestExpandArgsDeduplicates(t *testing.T) {
	dir := t.TempDir()
	song := filepath.Join(dir, "song.flac")
	writeFile(t, song, "x")

	got, err := expandArgs([]string{song, dir}, testExts())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != song {
		t.Fatalf("expandArgs = %v, want just %s", got, song)
	}
}
