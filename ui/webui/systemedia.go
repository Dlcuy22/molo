// OS media controls for the web UI: the bridge between the player's snapshot
// and the systemedia package, as presence.go is the bridge to Discord. It
// decides what the desktop shows and routes its transport commands back into
// the engine.
//
// Linux is MPRIS: the desktop media widget, lock screen, media keys and headset
// buttons all drive molo through it. The transport is chosen by build tag
// inside systemedia, so this file never names the platform.
package main

import (
	"sync"
	"time"

	"github.com/dlcuy22/molo/ui/webui/internal/systemedia"
)

// systemMediaState is the mutable half of the integration: the manager and the
// guard around it.
type systemMediaState struct {
	mu      sync.Mutex
	manager *systemedia.Manager
}

// initSystemMedia starts the host transport. It runs after the engine exists, so
// the desktop is told what is already playing. A host with no transport (or no
// session bus) logs once and stays inert: system media controls are a
// convenience, never a reason the player fails to start.
func (s *PlayerService) initSystemMedia() {
	mgr := systemedia.New(s.log)

	s.systemMedia.mu.Lock()
	s.systemMedia.manager = mgr
	s.systemMedia.mu.Unlock()

	if err := mgr.Start(s.systemMediaCommands()); err != nil {
		s.log.Info("system media controls unavailable", "error", err)

		return
	}

	// Describe whatever is already loaded, so starting the app on a paused or
	// playing track does not wait for the next change.
	s.syncSystemMedia(s.Snapshot())
}

// stopSystemMedia releases the transport at shutdown. It is idempotent and
// safe when initSystemMedia never ran.
func (s *PlayerService) stopSystemMedia() {
	s.systemMedia.mu.Lock()
	mgr := s.systemMedia.manager
	s.systemMedia.manager = nil
	s.systemMedia.mu.Unlock()
	if mgr != nil {
		mgr.Stop()
	}
}

// syncSystemMedia pushes the current track to the desktop. The transport
// compares each property and only signals the ones that moved, so an unchanged
// tick costs no bus traffic; Position is refreshed every tick because it is read
// on demand rather than signalled.
func (s *PlayerService) syncSystemMedia(snap Snapshot) {
	s.systemMedia.mu.Lock()
	mgr := s.systemMedia.manager
	s.systemMedia.mu.Unlock()
	if mgr == nil {
		return
	}

	// Nothing loaded is an empty transport, not a stale one.
	if snap.Path == "" || snap.Title == "" {
		mgr.Clear()

		return
	}

	mgr.Update(s.systemMediaTrack(snap))
}

// systemMediaSeeked announces a seek the desktop did not initiate, so a
// progress bar jumps instead of waiting for the next position tick.
func (s *PlayerService) systemMediaSeeked(position time.Duration) {
	s.systemMedia.mu.Lock()
	mgr := s.systemMedia.manager
	s.systemMedia.mu.Unlock()
	if mgr == nil {
		return
	}
	mgr.Seeked(position)
}

// systemMediaTrack maps the UI snapshot to the platform-neutral track. Cover art
// is read from the live engine snapshot because the UI carries only an id, and
// the path is re-checked so a track change mid-read cannot attach the wrong art.
func (s *PlayerService) systemMediaTrack(snap Snapshot) systemedia.TrackInfo {
	var art []byte
	var mime string
	if live := s.player.Snapshot(); live.Path == snap.Path {
		art = live.Meta.Tags.Cover
		mime = live.Meta.Tags.CoverMIME
	}

	return systemedia.TrackInfo{
		ID:         snap.Path,
		Title:      snap.Title,
		Artist:     snap.Artist,
		Album:      snap.Album,
		Artwork:    art,
		MIME:       mime,
		Duration:   time.Duration(snap.Duration) * time.Millisecond,
		Position:   time.Duration(snap.Position) * time.Millisecond,
		Volume:     snap.Volume,
		Playing:    snap.State == "playing",
		Paused:     snap.State == "paused",
		Stopped:    snap.State == "stopped",
		Shuffle:    snap.Shuffled,
		CanControl: true,
		// The seek bar is gated on a known duration everywhere else in the UI,
		// so the same rule decides whether the desktop is offered a seek.
		CanSeek: snap.Duration > 0,
		// The engine advances through its queue, so "next" and "previous" are
		// offered whenever there is a queue to move through.
		CanGoNext:     len(snap.Queue) > 0,
		CanGoPrevious: len(snap.Queue) > 0,
	}
}

// systemMediaCommands wires the desktop's transport controls to the engine. A
// nil field is a control the engine cannot honour, which the transport refuses
// rather than faking.
func (s *PlayerService) systemMediaCommands() systemedia.Commands {
	return systemedia.Commands{
		Play:      func() { _ = s.player.Resume() },
		Pause:     func() { _ = s.player.Pause() },
		PlayPause: func() { _ = s.TogglePause() },
		Next:      func() { _ = s.player.Next() },
		Previous:  func() { _ = s.player.Prev() },
		Stop:      func() { _ = s.player.Stop() },
		// The desktop sends a relative offset; the engine takes an absolute
		// position, so it is resolved against the live position and clamped.
		Seek: func(offset time.Duration) {
			target := s.player.Snapshot().Position + offset
			if target < 0 {
				target = 0
			}
			_ = s.player.Seek(target)
		},
		SeekTo: func(position time.Duration) {
			if position < 0 {
				position = 0
			}
			_ = s.player.Seek(position)
		},
		SetVolume:  func(v float64) { s.player.SetVolume(clamp01(v)) },
		SetShuffle: func(on bool) { _ = s.player.SetShuffle(on) },
		// SetLoop stays nil: the engine has no repeat mode, so the transport
		// refuses a LoopStatus write instead of faking it.
	}
}
