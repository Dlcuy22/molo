package analysis

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
)

func TestWaveformCacheHitSkipsDecode(t *testing.T) {
	dir := t.TempDir()
	path := tempPathIn(t, dir)
	info := monoInfo(8)
	data := []float32{0, 1, 2, 3, 4, 5, 6, 7}
	opens := stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: info, data: data}
	})

	first, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir})
	if err != nil {
		t.Fatalf("first Waveform: %v", err)
	}
	if *opens != 1 {
		t.Fatalf("opens after miss = %d, want 1", *opens)
	}

	second, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir})
	if err != nil {
		t.Fatalf("second Waveform: %v", err)
	}
	if *opens != 1 {
		t.Fatalf("opens after hit = %d, want 1", *opens)
	}
	if len(second.Buckets) != len(first.Buckets) || second.Frames != first.Frames {
		t.Fatalf("hit result %+v does not match miss result %+v", second, first)
	}
	for i := range first.Buckets {
		if second.Buckets[i] != first.Buckets[i] {
			t.Fatalf("bucket %d = %+v, want %+v", i, second.Buckets[i], first.Buckets[i])
		}
	}
}

func TestWaveformCacheBypassReDecodes(t *testing.T) {
	dir := t.TempDir()
	path := tempPathIn(t, dir)
	opens := stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(8), data: []float32{0, 1, 2, 3, 4, 5, 6, 7}}
	})

	if _, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir}); err != nil {
		t.Fatalf("first Waveform: %v", err)
	}

	// A bypass must still not write: it is a read-through skip, not a refresh.
	if _, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir, NoCache: true}); err != nil {
		t.Fatalf("second Waveform: %v", err)
	}
	if *opens != 2 {
		t.Fatalf("opens = %d, want 2 (bypass must decode again)", *opens)
	}
}

func TestWaveformCacheStaleWhenFileChanges(t *testing.T) {
	dir := t.TempDir()
	path := tempPathIn(t, dir)
	opens := stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(8), data: []float32{0, 1, 2, 3, 4, 5, 6, 7}}
	})

	if _, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir}); err != nil {
		t.Fatalf("first Waveform: %v", err)
	}

	// A different size and mtime must invalidate the entry. Sleeping past the
	// filesystem's timestamp resolution keeps mtime distinct.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte("a different, longer payload"), 0o600); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}

	if _, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir}); err != nil {
		t.Fatalf("second Waveform: %v", err)
	}
	if *opens != 2 {
		t.Fatalf("opens = %d, want 2 (changed file must miss)", *opens)
	}
}

func TestWaveformFailureIsNotCached(t *testing.T) {
	dir := t.TempDir()
	path := tempPathIn(t, dir)
	boom := errors.New("decode exploded")
	old := openDecoder
	openDecoder = func(string) (decode.Decoder, error) {
		return &streamDecoder{info: monoInfo(4), data: []float32{1, 2, 3, 4}, readErr: boom}, nil
	}
	t.Cleanup(func() { openDecoder = old })

	if _, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	if files := cacheFiles(t, dir); len(files) != 0 {
		t.Fatalf("failed run wrote cache files: %v", files)
	}
}

func TestWaveformCacheCorruptEntryFallsBackToDecode(t *testing.T) {
	dir := t.TempDir()
	path := tempPathIn(t, dir)
	opens := stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(8), data: []float32{0, 1, 2, 3, 4, 5, 6, 7}}
	})

	if _, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir}); err != nil {
		t.Fatalf("first Waveform: %v", err)
	}

	files := cacheFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("cache files = %v, want exactly one", files)
	}
	if err := os.WriteFile(files[0], []byte("garbage that is not a cache entry"), 0o600); err != nil {
		t.Fatalf("corrupt cache: %v", err)
	}

	if _, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir}); err != nil {
		t.Fatalf("second Waveform: %v", err)
	}
	if *opens != 2 {
		t.Fatalf("opens = %d, want 2 (corrupt entry must be re-decoded)", *opens)
	}
}

func TestWaveformCacheUsesUserCacheDirByDefault(t *testing.T) {
	// os.UserCacheDir honours XDG_CACHE_HOME on Linux, so this proves both that
	// the default location is the user cache and that it is overridable so a
	// test never writes to the real one.
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(8), data: []float32{0, 1, 2, 3, 4, 5, 6, 7}}
	})

	path := tempPathIn(t, t.TempDir())
	if _, err := Waveform(context.Background(), path, Options{Buckets: 2}); err != nil {
		t.Fatalf("Waveform: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(base, "player", "waveform"))
	if err != nil {
		t.Fatalf("default cache did not land under the user cache dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("default cache wrote no entry")
	}
}

func TestWaveformExplicitCacheDirLeavesUserCacheAlone(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(8), data: []float32{0, 1, 2, 3, 4, 5, 6, 7}}
	})

	dir := t.TempDir()
	path := tempPathIn(t, dir)
	if _, err := Waveform(context.Background(), path, Options{Buckets: 2, CacheDir: dir}); err != nil {
		t.Fatalf("Waveform: %v", err)
	}

	// The explicit dir got the entry; the user cache was never touched.
	if files := cacheFiles(t, dir); len(files) != 1 {
		t.Fatalf("explicit cache files = %v, want one", files)
	}
	if _, err := os.Stat(filepath.Join(base, "player")); !os.IsNotExist(err) {
		t.Fatalf("an explicit CacheDir still touched the user cache (stat err = %v)", err)
	}
}

func TestWaveformMissingFileFailsBeforeAnyCacheWrite(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)

	path := filepath.Join(t.TempDir(), "absent.opus")
	if _, err := Waveform(context.Background(), path, Options{Buckets: 2}); err == nil {
		t.Fatal("Waveform on a missing file returned nil error")
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read overridden cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed run wrote into the user cache: %v", entries)
	}
}
