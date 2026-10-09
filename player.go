// Package molo is the facade every UI imports. It exposes the controller as
// a small interface of cheap queries and non-blocking commands, and re-exports
// the state and event types so a UI never has to reach into internal/session.
//
// The package is deliberately UI-agnostic: no terminal code, no bubbletea, no
// os.Exit. A TUI, a desktop window and a headless CLI all program against the
// same Player.
package molo

import (
	"fmt"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/internal/session"
	"github.com/dlcuy22/molo/meta"
	"github.com/dlcuy22/molo/playback"
	"github.com/dlcuy22/molo/provider"
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

// Swapped confirms a live decoder or backend change on the current track.
// Kind is "decoder" or "backend".
type Swapped = session.Swapped

// PipelineChanged confirms that a new post-ring effect chain is in force.
// Stages is how many effects the chain runs after the change.
type PipelineChanged = session.PipelineChanged

// TrackEnded is emitted once per track when it reaches its natural end.
type TrackEnded = session.TrackEnded

// Failed reports a decode or device error. The player remains usable.
type Failed = session.Failed

// DebugEvent is one diagnostic record from the debug stream. It is separate
// from the control events: debug traffic is frequent and disposable, and it
// never displaces a control event. Which numeric fields are meaningful depends
// on Kind.
type DebugEvent = session.DebugEvent

// DebugKind classifies a DebugEvent.
type DebugKind = session.DebugKind

// The debug record kinds. They mirror the control events but carry diagnostics
// rather than state transitions.
const (
	// DebugTrack is emitted when a track is activated, naming its resolved
	// decoder, parser and backend.
	DebugTrack = session.DebugTrack
	// DebugUnderrun is emitted when the streamer's underrun counter advances.
	DebugUnderrun = session.DebugUnderrun
	// DebugSeek is emitted after a reposition lands.
	DebugSeek = session.DebugSeek
	// DebugSwap is emitted after a live decoder or backend change lands.
	DebugSwap = session.DebugSwap
	// DebugPipeline is emitted when a new post-ring chain is installed.
	DebugPipeline = session.DebugPipeline
	// DebugProbe is emitted when the asynchronous duration probe answers.
	DebugProbe = session.DebugProbe
	// DebugState is emitted for an accepted state transition.
	DebugState = session.DebugState
	// DebugError is emitted for a decode, device or engine failure.
	DebugError = session.DebugError
)

// Tap is the real-time audio feed a visualizer reads. Read copies up to
// len(dst) mono downmixed frames in time order, never blocks, and reports zero
// when nothing is buffered.
type Tap = session.Tap

// Settings is the subset of configuration that can change while a Player is
// alive. Everything else is read once by New.
//
// The fields take effect at different moments, which a UI should know:
//
//   - Volume applies immediately.
//   - Decoder and ProbeMode are read when the next track is built, so the
//     track already playing keeps the decoder it started with.
//   - Backend is read when a device is built. An open device cannot be
//     re-bound, so a backend change lands on the next Play that has to build
//     one.
//   - Pipeline is applied to the running audio as soon as the engine accepts
//     it, confirmed by a PipelineChanged event. Only its Post half is used;
//     Pre belongs to the streamer.
//
// Changing Decoder or Backend here is for the next build; to change what is
// playing now, call SwapDecoder or SwapBackend.
type Settings = session.Settings

// ErrInvalidSetting reports a rejected Settings update. It wraps a
// field-specific error, so a UI can tell "bad value" apart from other failures.
var ErrInvalidSetting = session.ErrInvalidSetting

// ErrIndexOutOfRange reports a PlayIndex jump to a queue position that does not
// exist. Like the other command errors it is returned synchronously, before the
// request reaches the engine.
var ErrIndexOutOfRange = session.ErrIndexOutOfRange

// ErrNoLiveTrack reports InsertQueueAndPlay while a track is already loaded,
// which would cut that track off. The caller appends instead, which is
// behaviourally what the user meant. It is returned synchronously.
var ErrNoLiveTrack = session.ErrNoLiveTrack

// ErrEmptyQueue reports PlayQueue or InsertQueue handed nothing to play. Match
// it with errors.Is.
var ErrEmptyQueue = session.ErrEmptyQueue

// ErrUnknownDecoder reports a decoder preference that named no registered
// codec. ApplySettings wraps it, so a UI can tell a bad decoder from any other
// rejected update. Match it with errors.Is.
var ErrUnknownDecoder = session.ErrUnknownDecoder

// ErrUnknownBackend reports a backend preference that named no registered
// backend. ApplySettings wraps it. Match it with errors.Is.
var ErrUnknownBackend = session.ErrUnknownBackend

// ErrBadProbeMode reports a duration mode outside the known set. ApplySettings
// wraps it. Match it with errors.Is.
var ErrBadProbeMode = session.ErrBadProbeMode

// ErrBadVolume reports a gain outside dsp's accepted range. ApplySettings wraps
// it. Match it with errors.Is.
var ErrBadVolume = session.ErrBadVolume

// ErrNoProvider reports a configured provider list where none claimed the
// reference, which WithProviders documents as a failed track rather than a
// silent fallback to a file path. Match it with errors.Is.
var ErrNoProvider = session.ErrNoProvider

// ValidateDecoder reports whether name is a usable decoder preference: empty
// means automatic selection. It is the same rule ApplySettings enforces, exposed
// so a caller can check a value before offering it (a CLI flag, a settings
// screen) without importing the registry or reimplementing the check.
func ValidateDecoder(name string) error { return session.ValidateDecoder(name) }

// ValidateBackend reports whether name is a usable playback backend, with empty
// meaning the default. Same rule as ApplySettings enforces.
func ValidateBackend(name string) error { return session.ValidateBackend(name) }

// SupportedExtensions lists the file extensions this build can decode, each
// lowercased and including the leading dot (" .flac"). A caller that wants to
// turn a directory into a queue uses it to tell a playable file from a stray
// one without opening anything.
func SupportedExtensions() []string { return decode.Default.Supported() }

// Codec is one selectable decoder, re-exported so a UI can build a chooser
// without importing the decode registry itself.
type Codec = decode.Codec

// Codecs lists the registered decoders, sorted, for a UI that offers a choice.
// The Name is the configuration key Settings.Decoder and SwapDecoder accept;
// a UI shows FriendlyName.
func Codecs() []Codec { return decode.Default.Codecs() }

// Backends lists the registered playback backend names, for a UI that offers a
// choice. Empty is always valid and means the default.
func Backends() []string { return playback.Names() }

// UserBackends lists the playback backends a UI should offer. It is Backends
// without the dev-only entries, so a fake or null sink never reaches a
// listener's output dropdown. Validation still accepts the full list, so a
// test or a CLI flag can select a dev backend by name.
func UserBackends() []string { return playback.UserNames() }

// Player is what a UI holds. Every command is non-blocking; the only errors a
// command returns are immediate validation problems such as an empty queue.
//
// Concurrency: every method is safe to call from any goroutine. A command
// appends to the engine's inbox under a short-lived mutex and wakes the control
// loop without blocking, and every slow step runs on its own goroutine, so a UI
// can drive the Player from whatever goroutine owns its event loop. Close is
// idempotent and safe to call from any goroutine; it blocks until the engine has
// stopped.
type Player interface {
	// Effects is the chain editor surface, embedded so every Player is
	// directly usable as one. Effects returns the same view.
	Effects

	// Snapshot returns a cheap, read-only view of the player at one instant.
	Snapshot() Snapshot
	// Events returns the stream of player events. It never blocks the engine: a
	// consumer that falls behind costs the oldest event, counted in
	// Snapshot.DroppedEvents.
	Events() <-chan Event

	// DebugEvents returns the diagnostic stream: per-track codec selection,
	// underrun deltas, seek and swap timings, pipeline installs and failures.
	// It is separate from Events and costs the engine nothing until it is
	// called. A consumer that falls behind costs the oldest record, counted in
	// DebugDropped.
	DebugEvents() <-chan DebugEvent

	// DebugDropped reports how many debug records a slow consumer cost. It is
	// the debug stream's counterpart to Snapshot.DroppedEvents.
	DebugDropped() int64

	// Play replaces the queue with one track and starts it. ref is a track
	// reference, not necessarily a filesystem path: it may be any string a
	// configured provider claims (see WithProviders). With no providers it is a
	// path resolved by the decode registry.
	Play(ref string) error
	// PlayQueue replaces the queue with refs and starts at the first. Each entry
	// is a track reference, as for Play.
	PlayQueue(refs []string) error
	// PlayIndex starts the queue entry at index, keeping the queue. It is what
	// a click on a row in a track list means. An index outside the queue is
	// rejected synchronously and changes nothing.
	PlayIndex(index int) error
	// Next advances to the next queued track; past the last track it stops.
	Next() error
	// Prev goes back one track, restarting the current one when it is the
	// first.
	Prev() error
	// SetShuffle turns baked shuffle on or off. On, the queue in force is
	// replaced with a shuffled permutation and the current track keeps playing;
	// advances then follow that order. Off restores the order the last shuffle
	// was taken from. It is the queue's order that changes, not a per-advance
	// random pick.
	SetShuffle(on bool) error
	// Shuffled reports whether baked shuffle is in force.
	Shuffled() bool
	// InsertQueue inserts refs into the queue at index without touching the
	// track that is playing, which is what "play next" means. An index at or
	// past the end appends. The live track keeps sounding; the inserted refs
	// are what the engine reaches when it advances.
	InsertQueue(index int, refs []string) error
	// InsertQueueAndPlay inserts refs and starts the first of them. With a
	// track already sounding it would cut that track off, so it is refused with
	// ErrNoLiveTrack and nothing changes; the caller then appends instead.
	InsertQueueAndPlay(index int, refs []string) error
	// Queue returns a copy of the current queue, which holds track references
	// (for example "ytm:abc123") rather than paths once a provider is in use.
	Queue() []string
	// Providers lists the names of the providers in effect, in priority order,
	// for a UI that shows where audio can come from. The default is the
	// built-in list ("Network", then "local").
	Providers() []string

	// Pause stops the device; it is ignored unless the player is playing.
	Pause() error
	// Resume restarts a paused device; it is ignored unless the player is
	// paused.
	Resume() error
	// Stop ends playback and keeps the queue, so a later Play resumes from the
	// same queue.
	Stop() error
	// Seek repositions the current track once the device has been paused, so
	// the device never reads a half-flushed ring.
	Seek(d time.Duration) error
	// SetVolume changes the post-ring gain in [0, 1]. It takes effect on the
	// next buffer and is safe to call while audio is running.
	SetVolume(v float64)

	// SwapDecoder forces the named decoder for the current track, reopening it
	// at the position playback has reached. Empty means automatic selection. A
	// bad name is rejected synchronously and changes nothing. With no live
	// track it just sets the preference the next track uses.
	SwapDecoder(name string) error
	// SwapBackend forces the named playback backend for the current device,
	// reopening the device against the same provider without touching the
	// stream. Empty means the default. An unknown name is rejected
	// synchronously. With no open device it just sets the preference the next
	// device uses.
	SwapBackend(name string) error

	// Settings reads the mutable configuration in effect.
	Settings() Settings
	// ApplySettings validates every field before applying any of them: a
	// rejected update changes nothing, so a UI can report the error without
	// worrying about partial state. It never blocks on disk or the device.
	ApplySettings(s Settings) error

	// Pipeline returns the effect chain in effect.
	Pipeline() dsp.Pipeline
	// ApplyPipeline validates every stage before applying any of them,
	// matching ApplySettings: a rejected update changes nothing. The accepted
	// chain is handed to the engine, which confirms it with PipelineChanged.
	ApplyPipeline(p dsp.Pipeline) error
	// EffectSchema returns the parameter schema for an effect kind, so a UI
	// can render controls without building the effect. It returns an error for
	// an unknown kind.
	EffectSchema(kind string) ([]dsp.Param, error)
	// EffectKinds lists the registered effect kinds, sorted.
	EffectKinds() []string
	// Effects returns the chain editor over this player, the same value the
	// embedded Effects methods act on.
	Effects() Effects

	// Tap returns the visualizer feed. The feed starts publishing on the first
	// Read, so holding a Tap without reading it costs the audio path nothing.
	Tap() Tap

	// Close stops every goroutine and releases the device. It is idempotent
	// and safe to call from any goroutine.
	Close() error
}

// Option customises a Player. It is a function over Config, so a new knob is
// one option and no new type, and a consumer outside the module can author one
// as a plain closure.
type Option func(*Config)

// Config holds the public construction knobs an Option writes. Zero fields take
// the documented default, and New maps a Config into the controller's own
// config, so the empty Config is the intended setup.
type Config struct {
	// Backend is the playback backend name, default "oto".
	Backend string

	// EventBuffer is the event channel depth, default 64.
	EventBuffer int

	// RingFrames is the streamer ring size in output frames. Zero selects the
	// streamer's own default of about 300 ms.
	RingFrames int

	// Volume is the initial gain in [0, 1]. Zero selects unity, because a
	// muted-by-default player is never what a caller means.
	Volume float64

	// Resolver describes each track. Nil selects meta.Default().
	Resolver meta.Resolver

	// Providers resolves track references, highest priority first. See
	// WithProviders for the ordering rule.
	Providers []provider.AudioProvider

	// ProbeMode selects how much work the asynchronous duration probe may do.
	// Nil selects the cheap tail probe. A pointer, not a value, because
	// core.DurationUnknown is the zero value and must stay distinguishable
	// from "the caller did not choose".
	ProbeMode *core.DurationMode

	// Decoder is the initial decoder preference: a name from
	// decode.Default.Codecs(), or empty for automatic selection.
	Decoder string

	// Pipeline is the post-ring effect chain. Zero is an empty chain, which
	// means only the master gain runs. The Pre half is the streamer's business
	// and is ignored here.
	Pipeline dsp.Pipeline

	// Experimental toggles not-yet-stable features. Zero is today's behaviour.
	Experimental Experimental
}

// Experimental toggles features that are not yet stable, re-exported so a UI
// can opt in without importing internal/session. The zero value is the current
// behaviour.
type Experimental = session.Experimental

// WithExperimental enables or disables experimental features for this Player.
// It is read once, at construction, like WithProviders: these knobs change how
// a source is opened and swapped, so they cannot be a runtime setting. The zero
// value leaves every path exactly as it behaves today.
func WithExperimental(e Experimental) Option {
	return func(c *Config) { c.Experimental = e }
}

// WithBackend selects the playback backend by registry name. The default is
// "oto".
func WithBackend(name string) Option {
	return func(c *Config) { c.Backend = name }
}

// WithEventBuffer sets the event channel depth. A deeper buffer tolerates a
// slower consumer before events are dropped.
func WithEventBuffer(n int) Option {
	return func(c *Config) { c.EventBuffer = n }
}

// WithRingFrames sets the streamer ring size in output frames. Zero selects
// the streamer default of about 300 ms.
func WithRingFrames(n int) Option {
	return func(c *Config) { c.RingFrames = n }
}

// WithVolume sets the initial gain in [0, 1]. Zero means unity.
func WithVolume(v float64) Option {
	return func(c *Config) { c.Volume = v }
}

// WithResolver supplies the metadata resolver. Nil selects meta.Default().
func WithResolver(r meta.Resolver) Option {
	return func(c *Config) { c.Resolver = r }
}

// WithProbeMode selects how much work the asynchronous duration probe may do
// when a track starts. It does not affect playback, only how quickly and how
// accurately the reported duration becomes known.
func WithProbeMode(mode core.DurationMode) Option {
	return func(c *Config) { c.ProbeMode = &mode }
}

// WithDecoder picks the decoder by registry name, for example "opus" or
// "opus-libopusfile". Empty means automatic selection, where the codec with the
// highest weight wins. It is the starting value; ApplySettings can change it
// while the player runs.
func WithDecoder(name string) Option {
	return func(c *Config) { c.Decoder = name }
}

// WithPipeline sets the initial post-ring effect chain. Only the Post half is
// used; the Pre half belongs to the streamer. Each stage is validated when the
// player is built, so a bad stage fails New rather than becoming a Failed event
// on the first Play. ApplyPipeline can change the chain while the player runs.
func WithPipeline(p dsp.Pipeline) Option {
	return func(c *Config) { c.Pipeline = p }
}

// WithProviders sets the audio source providers, highest priority first. Each
// track reference is given to the first provider whose Match returns true, so a
// catch-all provider such as provider.LocalAudio must stay last.
//
// Nil (the default) selects provider.Defaults(): direct http/https streaming,
// then the local filesystem. An explicit list replaces the default entirely, so
// a caller that wants only local files passes provider.LocalAudio{}.
//
// A non-empty list that claims nothing fails the track with ErrNoProvider
// rather than silently treating the reference as a file path.
func WithProviders(ps ...provider.AudioProvider) Option {
	return func(c *Config) { c.Providers = append([]provider.AudioProvider(nil), ps...) }
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
	var cfg Config
	for _, opt := range opts {
		opt(&cfg)
	}
	// An unset provider list selects the built-in sources. This keeps direct
	// http/https streaming available with no configuration while leaving local
	// playback identical, because LocalAudio marks its source Local and the
	// session keeps its own opener, resolver and prober for it.
	providers := cfg.Providers
	if providers == nil {
		providers = provider.Defaults()
	}

	s, err := session.New(session.Config{
		Backend:      cfg.Backend,
		EventBuffer:  cfg.EventBuffer,
		RingFrames:   cfg.RingFrames,
		Volume:       cfg.Volume,
		Resolver:     cfg.Resolver,
		Providers:    providers,
		ProbeMode:    cfg.ProbeMode,
		Decoder:      cfg.Decoder,
		Pipeline:     cfg.Pipeline,
		Experimental: cfg.Experimental,
	})
	if err != nil {
		return nil, err
	}

	return &player{session: s}, nil
}

func (p *player) Snapshot() Snapshot   { return p.session.Snapshot() }
func (p *player) Events() <-chan Event { return p.session.Events() }
func (p *player) DebugEvents() <-chan DebugEvent {
	return p.session.DebugEvents()
}
func (p *player) DebugDropped() int64   { return p.session.DebugDropped() }
func (p *player) Play(ref string) error { return p.session.Play(ref) }
func (p *player) PlayQueue(refs []string) error {
	return p.session.PlayQueue(refs)
}
func (p *player) PlayIndex(index int) error { return p.session.PlayIndex(index) }
func (p *player) Next() error               { return p.session.Next() }
func (p *player) Prev() error               { return p.session.Prev() }
func (p *player) SetShuffle(on bool) error  { return p.session.SetShuffle(on) }
func (p *player) Shuffled() bool            { return p.session.Shuffled() }
func (p *player) Queue() []string           { return p.session.Queue() }

// InsertQueue inserts refs at index without disturbing the current track.
func (p *player) InsertQueue(index int, refs []string) error {
	return p.session.InsertQueue(index, refs)
}

// InsertQueueAndPlay inserts refs and starts the first, unless a track is
// already live, which is refused rather than interrupted.
func (p *player) InsertQueueAndPlay(index int, refs []string) error {
	return p.session.InsertQueueAndPlay(index, refs)
}

// Providers lists the names of the providers in effect, in priority order, for
// a UI that shows where audio can come from. It is empty for the default
// local-only setup.
func (p *player) Providers() []string        { return p.session.Providers() }
func (p *player) Pause() error               { return p.session.Pause() }
func (p *player) Resume() error              { return p.session.Resume() }
func (p *player) Stop() error                { return p.session.Stop() }
func (p *player) Seek(d time.Duration) error { return p.session.Seek(d) }
func (p *player) SetVolume(v float64)        { p.session.SetVolume(v) }
func (p *player) SwapDecoder(name string) error {
	return p.session.SwapDecoder(name)
}
func (p *player) SwapBackend(name string) error {
	return p.session.SwapBackend(name)
}
func (p *player) Tap() Tap           { return p.session.Tap() }
func (p *player) Settings() Settings { return p.session.Settings() }

// ApplySettings validates on the caller's goroutine and never touches the
// control loop, so a bad value is an immediate error rather than a Failed
// event on the next track. The session wraps the field-specific errors, which
// ErrInvalidSetting lets a UI match on.
func (p *player) ApplySettings(s Settings) error {
	if err := p.session.SetSettings(s); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSetting, err)
	}

	return nil
}

// Pipeline returns the post-ring effect chain in effect.
func (p *player) Pipeline() dsp.Pipeline { return p.session.Pipeline() }

// ApplyPipeline validates on the caller's goroutine and, when the pipeline is
// accepted, queues its install. A bad stage is an immediate error rather than a
// Failed event, and nothing changes. The engine confirms the install with a
// PipelineChanged event.
func (p *player) ApplyPipeline(next dsp.Pipeline) error {
	return p.session.ApplyPipeline(next)
}

// EffectSchema returns the parameter schema for an effect kind. It reads the
// registry, so it works before anything is built; an unknown kind is an error.
func (p *player) EffectSchema(kind string) ([]dsp.Param, error) {
	return p.session.EffectSchema(kind)
}

// EffectKinds lists the registered effect kinds, sorted.
func (p *player) EffectKinds() []string { return p.session.EffectKindNames() }

func (p *player) Close() error { return p.session.Close() }
