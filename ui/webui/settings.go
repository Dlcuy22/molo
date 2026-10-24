// Persisted application settings for the webui.
//
// This file is the storage half of the settings panel: it reads and writes the
// small JSON document the panel edits, and translates it to and from the
// engine's own settings types. It is deliberately separate from the panel: the
// panel never touches the filesystem, and this file never touches the DOM.
//
// The store is a plain JSON file under the platform config directory, beside
// the Discord choice and the user's effect scripts. JSON is the right size for
// a handful of scalar settings: no dependency, human-readable, and easy to
// repair by hand. The write is atomic (a temp file and a rename), which is the
// same discipline the waveform cache uses, so a crash mid-write cannot leave a
// half-written file that a later read would reject.
//
// The read is forgiving on purpose: a missing, unreadable, or corrupt file is
// not an error, it is "no settings yet", and the defaults are used. A settings
// file is a convenience, never a reason the app fails to start.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/dlcuy22/molo"
)

// settingsVersion is the schema version of the persisted file. It is written on
// every save and checked on load, so a future layout change can migrate an old
// file rather than misread it.
const settingsVersion = 1

// AppSettings is the persisted settings document. It is the whole file: a
// version tag and the settings grouped by the panel's tabs. Only Playback has
// content today; the other groups are reserved so a later tab adds a field
// without changing the file's shape.
type AppSettings struct {
	Version  int              `json:"version"`
	Playback PlaybackSettings `json:"playback"`
	Stream   StreamSettings   `json:"stream"`
	General  GeneralSettings  `json:"general"`
}

// PlaybackSettings is the Playback tab. Crossfade is the track overlap; Repeat
// is the engine's repeat mode as the "off"/"all"/"one" strings, the same
// spelling the snapshot publishes.
type PlaybackSettings struct {
	Crossfade CrossfadeSettings `json:"crossfade"`
	Repeat    string            `json:"repeat"`
}

// StreamSettings is the Stream tab. It is empty today: the YouTube Music and
// network provider settings the tab is meant to hold have not been specified,
// so nothing is invented here.
type StreamSettings struct{}

// GeneralSettings is the General tab. VisualizerEnabled restores the spectrum
// pump on startup; it is the persisted half of the panel's visualizer switch.
type GeneralSettings struct {
	VisualizerEnabled bool `json:"visualizerEnabled"`
}

// CrossfadeSettings mirrors the engine's crossfade settings. It is restated
// rather than reusing molo.CrossfadeSettings directly so the JSON field names
// are the panel's contract and a change to the engine's Go field names cannot
// silently re-shape the file.
type CrossfadeSettings struct {
	Enabled       bool `json:"enabled"`
	DurationMs    int  `json:"durationMs"`
	SkipSameAlbum bool `json:"skipSameAlbum"`
}

// defaultAppSettings is the state a fresh install starts from. Crossfade is off
// and takes the engine's reference values, so an untouched install behaves as
// it did before the feature existed. The visualizer defaults to on, which is the
// pump's own starting state.
func defaultAppSettings() AppSettings {
	return AppSettings{
		Version: settingsVersion,
		Playback: PlaybackSettings{
			Crossfade: CrossfadeSettings{
				Enabled:       false,
				DurationMs:    molo.DefaultCrossfadeMs,
				SkipSameAlbum: true,
			},
			Repeat: "off",
		},
		General: GeneralSettings{
			VisualizerEnabled: true,
		},
	}
}

// settingsPath is where the settings document lives. It follows the platform
// config convention, landing beside the Discord choice and the effects folder.
func settingsPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(base, "molo", "settings.json"), nil
}

// loadAppSettings reads the persisted settings, falling back to the defaults
// for a missing, unreadable, or malformed file. It never returns an error: a
// settings file is a convenience, and a bad one must not stop the app.
func loadAppSettings() AppSettings {
	def := defaultAppSettings()

	p, err := settingsPath()
	if err != nil {
		return def
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return def
	}

	var s AppSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return def
	}

	return normalizeAppSettings(s, generalFieldPresence(raw))
}

// generalFieldPresence reports which General fields the raw document actually
// carried. A plain bool cannot tell "absent" from "explicitly false", and the
// two mean different things here: an absent visualizerEnabled is an install that
// predates the field and must keep its visualizer, while an explicit false is
// the user turning it off. The raw document is read only for this presence
// check; the written JSON keeps its plain bool shape.
func generalFieldPresence(raw []byte) map[string]bool {
	var probe struct {
		General map[string]json.RawMessage `json:"general"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	present := make(map[string]bool, len(probe.General))
	for k := range probe.General {
		present[k] = true
	}

	return present
}

// normalizeAppSettings fills any field a file did not carry and clamps the
// crossfade duration into the engine's accepted range, so a hand-edited file
// cannot wedge the panel or send the engine a value it would only clamp again.
//
// present is the set of General fields the raw file carried, and it is nil on
// the save path. That distinction is deliberate: the loader passes the real
// presence set so a bool the file omitted keeps its default, while a save from
// the panel already carries a complete document, so a nil set means "trust the
// value as sent" and must not re-default it. Defaulting on save would silently
// rewrite a user's explicit false (see generalFieldPresence).
func normalizeAppSettings(s AppSettings, present map[string]bool) AppSettings {
	def := defaultAppSettings()

	// A file written by an older schema may lack the version; treat it as the
	// current one rather than refusing it.
	s.Version = settingsVersion

	// A zero duration means the field was absent, not "instant"; fall back to
	// the default before clamping.
	if s.Playback.Crossfade.DurationMs == 0 {
		s.Playback.Crossfade.DurationMs = def.Playback.Crossfade.DurationMs
	}
	s.Playback.Crossfade.DurationMs = clampCrossfadeMs(s.Playback.Crossfade.DurationMs)

	// An empty repeat is the migration case: a file written before the field
	// existed. Both that and a hand-edit with an unknown value normalize to off
	// rather than reaching the engine with a value it would reject.
	if s.Playback.Repeat == "" {
		s.Playback.Repeat = def.Playback.Repeat
	} else if _, err := repeatFromName(s.Playback.Repeat); err != nil {
		s.Playback.Repeat = def.Playback.Repeat
	}

	// An install that predates the field keeps its visualizer on; only an
	// explicit false turns it off. On the save path present is nil and the value
	// the panel sent is taken as-is.
	if present != nil && !present["visualizerEnabled"] {
		s.General.VisualizerEnabled = def.General.VisualizerEnabled
	}

	return s
}

// clampCrossfadeMs bounds a duration to the engine's range.
func clampCrossfadeMs(ms int) int {
	switch {
	case ms < molo.MinCrossfadeMs:
		return molo.MinCrossfadeMs
	case ms > molo.MaxCrossfadeMs:
		return molo.MaxCrossfadeMs
	default:
		return ms
	}
}

// saveAppSettings writes the settings document atomically. A write goes to a
// temporary file in the same directory and is renamed into place, so a reader
// never sees a partial file and a crash mid-write leaves the previous settings
// intact.
func saveAppSettings(s AppSettings) error {
	s.Version = settingsVersion

	p, err := settingsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)

		return err
	}
	// Flush to disk before the rename, so the rename cannot publish a file
	// whose contents are still only in the page cache.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)

		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)

		return err
	}
	// Match the sibling discord.json's mode: a settings file is readable by the
	// user, not secret. CreateTemp makes 0600, so the mode is set explicitly.
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)

		return err
	}
	if err := os.Rename(name, p); err != nil {
		os.Remove(name)

		return err
	}

	return nil
}

// crossfadeFromSettings maps the persisted crossfade to the engine's type.
func crossfadeFromSettings(c CrossfadeSettings) molo.CrossfadeSettings {
	return molo.CrossfadeSettings{
		Enabled:       c.Enabled,
		DurationMs:    c.DurationMs,
		SkipSameAlbum: c.SkipSameAlbum,
	}
}

// crossfadeToSettings maps the engine's crossfade back to the persisted shape.
func crossfadeToSettings(c molo.CrossfadeSettings) CrossfadeSettings {
	return CrossfadeSettings{
		Enabled:       c.Enabled,
		DurationMs:    c.DurationMs,
		SkipSameAlbum: c.SkipSameAlbum,
	}
}

// AppSettings returns the persisted settings the panel renders. It is a plain
// read: the panel calls it once on open and mirrors edits locally.
func (s *PlayerService) AppSettings() AppSettings {
	s.appSettingsMu.Lock()
	defer s.appSettingsMu.Unlock()

	return s.appSettings
}

// SetAppSettings replaces the persisted settings, applies the playback half to
// the engine, and writes the document to disk. It returns the normalized
// document that was stored, so the caller can mirror exactly what was
// persisted rather than the value it sent (which may have been clamped).
//
// Applying first means a failed disk write still leaves the engine running what
// the user chose; the write is best effort, so a read-only config directory
// only costs persistence across a restart, never the setting in this session.
func (s *PlayerService) SetAppSettings(next AppSettings) (AppSettings, error) {
	// The panel always sends a whole document, so every field is present by
	// construction; a nil presence set means the normalizer treats the document
	// as fully specified.
	next = normalizeAppSettings(next, nil)

	s.appSettingsMu.Lock()
	s.appSettings = next
	s.appSettingsMu.Unlock()

	s.applyAppSettingsLocked()

	if err := saveAppSettings(next); err != nil {
		s.log.Warn("settings: could not persist", "error", err)

		return next, err
	}

	return next, nil
}

// applyAppSettingsLocked pushes the persisted playback settings onto the
// engine. It is called at startup and after every save, so the engine's copy
// and the panel's copy cannot drift. It is safe with no player yet, which is
// the case in tests that build the service without an engine.
//
// Mute and shuffle are deliberately not persisted: mute is gain 0, which
// SetVolume already covers, and shuffle is a one-shot queue reorder a restart
// should not replay.
func (s *PlayerService) applyAppSettingsLocked() {
	if s.player == nil {
		return
	}

	s.appSettingsMu.Lock()
	cfg := s.appSettings.Playback
	visualizer := s.appSettings.General.VisualizerEnabled
	s.appSettingsMu.Unlock()

	_ = s.player.SetCrossfade(crossfadeFromSettings(cfg.Crossfade))
	if mode, err := repeatFromName(cfg.Repeat); err == nil {
		_ = s.player.SetRepeat(mode)
	}
	// Carry the live shape through so only Enabled changes: a save must not
	// reset bars, fft or the frequency range to the shipped defaults, so the
	// current shape is read back and echoed with the new enable flag.
	s.spectrumMu.Lock()
	shape := s.spectrumCfg
	s.spectrumMu.Unlock()
	_ = s.ConfigureSpectrum(SpectrumConfig{
		Bars:    shape.Bars,
		MinHz:   int(shape.MinHz),
		MaxHz:   int(shape.MaxHz),
		FFT:     shape.FFT,
		Enabled: visualizer,
	})
}
