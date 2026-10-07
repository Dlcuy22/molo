// Discord Rich Presence for the web UI.
//
// This file is the bridge between the player's snapshot and the discordrpc
// package: it decides what the activity says, resolves the artwork the way
// Discord needs it (an https URL or a registered asset key), and keeps the
// manager fed without ever blocking the snapshot pump.
//
// The artwork split is the one non-obvious part. A YouTube Music track already
// has a public cover URL, so the cover is handed over as-is; its artist avatar
// needs one extra browse lookup, cached on disk. A local file has neither, so
// its embedded cover is uploaded to a temporary host and its small image falls
// back to the application asset.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/ui/webui/internal/discordrpc"
)

// Discord application identity. The id is OngoPlayer's registered application
// for now; moving to a dedicated molo application is a change to these
// constants and the asset uploads, nothing else.
const (
	discordAppID = "1498082439925334108"

	// Registered asset keys, shown when a track has no cover or avatar of its
	// own. Reusing the Android client's asset names keeps the presence
	// recognisable across platforms.
	discordLargeAssetKey  = "ongoplayer"
	discordLargeAssetText = "molo"
	discordSmallAssetKey  = "ongoplayer_small"
	discordSmallAssetText = "molo"

	// YouTube Music's watch URL, for the "listen" button on a remote track.
	ytmWatchURL = "https://music.youtube.com/watch?v="
	// The project home, for the second button.
	projectURL = "https://github.com/dlcuy22/molo"
)

// DiscordStatus is the settings UI's read of the integration: whether the user
// turned it on and whether the IPC socket is currently open.
type DiscordStatus struct {
	Enabled   bool `json:"enabled"`
	Connected bool `json:"connected"`
}

// presenceState is the mutable half of the integration. It is kept in one
// struct so the guard covers the manager, the uploader and the track the last
// activity described.
type presenceState struct {
	mu       sync.Mutex
	enabled  bool
	manager  *discordrpc.Manager
	uploader *discordrpc.ImageUploader

	// lastPath is the reference of the track the current activity describes, so
	// a 4 Hz tick that changes nothing does no work.
	lastPath string

	// wg tracks the background art-resolution goroutines, so shutdown waits for
	// them instead of letting one touch a torn-down engine. An Add is only ever
	// made while enabled is true, and stop clears enabled under mu before
	// waiting, so no Add can race the Wait.
	wg sync.WaitGroup
}

// discordConfigPath is where the on/off choice is remembered. It follows the
// platform config convention, landing beside the effects directory.
func discordConfigPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(base, "molo", "discord.json"), nil
}

// loadDiscordEnabled reads the remembered choice. A missing or unreadable file
// means "off", so a first run never surprises the user with a presence.
func loadDiscordEnabled() bool {
	p, err := discordConfigPath()
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var cfg struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false
	}

	return cfg.Enabled
}

// saveDiscordEnabled remembers the choice. It is best effort: a write failure
// only means the setting does not survive a restart.
func saveDiscordEnabled(enabled bool) error {
	p, err := discordConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Enabled bool `json:"enabled"`
	}{enabled})
	if err != nil {
		return err
	}

	return os.WriteFile(p, raw, 0o644)
}

// initDiscord restores the remembered choice at startup. It runs after the
// engine exists, so turning presence on can immediately describe what is
// already playing.
func (s *PlayerService) initDiscord() {
	if loadDiscordEnabled() {
		if err := s.SetDiscordEnabled(true); err != nil {
			s.log.Warn("discord: could not restore enabled state", "error", err)
		}
	}
}

// SetDiscordEnabled turns the integration on or off and remembers the choice.
func (s *PlayerService) SetDiscordEnabled(on bool) error {
	s.presence.mu.Lock()
	if on == s.presence.enabled {
		s.presence.mu.Unlock()

		return nil
	}

	if !on {
		mgr := s.presence.manager
		s.presence.manager = nil
		s.presence.enabled = false
		s.presence.lastPath = ""
		s.presence.mu.Unlock()
		if mgr != nil {
			mgr.Stop()
		}
		if err := saveDiscordEnabled(false); err != nil {
			s.log.Warn("discord: could not persist disabled state", "error", err)
		}
		s.log.Info("discord: presence disabled")

		return nil
	}

	mgr := discordrpc.New(discordrpc.Config{
		AppID:          discordAppID,
		LargeAssetKey:  discordLargeAssetKey,
		LargeAssetText: discordLargeAssetText,
		SmallAssetKey:  discordSmallAssetKey,
		SmallAssetText: discordSmallAssetText,
	}, s.log)
	// The live position and pause state are polled on every send, so a seek or
	// a pause is reflected without a fresh Update. The snapshot is the engine's
	// own view, so this stays correct across a decoder swap or a track change.
	mgr.GetPosition = func() int64 { return s.player.Snapshot().Position.Milliseconds() }
	mgr.GetDuration = func() int64 { return s.player.Snapshot().Duration.Milliseconds() }
	mgr.IsPaused = func() bool { return s.player.Snapshot().State == molo.Paused }

	if s.presence.uploader == nil {
		s.presence.uploader = discordrpc.NewImageUploader(s.log)
	}
	s.presence.manager = mgr
	s.presence.enabled = true
	s.presence.lastPath = ""
	s.presence.mu.Unlock()

	mgr.Start()
	if err := saveDiscordEnabled(true); err != nil {
		s.log.Warn("discord: could not persist enabled state", "error", err)
	}
	s.log.Info("discord: presence enabled")

	// Describe whatever is already loaded, so enabling mid-track does not wait
	// for the next change.
	s.syncPresence(s.Snapshot())

	return nil
}

// DiscordStatus reports the integration's state for the settings UI.
func (s *PlayerService) DiscordStatus() DiscordStatus {
	s.presence.mu.Lock()
	mgr := s.presence.manager
	enabled := s.presence.enabled
	s.presence.mu.Unlock()

	return DiscordStatus{Enabled: enabled, Connected: mgr != nil && mgr.Connected()}
}

// stopDiscord tears the integration down at shutdown. It clears the enabled
// flag first, so no new art-resolution goroutine starts, then waits for the
// ones already running before stopping the manager.
func (s *PlayerService) stopDiscord() {
	s.presence.mu.Lock()
	mgr := s.presence.manager
	s.presence.manager = nil
	s.presence.enabled = false
	s.presence.mu.Unlock()
	s.presence.wg.Wait()
	if mgr != nil {
		mgr.Stop()
	}
}

// syncPresence pushes the current track to Discord when it changed. It is cheap
// on an unchanged tick: one comparison against the track the activity already
// describes. Artwork that needs the network is resolved on its own goroutine,
// so this never blocks the snapshot pump.
func (s *PlayerService) syncPresence(snap Snapshot) {
	s.presence.mu.Lock()
	mgr := s.presence.manager
	if mgr == nil {
		s.presence.mu.Unlock()

		return
	}

	// Nothing sounding is an empty presence, not a stale one.
	if snap.Path == "" || snap.Title == "" || snap.State == "stopped" {
		changed := s.presence.lastPath != ""
		s.presence.lastPath = ""
		s.presence.mu.Unlock()
		if changed {
			mgr.Clear()
		}

		return
	}

	if snap.Path == s.presence.lastPath {
		s.presence.mu.Unlock()

		return
	}
	s.presence.lastPath = snap.Path
	// The Add is made under the same lock stopDiscord takes to clear the
	// manager, and stopDiscord clears it before waiting, so no Add can race the
	// Wait.
	s.presence.wg.Add(1)
	s.presence.mu.Unlock()

	// The text-only activity lands first, so the presence appears the moment the
	// track does. The cover and avatar follow in the background.
	mgr.Update(presenceTrack(snap, "", ""))

	go func() {
		defer s.presence.wg.Done()
		s.resolvePresenceArt(snap)
	}()
}

// presenceTrack builds the activity from the snapshot, with the image fields
// supplied by the caller. Duration is carried as a fallback only: the manager
// prefers the live engine value, because the probe may not have answered when
// this snapshot was taken. It is a free function so the mapping is testable
// without a service.
func presenceTrack(snap Snapshot, largeImage, smallImage string) discordrpc.TrackInfo {
	info := discordrpc.TrackInfo{
		Title:        snap.Title,
		Artist:       snap.Artist,
		LargeImage:   largeImage,
		LargeText:    snap.Album,
		SmallImage:   smallImage,
		SmallText:    snap.Artist,
		DurationMs:   snap.Duration,
		ActivityType: 2, // Listening
	}

	if isYTMRef(snap.Path) {
		id := ytmID(snap.Path)
		info.Buttons = []discordrpc.Button{
			{Label: "Listen on YouTube Music", URL: ytmWatchURL + id},
			{Label: "Visit molo", URL: projectURL},
		}
	} else {
		info.Buttons = []discordrpc.Button{
			{Label: "Visit molo", URL: projectURL},
		}
	}

	return info
}

// resolvePresenceArt resolves the cover and avatar for a track, then re-sends
// the activity. It runs off the pump; a failure leaves the text-only presence
// in place rather than clearing it.
func (s *PlayerService) resolvePresenceArt(snap Snapshot) {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	large := s.coverURL(ctx, snap)
	small := s.artistURL(ctx, snap)

	s.presence.mu.Lock()
	mgr := s.presence.manager
	// A track change while this was in flight makes the result stale; the newer
	// syncPresence already owns the activity.
	stale := s.presence.lastPath != snap.Path
	s.presence.mu.Unlock()
	if mgr == nil || stale {
		return
	}

	mgr.Update(presenceTrack(snap, large, small))
}

// coverURL returns the large image Discord should draw. A remote track uses its
// catalogue cover URL directly; a local file's embedded cover is uploaded to a
// temporary host, because Discord will not fetch a local path.
func (s *PlayerService) coverURL(ctx context.Context, snap Snapshot) string {
	if isYTMRef(snap.Path) {
		if t, ok := s.ytm.lookup(snap.Path); ok {
			return t.ThumbnailURL
		}

		return ""
	}

	// Read the live engine snapshot and confirm it is still the same track, so
	// a change mid-resolution cannot attach the next track's cover to this one.
	live := s.player.Snapshot()
	if live.Path != snap.Path {
		return ""
	}
	raw := live.Meta.Tags.Cover
	if len(raw) == 0 {
		return ""
	}

	s.presence.mu.Lock()
	up := s.presence.uploader
	s.presence.mu.Unlock()
	if up == nil {
		return ""
	}
	// The uploader hashes these bytes, so the same album cover across many
	// tracks is uploaded once and reused while its link stays alive.
	url, err := up.GetImageURL(raw)
	if err != nil {
		s.log.Debug("discord: cover upload failed", "error", err)

		return ""
	}

	return url
}

// artistURL returns the small image: the artist's avatar for a remote track,
// and nothing for a local file, which has no catalogue artist to look up.
func (s *PlayerService) artistURL(ctx context.Context, snap Snapshot) string {
	if !isYTMRef(snap.Path) {
		return ""
	}

	return s.ytmProv.artistArtFor(ctx, snap.Path)
}
