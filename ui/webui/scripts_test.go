package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dlcuy22/player/script"
)

// The bundled effects are what the effect window shows on a fresh install, so
// they must exist and must parse. A build that drops them, or a script that
// stops compiling against the engine, would otherwise only surface at runtime.
func TestBundledEffectsParse(t *testing.T) {
	entries, err := fs.ReadDir(bundledEffects, "effects")
	if err != nil {
		t.Fatalf("read embedded effects: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no bundled effects embedded")
	}

	dir := t.TempDir()
	for _, e := range entries {
		data, err := bundledEffects.ReadFile("effects/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
	}

	factories, err := script.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(factories) != len(entries) {
		t.Fatalf("LoadDir returned %d factories for %d files", len(factories), len(entries))
	}

	names := make([]string, 0, len(factories))
	for _, f := range factories {
		if f.Kind() != "script" {
			t.Errorf("factory kind = %q, want script", f.Kind())
		}
		if f.Impl() == "" {
			t.Error("factory has an empty impl name")
		}
		if len(f.Schema()) == 0 {
			t.Errorf("factory %q has no schema", f.Impl())
		}
		names = append(names, f.Impl())
	}
	sort.Strings(names)
	t.Logf("bundled effects: %v", names)
}

// The bundled effects are copies of the engine's testdata scripts. They are
// duplicated because the webui module embeds its own set, so a script edited in
// one place can silently drift from the other. This pins them byte-for-byte:
// the two copies must stay identical, which is what keeps the shipped UI's
// controls (a widget hint, a param range) from disagreeing with the script the
// engine tests against.
func TestBundledEffectsMatchEngineTestdata(t *testing.T) {
	entries, err := fs.ReadDir(bundledEffects, "effects")
	if err != nil {
		t.Fatalf("read embedded effects: %v", err)
	}

	for _, e := range entries {
		bundled, err := bundledEffects.ReadFile("effects/" + e.Name())
		if err != nil {
			t.Fatalf("read bundled %s: %v", e.Name(), err)
		}
		source, err := os.ReadFile(filepath.Join("..", "..", "script", "testdata", e.Name()))
		if err != nil {
			// A bundled script with no engine counterpart is allowed; only a
			// drifted copy is a bug.
			continue
		}
		if string(bundled) != string(source) {
			t.Errorf("%s differs from script/testdata/%s; keep the two copies identical", e.Name(), e.Name())
		}
	}
}

// userEffectsDir must land under the platform config dir so a listener can find
// it, and must not be empty.
func TestUserEffectsDirIsAbsolute(t *testing.T) {
	dir, err := userEffectsDir()
	if err != nil {
		t.Fatalf("userEffectsDir: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("userEffectsDir = %q, want an absolute path", dir)
	}
	if filepath.Base(dir) != "effects" {
		t.Fatalf("userEffectsDir = %q, want it to end in effects", dir)
	}
}
