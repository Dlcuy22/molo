package analysis

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain points the user cache at a scratch directory for the whole package,
// so no test can write to the developer's real cache even when it leaves
// Options.CacheDir empty. Individual tests that exercise the default path
// override XDG_CACHE_HOME again with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "analysis-cache-")
	if err != nil {
		panic("analysis tests: create scratch cache: " + err.Error())
	}
	_ = os.Setenv("XDG_CACHE_HOME", dir)

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// tempPathIn creates a file inside dir/src so a test can point both the source
// and the cache at the same scratch directory without the source being mistaken
// for a cache entry.
func tempPathIn(t *testing.T, dir string) string {
	t.Helper()

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatalf("create source dir: %v", err)
	}
	path := filepath.Join(src, "synthetic.opus")
	if err := os.WriteFile(path, []byte("not real audio"), 0o600); err != nil {
		t.Fatalf("write synthetic file: %v", err)
	}

	return path
}

// cacheFiles lists the entries in a test cache directory, treating a missing
// directory as empty.
func cacheFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read cache dir: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, filepath.Join(dir, e.Name()))
		}
	}

	return names
}

func runtimeGoroutines() int { return runtime.NumGoroutine() }
