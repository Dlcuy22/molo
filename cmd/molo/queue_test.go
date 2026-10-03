package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// tree writes files under root, creating parent directories as needed, and
// returns root. Paths are relative to root.
func tree(t *testing.T, files ...string) string {
	t.Helper()

	root := t.TempDir()
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	return root
}

func TestResolveQueueKeepsFileArgumentsInOrder(t *testing.T) {
	root := tree(t, "b.opus", "a.opus")

	got, err := resolveQueue([]string{filepath.Join(root, "b.opus"), filepath.Join(root, "a.opus")}, []string{".opus"})
	if err != nil {
		t.Fatalf("resolveQueue: %v", err)
	}

	want := []string{filepath.Join(root, "b.opus"), filepath.Join(root, "a.opus")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveQueue = %q, want %q", got, want)
	}
}

func TestResolveQueueIncludesFilesWithUnknownExtensions(t *testing.T) {
	root := tree(t, "notes.txt")

	got, err := resolveQueue([]string{filepath.Join(root, "notes.txt")}, []string{".opus"})
	if err != nil {
		t.Fatalf("resolveQueue: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("explicit file argument was filtered out: %q", got)
	}
}

func TestResolveQueueExpandsDirectoryRecursivelyAndSorted(t *testing.T) {
	root := tree(t,
		"z.opus",
		"a.opus",
		"sub/c.opus",
		"sub/deep/d.opus",
		"skip.txt",
	)

	got, err := resolveQueue([]string{root}, []string{".opus"})
	if err != nil {
		t.Fatalf("resolveQueue: %v", err)
	}

	want := []string{
		filepath.Join(root, "a.opus"),
		filepath.Join(root, "sub", "c.opus"),
		filepath.Join(root, "sub", "deep", "d.opus"),
		filepath.Join(root, "z.opus"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveQueue = %q, want %q", got, want)
	}
}

func TestResolveQueueMatchesExtensionsCaseInsensitively(t *testing.T) {
	root := tree(t, "a.OPUS", "b.Opus")

	got, err := resolveQueue([]string{root}, []string{".opus"})
	if err != nil {
		t.Fatalf("resolveQueue: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("case-insensitive extension match failed: %q", got)
	}
}

func TestResolveQueueDeduplicatesPreservingFirstOccurrence(t *testing.T) {
	root := tree(t, "a.opus", "b.opus")

	a := filepath.Join(root, "a.opus")
	b := filepath.Join(root, "b.opus")

	// a is named explicitly and also lives in the directory; b appears twice.
	got, err := resolveQueue([]string{a, b, root, a}, []string{".opus"})
	if err != nil {
		t.Fatalf("resolveQueue: %v", err)
	}

	want := []string{a, b}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveQueue = %q, want %q", got, want)
	}
}

func TestResolveQueueErrorsOnMissingPath(t *testing.T) {
	if _, err := resolveQueue([]string{filepath.Join(t.TempDir(), "nope.opus")}, []string{".opus"}); err == nil {
		t.Fatal("resolveQueue accepted a missing path")
	}
}

func TestResolveQueueEmptyDirectoryYieldsNothing(t *testing.T) {
	root := tree(t, "notes.txt")

	got, err := resolveQueue([]string{root}, []string{".opus"})
	if err != nil {
		t.Fatalf("resolveQueue: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected tracks: %q", got)
	}
}

// TestResolveQueueReturnsNoErrorForAnEmptyArgumentList keeps the "no input"
// decision in run, where it maps to exit code 3, rather than turning it into a
// usage error here.
func TestResolveQueueReturnsNoErrorForAnEmptyArgumentList(t *testing.T) {
	got, err := resolveQueue(nil, []string{".opus"})
	if err != nil {
		t.Fatalf("resolveQueue(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("resolveQueue(nil) = %q, want empty", got)
	}
}

func TestResolveQueueIgnoresSymlinkedDirectories(t *testing.T) {
	root := tree(t, "real/a.opus")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := resolveQueue([]string{root}, []string{".opus"})
	if err != nil {
		t.Fatalf("resolveQueue: %v", err)
	}

	// filepath.WalkDir does not follow directory symlinks, so the track is
	// listed once, under its real path.
	want := []string{filepath.Join(root, "real", "a.opus")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveQueue = %q, want %q", got, want)
	}
}
