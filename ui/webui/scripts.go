// Bundled Lua effects.
//
// The webui ships a few example scripts so the effect window has something to
// show on a fresh install. They are embedded rather than read from disk so a
// packaged build carries them, and so the first run does not depend on a
// working directory. A user's own scripts are loaded from disk on top of these
// (see loadScripts).

package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/script"
)

//go:embed effects/*.lua
var bundledEffects embed.FS

// loadScripts registers the bundled Lua effects, then any scripts in the user's
// effects directory. A bundled script that fails to load is a build problem, so
// it is logged loudly; a user script that fails is expected (a typo in a file
// the user is editing) and is logged and skipped rather than aborting startup.
//
// Registration is process-wide and permanent: dsp.Register has no unregister, so
// a reload would collide. This runs once, from ServiceStartup.
func loadScripts() {
	loadBundledScripts()
	loadUserScripts()
}

// loadBundledScripts writes the embedded scripts to a temp dir and loads them
// through the same path as a user script, so there is one code path and the
// embedded files never diverge from what LoadDir expects.
func loadBundledScripts() {
	dir, err := os.MkdirTemp("", "molo-effects-")
	if err != nil {
		slog.Error("bundled effects: temp dir", "error", err)

		return
	}
	defer os.RemoveAll(dir)

	entries, err := fs.ReadDir(bundledEffects, "effects")
	if err != nil {
		slog.Error("bundled effects: read", "error", err)

		return
	}
	for _, e := range entries {
		data, err := bundledEffects.ReadFile("effects/" + e.Name())
		if err != nil {
			slog.Error("bundled effects: read file", "file", e.Name(), "error", err)

			continue
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
			slog.Error("bundled effects: write", "file", e.Name(), "error", err)

			continue
		}
	}

	registerDir(dir, "bundled")
}

// loadUserScripts registers scripts from the user's effects directory. The
// directory is created on first run so the path exists for the user to drop a
// file into.
func loadUserScripts() {
	dir, err := userEffectsDir()
	if err != nil {
		slog.Error("user effects: dir", "error", err)

		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("user effects: create dir", "error", err)

		return
	}

	registerDir(dir, "user")
}

// registerDir loads and registers every script in dir. A failure is logged with
// the file name and the rest continue, so one bad script does not hide the
// others.
func registerDir(dir, source string) {
	factories, err := script.LoadDir(dir)
	if err != nil {
		slog.Error("effects: load dir", "source", source, "dir", dir, "error", err)

		return
	}
	for _, f := range factories {
		if err := registerFactory(f); err != nil {
			slog.Error("effects: register", "source", source, "impl", f.Impl(), "error", err)

			continue
		}
		slog.Info("effect loaded", "source", source, "impl", f.Impl())
	}
}

// registerFactory registers one script, turning the registry's duplicate panic
// into an error. Two scripts with the same name are a real collision: the
// registry keeps the last, so a user script silently shadowing a bundled one is
// worth reporting rather than swallowing.
func registerFactory(f dsp.Factory) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("duplicate effect %q", f.Impl())
		}
	}()
	dsp.Register(f)

	return nil
}

// userEffectsDir is where a listener's own scripts live. It follows the XDG
// convention on Linux and the platform config dir elsewhere, so it lands beside
// the rest of the app's configuration.
func userEffectsDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(base, "molo", "effects"), nil
}
