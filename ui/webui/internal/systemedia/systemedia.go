// Package systemedia publishes what is playing to the operating system's media
// controls and routes their transport commands back into the player.
//
// The platform-neutral half lives here: TrackInfo, Commands, and the Backend
// contract. Each transport is one file behind a build tag and this file imports
// none of them, so only the host's transport is built. Linux is MPRIS
// (mpris_linux.go); Windows will be SMTC and macOS the MediaPlayer framework,
// with a no-op backend elsewhere (backend_stub.go).
//
// Unlike the outbound-only Discord presence, MPRIS and SMTC are conversations:
// the OS reads state back and calls Play, Pause, Next and Seek, which is why
// Commands exists.
package systemedia

import (
	"errors"
	"log/slog"
	"sync"
	"time"
)

// errUnsupported is what a host with no transport reports from Start. It is
// neutral, so the caller can tell "this platform has no integration" apart
// from "the integration failed to start".
var errUnsupported = errors.New("systemedia: no media transport for this platform")

// TrackInfo is one snapshot of playback, in platform-neutral terms. Each
// transport maps it to its own vocabulary: MPRIS to the xesam metadata map and
// an mpris:trackid, SMTC to a music property set and a timeline.
//
// Artwork travels as the raw bytes the tag carried: MPRIS wants a URI and SMTC
// wants a thumbnail stream, and the bytes are the one form both build from.
type TrackInfo struct {
	// ID is the track reference, the same string the queue holds. It is what
	// MPRIS turns into mpris:trackid, so it must be stable for one track and
	// different across tracks.
	ID string

	Title  string
	Artist string
	Album  string

	// Artwork is the embedded cover art, or nil. MIME is its declared type
	// ("image/png"), which MPRIS ignores but a file extension needs.
	Artwork []byte
	MIME    string

	// Duration is the whole track; zero means the engine has not learned it
	// yet, which MPRIS reports as a missing mpris:length. Position is where
	// playback is, both in the output clock. Position moves continuously, so
	// it is refreshed on every Update rather than only on a track change.
	Duration time.Duration
	Position time.Duration

	// Volume is the post-ring gain in [0, 1], the MPRIS Volume property.
	Volume float64

	// Playing, Paused and Stopped are the three states MPRIS names. Exactly
	// one is set; an all-false TrackInfo is the idle state, which reports as
	// Stopped.
	Playing bool
	Paused  bool
	Stopped bool

	// Shuffle and Loop map to the MPRIS Shuffle and LoopStatus properties. Loop
	// is "" for None, "Track" or "Playlist".
	Shuffle bool
	Loop    string

	// CanGoNext, CanGoPrevious, CanSeek and CanControl are the capability
	// properties. A zero value is the conservative "no", so a transport that
	// has not filled them exposes controls that do nothing rather than controls
	// that lie.
	CanGoNext     bool
	CanGoPrevious bool
	CanSeek       bool
	CanControl    bool
}

// Commands is the inbound direction: the calls the operating system makes into
// the player. Every field is optional, and a nil field is a control the
// transport does not offer, so a half-wired backend degrades instead of
// panicking.
//
// A command must not call back into the Manager. The transport invokes these
// from a bus goroutine while it holds its property lock, so a command that
// synchronously pushed an Update would deadlock; the player's own snapshot
// pump is what publishes the result.
type Commands struct {
	Play       func()
	Pause      func()
	PlayPause  func()
	Next       func()
	Previous   func()
	Stop       func()
	Seek       func(time.Duration)
	SeekTo     func(time.Duration)
	SetVolume  func(float64)
	SetShuffle func(bool)
	SetLoop    func(string)
	Open       func(string)
	Raise      func()
	Quit       func()
}

// Backend is one platform transport. A backend owns its own bus connection or
// media session; the Manager serializes Start, Stop, Update, Clear and Seeked,
// but a transport may be entered from its own goroutines for inbound commands,
// so it guards its own state.
type Backend interface {
	// Start acquires the transport and begins serving. It fails when the
	// platform facility is unavailable, which is a normal outcome the caller
	// treats as "this machine cannot show system media controls", not an error
	// worth stopping the app for.
	Start() error
	// Stop releases the transport. It is idempotent.
	Stop()
	// Update publishes the current track. An empty TrackInfo is the idle state,
	// not a request to clear: Clear does that. Update is called on every
	// snapshot tick, so a transport must be cheap on an unchanged track and
	// must still refresh the continuously-moving Position.
	Update(track TrackInfo)
	// Clear removes the published track, which is what an emptied queue means.
	Clear()
	// Seeked announces a position change the OS did not cause, so a progress
	// bar jumps instead of waiting for the next poll.
	Seeked(position time.Duration)
}

// Manager owns one Backend and its lifecycle. It is safe for concurrent use:
// Update may be called from the snapshot pump while the transport's own
// goroutines deliver commands.
type Manager struct {
	log *slog.Logger

	mu        sync.Mutex
	backend   Backend
	published bool
}

// New builds a manager for the host platform. It does not start the backend;
// Start does. A nil logger falls back to the default.
func New(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}

	return &Manager{log: log}
}

// Start acquires the host transport and begins serving commands. It is
// idempotent: a second Start while running is ignored. A backend that cannot
// start is reported and leaves the manager inert, so a machine without a
// session bus simply has no system media controls.
func (m *Manager) Start(cmd Commands) error {
	m.mu.Lock()
	if m.backend != nil {
		m.mu.Unlock()

		return nil
	}
	m.mu.Unlock()

	backend, err := newBackend(cmd, m.log)
	if err != nil {
		return err
	}
	if err := backend.Start(); err != nil {
		backend.Stop()

		return err
	}

	m.mu.Lock()
	m.backend = backend
	m.published = false
	m.mu.Unlock()

	return nil
}

// Stop releases the transport and clears the published track. It is idempotent
// and safe to call after a failed Start.
func (m *Manager) Stop() {
	m.mu.Lock()
	backend := m.backend
	m.backend = nil
	m.published = false
	m.mu.Unlock()
	if backend != nil {
		backend.Stop()
	}
}

// Update publishes the current track. It forwards every call, because Position
// moves continuously and a transport needs the fresh value on each tick; the
// backend is what decides which properties actually changed and need a signal.
func (m *Manager) Update(track TrackInfo) {
	m.mu.Lock()
	backend := m.backend
	if backend != nil {
		m.published = true
	}
	m.mu.Unlock()
	if backend == nil {
		return
	}
	backend.Update(track)
}

// Clear removes the published track, which is what an emptied queue means. It
// only touches the transport when something was actually published.
func (m *Manager) Clear() {
	m.mu.Lock()
	backend := m.backend
	changed := m.published
	m.published = false
	m.mu.Unlock()
	if backend == nil || !changed {
		return
	}
	backend.Clear()
}

// Seeked announces a position change the OS did not cause, so a progress bar
// jumps instead of waiting for the next poll. It is a no-op when nothing is
// published.
func (m *Manager) Seeked(position time.Duration) {
	m.mu.Lock()
	backend := m.backend
	live := m.published
	m.mu.Unlock()
	if backend == nil || !live {
		return
	}
	backend.Seeked(position)
}
