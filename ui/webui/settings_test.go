package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dlcuy22/molo"
)

// TestAppSettingsDefaultsAreOffAndReference pins the fresh-install state the
// panel shows before anything is saved: crossfade off, the default duration,
// and the same-album skip armed.
func TestAppSettingsDefaultsAreOffAndReference(t *testing.T) {
	got := defaultAppSettings()

	if got.Version != settingsVersion {
		t.Fatalf("Version = %d, want %d", got.Version, settingsVersion)
	}
	if got.Playback.Crossfade.Enabled {
		t.Fatal("crossfade should default to off")
	}
	if got.Playback.Crossfade.DurationMs != molo.DefaultCrossfadeMs {
		t.Fatalf("duration = %d, want %d", got.Playback.Crossfade.DurationMs, molo.DefaultCrossfadeMs)
	}
	if !got.Playback.Crossfade.SkipSameAlbum {
		t.Fatal("skip same album should default to on")
	}
}

// TestAppSettingsRoundTrips proves the panel's edits survive a restart.
func TestAppSettingsRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A fresh install reads the defaults with no file present.
	if got := loadAppSettings(); got.Playback.Crossfade != defaultAppSettings().Playback.Crossfade {
		t.Fatalf("fresh load = %+v, want the defaults", got.Playback.Crossfade)
	}

	want := AppSettings{
		Playback: PlaybackSettings{
			Crossfade: CrossfadeSettings{Enabled: true, DurationMs: 8000, SkipSameAlbum: false},
		},
	}
	if err := saveAppSettings(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := loadAppSettings()
	if got.Playback.Crossfade != want.Playback.Crossfade {
		t.Fatalf("loaded = %+v, want %+v", got.Playback.Crossfade, want.Playback.Crossfade)
	}
}

// TestLoadAppSettingsToleratesCorruptFile keeps a hand-broken file from stopping
// the app: the defaults are used instead of an error.
func TestLoadAppSettingsToleratesCorruptFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	p, err := settingsPath()
	if err != nil {
		t.Fatalf("settingsPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := loadAppSettings(); got.Playback.Crossfade != defaultAppSettings().Playback.Crossfade {
		t.Fatalf("corrupt file did not fall back to defaults: %+v", got.Playback.Crossfade)
	}
}

// TestLoadAppSettingsClampsDuration proves a hand-edited out-of-range duration
// is corrected on read, so the panel and the engine never see a bad value.
func TestLoadAppSettingsClampsDuration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	p, err := settingsPath()
	if err != nil {
		t.Fatalf("settingsPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"playback":{"crossfade":{"enabled":true,"durationMs":99999}}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := loadAppSettings().Playback.Crossfade.DurationMs; got != molo.MaxCrossfadeMs {
		t.Fatalf("duration = %d, want the clamp %d", got, molo.MaxCrossfadeMs)
	}
}

// TestSaveAppSettingsIsAtomic proves the temp-file dance leaves no partial file
// behind: after a save the directory holds the settings file and no stray temp.
func TestSaveAppSettingsIsAtomic(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	if err := saveAppSettings(defaultAppSettings()); err != nil {
		t.Fatalf("save: %v", err)
	}

	p, err := settingsPath()
	if err != nil {
		t.Fatalf("settingsPath: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("a temp file was left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("settings file missing after save: %v", err)
	}
}

// TestSetAppSettingsReachesTheEngine proves the panel's save path applies the
// playback settings to the engine, not just to the file: the crossfade is
// pushed through SetCrossfade and the document is persisted.
func TestSetAppSettingsReachesTheEngine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	svc := newPlayerService()
	fp := newFakePlayer()
	svc.player = fp

	next := defaultAppSettings()
	next.Playback.Crossfade = CrossfadeSettings{Enabled: true, DurationMs: 9000, SkipSameAlbum: false}
	stored, err := svc.SetAppSettings(next)
	if err != nil {
		t.Fatalf("SetAppSettings: %v", err)
	}
	if stored.Playback.Crossfade != next.Playback.Crossfade {
		t.Fatalf("SetAppSettings stored = %+v, want %+v", stored.Playback.Crossfade, next.Playback.Crossfade)
	}

	if !fp.called("crossfade") {
		t.Fatal("SetAppSettings did not push the crossfade to the engine")
	}

	// The document is persisted and reads back with the same values.
	got := loadAppSettings()
	if got.Playback.Crossfade != next.Playback.Crossfade {
		t.Fatalf("persisted crossfade = %+v, want %+v", got.Playback.Crossfade, next.Playback.Crossfade)
	}
}

// TestSetAppSettingsReturnsTheNormalizedDocument proves a clamped value is
// echoed back rather than silently stored, so a caller can mirror the truth.
func TestSetAppSettingsReturnsTheNormalizedDocument(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	svc := newPlayerService()
	svc.player = newFakePlayer()

	next := defaultAppSettings()
	next.Playback.Crossfade = CrossfadeSettings{Enabled: true, DurationMs: 999999}
	stored, err := svc.SetAppSettings(next)
	if err != nil {
		t.Fatalf("SetAppSettings: %v", err)
	}
	if stored.Playback.Crossfade.DurationMs != molo.MaxCrossfadeMs {
		t.Fatalf("stored duration = %d, want the clamp %d", stored.Playback.Crossfade.DurationMs, molo.MaxCrossfadeMs)
	}
}

// TestCrossfadeMappingsRoundTrip pins the two translators between the persisted
// shape and the engine's, so a field rename cannot silently drop a setting.
func TestCrossfadeMappingsRoundTrip(t *testing.T) {
	c := CrossfadeSettings{Enabled: true, DurationMs: 3000, SkipSameAlbum: true}

	eng := crossfadeFromSettings(c)
	if eng.Enabled != c.Enabled || eng.DurationMs != c.DurationMs || eng.SkipSameAlbum != c.SkipSameAlbum {
		t.Fatalf("to engine = %+v, want %+v", eng, c)
	}

	back := crossfadeToSettings(eng)
	if back != c {
		t.Fatalf("round trip = %+v, want %+v", back, c)
	}
}
