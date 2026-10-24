package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dlcuy22/molo"
)

// TestAppSettingsDefaultsAreOffAndReference pins the fresh-install state the
// panel shows before anything is saved: crossfade off, the default duration,
// the same-album skip armed, repeat off, and the visualizer on.
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
	if got.Playback.Repeat != "off" {
		t.Fatalf("repeat = %q, want off", got.Playback.Repeat)
	}
	if !got.General.VisualizerEnabled {
		t.Fatal("visualizer should default to on")
	}
}

// TestAppSettingsRoundTrips proves the panel's edits survive a restart, for
// every field including the repeat mode and the visualizer switch.
func TestAppSettingsRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A fresh install reads the defaults with no file present.
	if got := loadAppSettings(); got.Playback.Crossfade != defaultAppSettings().Playback.Crossfade {
		t.Fatalf("fresh load = %+v, want the defaults", got.Playback.Crossfade)
	}

	want := AppSettings{
		Playback: PlaybackSettings{
			Crossfade: CrossfadeSettings{Enabled: true, DurationMs: 8000, SkipSameAlbum: false},
			Repeat:    "one",
		},
		General: GeneralSettings{VisualizerEnabled: false},
	}
	if err := saveAppSettings(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := loadAppSettings()
	if got.Playback.Crossfade != want.Playback.Crossfade {
		t.Fatalf("loaded = %+v, want %+v", got.Playback.Crossfade, want.Playback.Crossfade)
	}
	if got.Playback.Repeat != want.Playback.Repeat {
		t.Fatalf("loaded repeat = %q, want %q", got.Playback.Repeat, want.Playback.Repeat)
	}
	if got.General.VisualizerEnabled != want.General.VisualizerEnabled {
		t.Fatalf("loaded visualizer = %v, want %v", got.General.VisualizerEnabled, want.General.VisualizerEnabled)
	}
}

// TestLoadAppSettingsNormalizesAnOldSchemaFile proves a file written before
// repeat and the visualizer existed comes back with repeat off and the
// visualizer on, which is the current behaviour rather than a wedged panel.
func TestLoadAppSettingsNormalizesAnOldSchemaFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	p, err := settingsPath()
	if err != nil {
		t.Fatalf("settingsPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file from the schema that predates both fields.
	body := `{"version":1,"playback":{"crossfade":{"enabled":true,"durationMs":6000}}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAppSettings()
	if got.Playback.Repeat != "off" {
		t.Fatalf("repeat = %q, want off for an old-schema file", got.Playback.Repeat)
	}
	if !got.General.VisualizerEnabled {
		t.Fatal("visualizer = false, want on for a file that predates the field")
	}
}

// TestLoadAppSettingsNormalizesAnUnknownRepeat proves a hand-edited repeat
// string is corrected on read, so the panel and the engine never see a value
// the engine would reject.
func TestLoadAppSettingsNormalizesAnUnknownRepeat(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	p, err := settingsPath()
	if err != nil {
		t.Fatalf("settingsPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"playback":{"crossfade":{"durationMs":6000},"repeat":"bogus"}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := loadAppSettings().Playback.Repeat; got != "off" {
		t.Fatalf("repeat = %q, want off for an unknown value", got)
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
// playback settings to the engine, not just to the file: the crossfade and the
// repeat mode are pushed through, and the document is persisted.
func TestSetAppSettingsReachesTheEngine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	svc := newPlayerService()
	fp := newFakePlayer()
	svc.player = fp

	next := defaultAppSettings()
	next.Playback.Crossfade = CrossfadeSettings{Enabled: true, DurationMs: 9000, SkipSameAlbum: false}
	next.Playback.Repeat = "all"
	stored, err := svc.SetAppSettings(next)
	if err != nil {
		t.Fatalf("SetAppSettings: %v", err)
	}
	if stored.Playback != next.Playback {
		t.Fatalf("SetAppSettings stored = %+v, want %+v", stored.Playback, next.Playback)
	}

	if !fp.called("crossfade") {
		t.Fatal("SetAppSettings did not push the crossfade to the engine")
	}
	if fp.Repeat() != molo.RepeatAll {
		t.Fatalf("repeat mode = %v, want RepeatAll", fp.Repeat())
	}

	// The document is persisted and reads back with the same values.
	got := loadAppSettings()
	if got.Playback != next.Playback {
		t.Fatalf("persisted playback = %+v, want %+v", got.Playback, next.Playback)
	}
}

// TestSetAppSettingsKeepsVisualizerShape proves a save toggles only the
// visualizer's Enabled: a non-default shape set beforehand must survive, since
// ConfigureSpectrum otherwise rebuilds from the shipped defaults.
func TestSetAppSettingsKeepsVisualizerShape(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	svc := newPlayerService()
	svc.player = newFakePlayer()

	want := SpectrumConfig{Bars: 64, MinHz: 120, MaxHz: 6000, FFT: 4096, Enabled: true}
	if err := svc.ConfigureSpectrum(want); err != nil {
		t.Fatalf("ConfigureSpectrum: %v", err)
	}

	next := defaultAppSettings()
	next.General.VisualizerEnabled = false
	if _, err := svc.SetAppSettings(next); err != nil {
		t.Fatalf("SetAppSettings: %v", err)
	}

	got := svc.SpectrumConfig()
	if got.Enabled {
		t.Fatal("Enabled = true after saving the visualizer off")
	}
	if got.Bars != want.Bars || got.MinHz != want.MinHz || got.MaxHz != want.MaxHz || got.FFT != want.FFT {
		t.Fatalf("shape = %+v, want the set shape %+v", got, want)
	}
}

// TestSetAppSettingsKeepsVisualizerOff proves the save path does not re-default
// the visualizer: a panel sending false must come back false and persist false,
// where the load path's "absent means on" rule must not apply.
func TestSetAppSettingsKeepsVisualizerOff(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	svc := newPlayerService()
	svc.player = newFakePlayer()

	next := defaultAppSettings()
	next.General.VisualizerEnabled = false
	stored, err := svc.SetAppSettings(next)
	if err != nil {
		t.Fatalf("SetAppSettings: %v", err)
	}
	if stored.General.VisualizerEnabled {
		t.Fatal("SetAppSettings returned visualizer on, want the false the panel sent")
	}

	persisted := loadAppSettings()
	if persisted.General.VisualizerEnabled {
		t.Fatal("SetAppSettings persisted visualizer on, want false")
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
