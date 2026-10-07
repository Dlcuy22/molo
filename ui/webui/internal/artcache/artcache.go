// Package artcache keeps fetched artwork bytes on disk so a track revisited
// later does not refetch them. Art is decorative, so every operation is best
// effort: a cache miss or a write failure is a reason to fetch again, never a
// reason to fail the feature.
//
// The location follows the platform convention: the XDG cache directory on
// Linux ($XDG_CACHE_HOME/molo/art, defaulting to ~/.cache/molo/art) and the
// temporary directory on Windows.
package artcache

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// subdir is the folder under the platform cache root. One name for both the
// XDG path and the temp fallback keeps a stale cache easy to find and delete.
const subdir = "molo-art"

// Dir returns the directory the cache lives in, creating it on first use. On
// Linux it is the XDG cache directory ($XDG_CACHE_HOME/molo/art, defaulting to
// ~/.cache/molo/art); on Windows it is the temporary directory, as requested.
func Dir() (string, error) {
	dir := fallbackDir()
	if runtime.GOOS != "windows" {
		if base, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(base, "molo", "art")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	return dir, nil
}

// fallbackDir is where the cache goes when the platform convention is not
// available. It is the same directory Windows always uses. os.TempDir is
// documented never to return empty, but the empty guard keeps a relative path
// from ever reaching the filesystem if that contract is broken.
func fallbackDir() string {
	base := os.TempDir()
	if base == "" {
		base = "."
	}

	return filepath.Join(base, subdir)
}

// Key is a stable id for one blob of bytes, so the same artwork maps to one
// cache entry wherever it is keyed.
func Key(data []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(data)

	return strconv.FormatUint(h.Sum64(), 16)
}

// path returns the file one key maps to. The key is hashed rather than used
// directly, so an artist id or a URL cannot leak a path separator or a reserved
// name into the filesystem.
func path(key string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, keyName(key)+".img"), nil
}

// keyName hashes a key into a filesystem-safe file stem.
func keyName(key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))

	return strconv.FormatUint(h.Sum64(), 16)
}

// Load returns the cached bytes for key, or nil when the key is empty or the
// entry is absent or unreadable.
func Load(key string) []byte {
	if key == "" {
		return nil
	}
	p, err := path(key)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}

	return data
}

// Store writes data under key. It is best effort and returns the error only so
// a caller can log it; a cache write that fails changes nothing about the track.
// The write goes to a temporary file first so a reader never sees a partial
// image.
func Store(key string, data []byte) error {
	if key == "" || len(data) == 0 {
		return nil
	}
	p, err := path(key)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}

	return os.Rename(tmp, p)
}
