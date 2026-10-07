package artcache

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestStoreLoadRoundTrip proves a stored blob reads back byte for byte, which
// is the only thing the cache promises.
func TestStoreLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	data := []byte("not really a png, but the cache does not care")
	if err := Store("artist:abc", data); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got := Load("artist:abc")
	if !bytes.Equal(got, data) {
		t.Fatalf("Load = %q, want %q", got, data)
	}
}

// TestLoadMissingIsNil pins the miss contract: an absent key is nil, not an
// error, so a caller reads "no cached art" and fetches again.
func TestLoadMissingIsNil(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if got := Load("artist:nope"); got != nil {
		t.Fatalf("Load(missing) = %q, want nil", got)
	}
	if got := Load(""); got != nil {
		t.Fatalf("Load(empty) = %q, want nil", got)
	}
}

// TestDirFollowsXDGCacheOnLinux pins the platform rule the task asks for: the
// cache lives under the XDG cache root, not the temp directory.
func TestDirFollowsXDGCacheOnLinux(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	want := filepath.Join(root, "molo", "art")
	if dir != want {
		t.Fatalf("Dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("Dir did not create the directory: %v", err)
	}
}

// TestKeyIsStableAndDistinct pins the hash contract: the same bytes map to one
// key, different bytes to different keys.
func TestKeyIsStableAndDistinct(t *testing.T) {
	a := Key([]byte("one"))
	b := Key([]byte("one"))
	c := Key([]byte("two"))
	if a != b {
		t.Fatalf("Key is not stable: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("Key collided for distinct inputs: %q", a)
	}
}
