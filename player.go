// Package player is the facade every UI imports. It exposes the controller as
// a small interface of cheap queries and non-blocking commands, and re-exports
// the state and event types so a UI never has to reach into internal/session.
//
// The package is deliberately UI-agnostic: no terminal code, no bubbletea, no
// os.Exit. A TUI, a GTK window and a headless CLI all program against the same
// Player.
package player

import (
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/internal/session"
	"github.com/dlcuy22/player/meta"
)

// State is the playback lifecycle. It is an alias of the controller's type, so
// a value crossing the boundary needs no conversion.
type State = session.State

const (
	// Idle is the state before any track has been started.
	Idle = session.StateIdle
	// Playing means a track is loaded and the device is consuming it.
	Playing = session.StatePlaying
	// Paused means a track is loaded but the device is paused.
	Paused = session.StatePaused
	// Stopped means playback ended, was stopped, or failed.
	Stopped = session.StateStopped
)

// Snapshot is a read-only view of the player at one instant. It is cheap
// enough to poll every frame.
type Snapshot = session.Snapshot

// Event is the sealed set of things the player reports. A UI type-switches on
// the concrete aliases below.
type Event = session.Event

// StateChanged is emitted for every accepted state transition.
type StateChanged = session.StateChanged

// TrackChanged is emitted when a new track becomes current.
type TrackChanged = session.TrackChanged

// Seeked confirms a seek after it has been applied.
type Seeked = session.Seeked

// TrackEnded is emitted once per track when it reaches its natural end.
type TrackEnded = session.TrackEnded

// Failed reports a decode or device error. The player remains usable.
type Failed = session.Failed

// Tap is the real-time audio feed a visualizer reads. Read copies up to
// len(dst) mono downmixed frames in time order, never blocks, and reports zero
// when nothing is buffered.
type Tap = session.Tap

// Player is what a UI holds. Every command is non-blocking; the only errors a
// command returns are immediate validation problems such as an empty queue.
type Player interface {
	Snapshot() Snapshot
	Events() <-chan Event

	Play(path string) error
	PlayQueue(paths []string) error
	Next() error
	Prev() error
	Queue() []string

	Pause() error
	Resume() error
	Stop() error
	Seek(d time.Duration) error
	SetVolume(v float64)

	// Tap returns the visualizer feed. The feed starts publishing on the first
	// Read, so holding a Tap without reading it costs the audio path nothing.
	Tap() Tap

	Close() error
}

// Option customises a Player. It is a function over the controller config, so
// a new knob is one option and no new type.
type Option func(*session.Config)

// WithBackend selects the playback backend by registry name. The default is
// "oto".
func WithBackend(name string) Option {
	return func(c *session.Config) { c.Backend = name }
}

// WithEventBuffer sets the event channel depth. A deeper buffer tolerates a
// slower consumer before events are dropped.
func WithEventBuffer(n int) Option {
	return func(c *session.Config) { c.EventBuffer = n }
}

// WithRingFrames sets the streamer ring size in output frames. Zero selects
// the streamer default of about 300 ms.
func WithRingFrames(n int) Option {
	return func(c *session.Config) { c.RingFrames = n }
}

// WithVolume sets the initial gain in [0, 1]. Zero means unity.
func WithVolume(v float64) Option {
	return func(c *session.Config) { c.Volume = v }
}

// WithResolver supplies the metadata resolver. Nil selects meta.Default().
func WithResolver(r meta.Resolver) Option {
	return func(c *session.Config) { c.Resolver = r }
}

// WithProbeMode selects how much work the asynchronous duration probe may do
// when a track starts. It does not affect playback, only how quickly and how
// accurately the reported duration becomes known.
func WithProbeMode(mode core.DurationMode) Option {
	return func(c *session.Config) { c.ProbeMode = &mode }
}

// player is the facade implementation. It adds nothing to the session: its
// whole job is to keep internal/session out of a UI's import graph.
type player struct {
	session *session.Session
}

// New builds a Player. It does not touch the file system or the audio server,
// so it succeeds headless; a backend failure surfaces as a Failed event on the
// first Play.
func New(opts ...Option) (Player, error) {
	var cfg session.Config
	for _, opt := range opts {
		opt(&cfg)
	}

	s, err := session.New(cfg)
	if err != nil {
		return nil, err
	}

	return &player{session: s}, nil
}

func (p *player) Snapshot() Snapshot     { return p.session.Snapshot() }
func (p *player) Events() <-chan Event   { return p.session.Events() }
func (p *player) Play(path string) error { return p.session.Play(path) }
func (p *player) PlayQueue(paths []string) error {
	return p.session.PlayQueue(paths)
}
func (p *player) Next() error                { return p.session.Next() }
func (p *player) Prev() error                { return p.session.Prev() }
func (p *player) Queue() []string            { return p.session.Queue() }
func (p *player) Pause() error               { return p.session.Pause() }
func (p *player) Resume() error              { return p.session.Resume() }
func (p *player) Stop() error                { return p.session.Stop() }
func (p *player) Seek(d time.Duration) error { return p.session.Seek(d) }
func (p *player) SetVolume(v float64)        { p.session.SetVolume(v) }
func (p *player) Tap() Tap                   { return p.session.Tap() }
func (p *player) Close() error               { return p.session.Close() }
