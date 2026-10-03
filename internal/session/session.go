package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/meta"
	"github.com/dlcuy22/molo/playback"
	"github.com/dlcuy22/molo/provider"
	"github.com/dlcuy22/molo/stream"
)

// The device format is fixed, which is what lets one device serve every track.
var canonical = core.CanonicalFormat

// defaultEventBuffer is the event channel depth. It is generous enough that a
// UI draining once a frame never loses an event, and small enough that a UI
// that has stopped reading cannot grow memory.
const defaultEventBuffer = 64

// devicePollInterval bounds how long a device error can go unnoticed. The oto
// backend fails on its own thread, so polling is the only way to surface it.
const devicePollInterval = 20 * time.Millisecond

var (
	// ErrEmptyQueue means PlayQueue was handed nothing to play.
	ErrEmptyQueue = errors.New("session: queue is empty")
	// ErrEmptyPath means Play was handed an empty path.
	ErrEmptyPath = errors.New("session: path is empty")
	// ErrIndexOutOfRange means PlayIndex named a queue position that does not
	// exist.
	ErrIndexOutOfRange = errors.New("session: queue index out of range")
	// ErrNoLiveTrack means InsertQueueAndPlay was asked to start a track while
	// one is already loaded, which would cut that track off. It is not an error
	// state, just a refusal: the caller appends instead.
	ErrNoLiveTrack = errors.New("session: a track is already loaded")
	// ErrNegativeSeek means a seek target was negative.
	ErrNegativeSeek = errors.New("session: seek target is negative")
	// ErrNoProber means the registry has no duration probe for the path. The
	// duration simply stays unknown.
	ErrNoProber = errors.New("session: no duration prober for the path")
	// ErrClosed means the session is shutting down.
	ErrClosed = errors.New("session: closed")
	// ErrUnknownDecoder means a decoder preference named no registered codec.
	ErrUnknownDecoder = errors.New("session: unknown decoder")
	// ErrUnknownBackend means a backend preference named no registered backend.
	ErrUnknownBackend = errors.New("session: unknown playback backend")
	// ErrBadProbeMode means a duration mode outside the known set.
	ErrBadProbeMode = errors.New("session: unknown duration mode")
	// ErrBadVolume means a gain outside dsp's accepted range.
	ErrBadVolume = errors.New("session: volume out of range")
	// ErrNoProvider means the session was configured with providers, none of
	// them claimed the reference, and the reference is not a local path. It is
	// the error an empty provider list never produces: with no providers the
	// session runs the legacy local path, which is behaviourally LocalAudio.
	// Non-empty providers are a deliberate configuration, so an unclaimed ref is
	// a caller mistake rather than something to silently open as a file.
	ErrNoProvider = provider.ErrNoProvider
	// ErrProviderNoOpener means a matching provider returned a Source with no
	// Opener. The provider matched, so this is not ErrNoProvider: it is a broken
	// provider, and passing the Source on would panic the producer goroutine.
	ErrProviderNoOpener = errors.New("session: provider returned no Opener")
	// ErrProviderNoDecoder means a provider's Opener returned a nil decoder with
	// a nil error. The Source contract requires a non-nil decoder or an error,
	// so this is a provider bug, not a decode failure.
	ErrProviderNoDecoder = errors.New("session: provider Opener returned a nil decoder")
	// ErrProviderNoDecoderSwap means SwapDecoder was asked to change the decoder
	// of a track owned by a provider. A provider supplies an opaque Opener, so
	// the session cannot rebuild it with a different decoder name; a decoder
	// swap is only meaningful on the local path, where the registry can open the
	// named codec. Local references are unaffected.
	ErrProviderNoDecoderSwap = errors.New("session: cannot swap the decoder of a provider reference")
	// ErrInvalidSetting marks a rejected Settings update. The field-specific
	// errors above are wrapped inside it, so a caller can match on this one to
	// tell "bad value" apart from every other failure.
	ErrInvalidSetting = errors.New("session: invalid setting")
)

// ValidateDecoder reports whether name is a usable decoder preference. The
// empty string means automatic selection and is always valid. This is the one
// copy of the rule: the facade and the config defaults both call it, so a
// chooser UI and a direct setter cannot disagree about what is legal.
func ValidateDecoder(name string) error {
	if name == "" {
		return nil
	}
	for _, c := range decode.Default.Codecs() {
		if c.Name == name {
			return nil
		}
	}

	return fmt.Errorf("%w: %q", ErrUnknownDecoder, name)
}

// ValidateBackend reports whether name is a usable playback backend.
// The empty string selects the default backend and is always valid.
func ValidateBackend(name string) error {
	if name == "" {
		return nil
	}
	if slices.Contains(playback.Names(), name) {
		return nil
	}

	return fmt.Errorf("%w: %q", ErrUnknownBackend, name)
}

// ValidateProbeMode reports whether mode is one of the defined duration modes.
// Bounds are spelled out rather than compared to DurationScan, because a future
// mode inserted in the middle of the enum would otherwise pass unnoticed.
func ValidateProbeMode(mode core.DurationMode) error {
	switch mode {
	case core.DurationUnknown, core.DurationProbe, core.DurationScan:
		return nil
	default:
		return fmt.Errorf("%w: %d", ErrBadProbeMode, mode)
	}
}

// ValidateVolume reports whether v is inside the gain's accepted range.
func ValidateVolume(v float64) error {
	if v < dsp.MinVolume || v > dsp.MaxVolume || math.IsNaN(v) {
		return fmt.Errorf("%w: %v", ErrBadVolume, v)
	}

	return nil
}

// Experimental toggles features that are not yet stable. The zero value is the
// current behaviour, so an empty Config changes nothing and no existing path is
// affected unless a caller opts in.
type Experimental struct {
	// SourceUpgrade enables the source-upgrade path: a provider Source that
	// offers Upgrade is started forward-only, and later swapped to the upgraded
	// decoder without flushing the streamer ring, so playback begins before the
	// whole track is available and the switch is inaudible.
	SourceUpgrade bool
}

// Config tunes a Session. Zero fields take the documented default, so the
// empty Config is the intended setup.
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

	// Providers resolves track references, highest priority first. Nil or empty
	// selects the legacy local path, which is behaviourally LocalAudio and keeps
	// the historic Config.openDecoder seam intact. When the list is non-empty
	// the first provider whose Match returns true wins, so order is significant
	// and a catch-all provider such as LocalAudio must stay last. A non-empty
	// list that claims nothing is an error (ErrNoProvider) rather than a silent
	// fallback to a file.
	Providers []provider.AudioProvider

	// ProbeMode selects how much work the asynchronous duration probe may do.
	// Nil selects the cheap tail probe. A pointer, not a value, because
	// core.DurationUnknown is the zero value and must stay distinguishable
	// from "the caller did not choose".
	ProbeMode *core.DurationMode

	// Decoder is the initial decoder preference: a name from
	// decode.Default.Codecs(), or empty for automatic selection. It is only the
	// starting value; Settings can change it while the session runs.
	Decoder string

	// Pipeline is the post-ring effect chain. Zero is an empty chain, which is
	// today's behaviour: only the master gain runs. The Pre half is the
	// streamer's business and is ignored here.
	Pipeline dsp.Pipeline

	// Experimental toggles not-yet-stable features. Zero is today's behaviour,
	// so leaving it unset keeps every existing path byte-for-byte.
	Experimental Experimental

	// validateDecoder overrides the decoder-name check. Tests that drive the
	// session with fake codecs set it, so they do not have to register into the
	// process-wide registry. Production code leaves it nil and the real
	// registry is consulted.
	validateDecoder func(name string) error

	// The seams below let tests drive the controller without a file system or
	// an audio server. Production code leaves them nil.
	//
	// openDecoder takes the decoder preference per call rather than being bound
	// once, because the preference can change between tracks. An empty name
	// means automatic selection.
	openDecoder func(name, path string) (decode.Decoder, error)
	probeStream func(path string, opts decode.ProbeOptions) (core.StreamInfo, error)
	// newDevice takes the backend name so it can be overridden per call; only
	// the first build after a backend change uses the new name.
	newDevice func(backend string) (playback.Device, error)
}

// runtimeSettings is the mutable half of the configuration. Volume lives on
// the gain itself, so it is not duplicated here.
//
// Backend is included because a change must survive until the next device is
// built, but note that an already-open device keeps playing on the old backend:
// a Device cannot be re-bound, so the switch lands on the next Play that has to
// build one.
type runtimeSettings struct {
	mu        sync.Mutex
	decoder   string
	backend   string
	probeMode core.DurationMode
	pipeline  dsp.Pipeline

	// pipelineGen counts accepted pipeline changes. A build captures it
	// together with the pipeline it built, and the control loop installs that
	// build's effects only while the generation still matches. Without it, a
	// pipeline change that landed while a track was building would be
	// overwritten by the older snapshot the build had already started from.
	pipelineGen uint64

	// pending is the built chain for pipelineGen, stored in the same critical
	// section that advances the generation. The editor reads it together with
	// the pipeline so it can resolve a stage the control loop has not published
	// yet; storing it separately would leave a window where the generation is
	// new but the effects are not.
	pending *installedChain
}

func (r *runtimeSettings) snapshot() (decoder, backend string, mode core.DurationMode) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.decoder, r.backend, r.probeMode
}

func (r *runtimeSettings) set(decoder, backend string, mode core.DurationMode) {
	r.mu.Lock()
	r.decoder = decoder
	r.backend = backend
	r.probeMode = mode
	r.mu.Unlock()
}

// pipelineSnapshot returns a copy of the current pipeline and the generation it
// belongs to. The stage slices are cloned so a caller cannot mutate the ones in
// force; the Params maps are shared because they are only read while a stage
// compiles its own state.
func (r *runtimeSettings) pipelineSnapshot() (dsp.Pipeline, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return clonePipeline(r.pipeline), r.pipelineGen
}

// pendingSnapshot returns the built chain for the current generation, or nil
// when the newest accepted change has already been published and cleared. The
// pipeline, the generation and this chain are read under one lock, so a caller
// never sees a new generation with the previous change's effects.
func (r *runtimeSettings) pendingSnapshot() *installedChain {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.pending
}

// setPipeline stores the pipeline as the one in force and advances the
// generation. It is how the initial Config pipeline is adopted, where a change
// count is meaningless because nothing existed before.
func (r *runtimeSettings) setPipeline(p dsp.Pipeline) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pipeline = clonePipeline(p)
	r.pipelineGen++

	return r.pipelineGen
}

// setPipelineIfChanged stores the pipeline only when it differs from the one in
// force and reports whether it did. The compare and the store share one lock, so
// two concurrent callers cannot both decide a change is new. Skipping an
// identical pipeline is what stops a settings change that does not touch the
// chain from re-installing it, resetting effect state mid-track and emitting a
// spurious PipelineChanged.
//
// Only the Post halves are compared: this is the same Post-only surface Settings
// exposes, and a Pre change is not this method's business. Normalising with
// clonePipeline is what makes a Pipeline carrying a Pre half compare equal to
// the stored one that has none.
func (r *runtimeSettings) setPipelineIfChanged(p dsp.Pipeline, effects []dsp.Effect, keys []stageKey) (bool, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reflect.DeepEqual(r.pipeline, clonePipeline(p)) {
		return false, r.pipelineGen
	}
	r.pipeline = clonePipeline(p)
	r.pipelineGen++
	// The pending chain is stored in the same critical section that advances
	// the generation, so a reader can never see the new generation without the
	// effects that belong to it.
	r.pending = &installedChain{effects: effects, gen: r.pipelineGen, keys: keys}

	return true, r.pipelineGen
}

// clearPending drops the pending chain once the control loop has published it,
// so a later read does not treat a live chain as still pending. It is a no-op
// when a newer change has already replaced it.
func (r *runtimeSettings) clearPending(gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pipelineGen == gen {
		r.pending = nil
	}
}

// pipelineGenNow reports the current generation without copying the pipeline.
func (r *runtimeSettings) pipelineGenNow() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.pipelineGen
}

// Pipeline returns a copy of the pipeline in force. Verifying a pipeline round
// trips is reading it back, so the copy is deliberate: a caller that mutates
// what it gets cannot reach the engine's copy.
func (r *runtimeSettings) Pipeline() dsp.Pipeline {
	r.mu.Lock()
	defer r.mu.Unlock()

	return clonePipeline(r.pipeline)
}

// clonePipeline copies the post-ring stages of a pipeline. The session's
// effect surface is Post-only: the Pre half belongs to the streamer, so it is
// dropped here rather than stored and echoed back unused. A nil and an empty
// slice both mean "no stages", so the copy preserves nil.
func clonePipeline(p dsp.Pipeline) dsp.Pipeline {
	return dsp.Pipeline{Post: append([]dsp.Spec(nil), p.Post...)}
}

func (r *runtimeSettings) decoderName() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.decoder
}

func (r *runtimeSettings) setDecoder(name string) {
	r.mu.Lock()
	r.decoder = name
	r.mu.Unlock()
}

func (r *runtimeSettings) setBackend(name string) {
	r.mu.Lock()
	r.backend = name
	r.mu.Unlock()
}

func (r *runtimeSettings) backendName() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.backend
}

func (r *runtimeSettings) probeDurationMode() core.DurationMode {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.probeMode
}

// command is one queued public call. Commands are values, so a caller never
// shares state with the controller.
type command struct {
	kind  cmdKind
	path  string
	paths []string
	name  string
	pos   time.Duration

	// index is the queue position a cmdPlayIndex jumps to, or the position a
	// cmdInsertQueue inserts at. It is a plain int rather than a replayed path
	// list so the jump reuses the queue the controller already holds, the way
	// Next and Prev do.
	index int

	// play makes cmdInsertQueue start the first inserted ref instead of only
	// splicing it in. It is a field on the insert command rather than a second
	// PlayIndex call because PlayIndex validates its index synchronously,
	// before the control loop has grown the queue, so the pair cannot be sent
	// as two commands.
	play bool

	// effects is a pre-built, configured post-ring chain an ApplyPipeline or
	// SetSettings accepted. It was built on the caller's goroutine, so the
	// control loop only has to publish it with an atomic swap.
	effects []dsp.Effect

	// keys identify the stages effects correspond to, in order.
	keys []stageKey

	// pipelineGen is the generation the effects belong to. The control loop
	// records it beside the published list so the effect editor can tell
	// whether a description and the live chain come from the same change.
	pipelineGen uint64

	// installed, when non-nil, is closed by the control loop once the chain has
	// been published. The effect editor waits on it so AddEffect returns with
	// the stage already live, which is what lets the caller set a parameter on
	// it immediately. It is nil for the ordinary asynchronous ApplyPipeline.
	installed chan struct{}
}

type cmdKind uint8

const (
	cmdPlay cmdKind = iota
	cmdPlayQueue
	cmdPlayIndex
	cmdInsertQueue
	cmdNext
	cmdPrev
	cmdPause
	cmdResume
	cmdStop
	cmdSeek
	cmdSwapDecoder
	cmdSwapBackend
	cmdApplyPipeline
)

// openResult is a finished synchronous pipeline build, delivered to the
// control loop so the build itself never runs there. effects is the post-ring
// chain the worker built and reset for this track, and pipelineGen is the
// generation it was built from, so a pipeline change that landed while the build
// ran is not clobbered by the older snapshot the build started with.
type openResult struct {
	seq         uint64
	index       int
	path        string
	streamer    *stream.Streamer
	effects     []dsp.Effect
	pipelineGen uint64
	// keys identify the stages the built effects correspond to. They let the
	// editor resolve a stage by identity rather than by index into a generation,
	// so a caller that read an older description still finds its effect.
	keys    []stageKey
	info    core.StreamInfo
	decoder string
	parser  string
	err     error

	// providerMeta is true when a provider supplied the track's description.
	// It changes two things downstream: activate seeds the view from Meta
	// instead of an empty Meta, and enrich skips the local resolver, which has
	// nothing to read for a remote ref and would clobber the provider's tags
	// with filename-derived junk.
	providerMeta bool
	meta         meta.Meta
	// probe is the provider's optional duration probe. Nil means the total
	// stays unknown for a provider ref, because the session's own prober only
	// works on a local path.
	probe func(mode core.DurationMode) (core.StreamInfo, error)

	// upgrade optionally replaces the decoder once the source has more bytes
	// available. It is carried only when the experimental seamless-swap path is
	// on and the provider offered one, so with the flag off the build behaves
	// exactly as before. It runs off the control loop after the track is
	// playing, because waiting for a whole download must not delay audio.
	upgrade func(context.Context) (stream.Opener, error)
}

type probeResult struct {
	seq   uint64
	index int
	info  core.StreamInfo
	err   error
}

type metaResult struct {
	seq   uint64
	index int
	m     *meta.Meta
	err   error
}

// seekResult is a finished reposition, delivered to the control loop so the
// seek itself never runs there. stream and seq identify the track it was
// started for, so the controller can drop a result whose track has moved on.
// The same type carries a decoder swap (decoder/parser name the new
// implementation) and a backend swap (device is the newly opened one), because
// all three are the same "one slow engine step at a time" contract.
type seekResult struct {
	seq     uint64
	stream  *stream.Streamer
	kind    requestKind
	name    string
	decoder string
	parser  string
	device  playback.Device
	target  time.Duration
	from    time.Duration
	resume  bool
	elapsed time.Duration
	err     error
}

// view is the mutable state Snapshot reads. The control goroutine is the only
// writer; readers take the same short-lived lock.
type view struct {
	state State

	path     string
	meta     meta.Meta
	duration time.Duration
	lastPos  time.Duration

	// decoder and parser label the streamer feeding the device. They belong to
	// the track, so startIndex clears them with the rest of the per-track state.
	decoder string
	parser  string

	// providerRef is true when the live track was opened through a provider
	// Source rather than the session's own decoder path. SwapDecoder refuses
	// such a track, so the flag lives here, under the same lock SwapDecoder
	// reads the path under, rather than being re-derived from the ref (which
	// would need to inspect the provider's identity).
	providerRef bool

	queueIndex int
	queueLen   int
}

// Session is the controller. It owns the queue, the current streamer, the
// device and the gain, and it is the only place those change.
//
// Concurrency: public commands append to an inbox under a short-lived mutex
// and signal the control loop without blocking. The control loop moves the
// batch and reacts; every slow operation (opening a decoder, probing a file,
// resolving tags) runs on its own goroutine and reports back through a
// channel. That is what makes a command non-blocking by construction rather
// than by the caller being careful.
type Session struct {
	cfg    Config
	gain   *dsp.Gain
	chain  *dsp.Chain
	tap    *tap
	events *eventQueue
	v      view

	mu     sync.Mutex
	inbox  []command
	closed bool

	wake    chan struct{}
	closing chan struct{}
	stopped chan struct{}
	feed    chan feedEvent
	openCh  chan openResult
	probeCh chan probeResult
	metaCh  chan metaResult
	seekCh  chan seekResult
	// seamlessCh carries a finished source upgrade from its worker to the
	// control loop, which decides when the one engine worker can take it.
	seamlessCh chan *seekRequest

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once

	// runtime holds the values a caller may change while the session runs. It
	// is read from worker goroutines (build) as well as the control goroutine,
	// so it is guarded by its own mutex rather than the control loop's state.
	runtime runtimeSettings

	// effectIDs mints unique stage IDs for the effect editor. It is separate
	// from the control loop's state because AddEffect mints one on the caller's
	// goroutine.
	effectIDs effectIDs

	// editorMu serializes the effect editor's read-modify-write. Add, remove and
	// move each read the pipeline, edit a copy and hand it back, so without this
	// two concurrent calls would start from the same list and the later install
	// would drop the other's stage.
	editorMu sync.Mutex

	// installed is the chain the audio path is running, published as one unit
	// with the pipeline generation it belongs to. The editor pairs a description
	// with the live effects only while the generations agree, and it reads the
	// pair together: two separate atomics could be caught between the two stores
	// and pair one generation's identity with another generation's meters.
	//
	// The effects a change has accepted but not yet published live in
	// runtimeSettings.pendingEffects, beside the generation, so the editor can
	// resolve a stage in that window too.
	installed atomic.Pointer[installedChain]

	// workerWG tracks the asynchronous build/probe/meta goroutines so
	// shutdown can wait for them before draining their result channels.
	workerWG sync.WaitGroup

	// The fields below are owned by the control goroutine and are never read
	// from another one, so they need no lock.
	provider      *routedProvider
	device        playback.Device
	deviceStarted bool
	live          *stream.Streamer
	queue         []string
	index         int
	seq           uint64
	inFlight      bool

	// Seek is the one slow step that is not a build, so the controller starts
	// it on a worker and keeps going. seekBusy is true while a worker is
	// repositioning and seekPending holds the newest target that has not
	// started yet (latest-wins). Both are owned by the control goroutine; the
	// worker only reports on seekCh.
	seekBusy    bool
	seekPending *seekRequest

	// pendingSeamless holds a finished source upgrade that could not start
	// because a seek or swap owned the worker slot. A user seek must never be
	// clobbered by an optimisation, so the upgrade waits here and is promoted
	// when the worker falls idle. It is bound to the same streamer and seq as
	// any other request, so a track change drops it.
	pendingSeamless *seekRequest

	// upgradeCancel cancels the current track's source-upgrade download. It is
	// owned by the control goroutine and called from retireLive, so a track the
	// user left stops its upgrade instead of downloading until session Close.
	upgradeCancel context.CancelFunc
}

// requestKind selects what a queued engine step does. A plain seek and a
// decoder swap both reposition the streamer; a backend swap rebuilds the
// device. They share one worker and one latest-wins slot so they cannot race
// each other on the same streamer or device.
type requestKind uint8

const (
	requestSeek requestKind = iota
	requestSwapDecoder
	requestSwapBackend
	requestSwapSeamless
)

// seekRequest is one accepted seek, swap or device rebuild, either in flight or
// waiting to replace an in-flight one. from is captured when the device has
// been parked, so it is the position the operation actually started from; for a
// decoder swap frame is that same position in the streamer's frame domain, kept
// exact so the swap does not quantise the position through a duration.
type seekRequest struct {
	seq    uint64
	stream *stream.Streamer
	kind   requestKind
	name   string
	path   string
	target time.Duration
	frame  int64
	from   time.Duration
	resume bool
	// providerRef mirrors the live track's provider ownership, captured on the
	// control loop, so the worker refuses a decoder swap on a provider ref even
	// if the caller raced a track change.
	providerRef bool

	// open and labels drive a seamless swap: open builds the upgraded decoder
	// and labels receives what that decoder reports about itself. The labels are
	// written by the producer goroutine inside the streamer and read only once
	// the swap has been acknowledged, exactly like decoderOpener's.
	open   stream.Opener
	labels *openerLabels
}

// New builds a session and starts its control goroutine. It does not touch the
// audio device or the file system, so it succeeds on a machine with no audio
// server; a backend failure surfaces as a Failed event on the first Play.
func New(cfg Config) (*Session, error) {
	if cfg.Backend == "" {
		cfg.Backend = "oto"
	}
	if cfg.EventBuffer <= 0 {
		cfg.EventBuffer = defaultEventBuffer
	}
	if cfg.Volume == 0 {
		cfg.Volume = 1
	}
	if cfg.Resolver == nil {
		cfg.Resolver = meta.Default()
	}
	if cfg.openDecoder == nil {
		cfg.openDecoder = decode.Default.OpenNamed
	}
	if cfg.probeStream == nil {
		cfg.probeStream = probeWithDefaultRegistry
	}
	if cfg.ProbeMode == nil {
		mode := core.DurationProbe
		cfg.ProbeMode = &mode
	}
	if cfg.newDevice == nil {
		cfg.newDevice = func(backend string) (playback.Device, error) {
			return playback.Open(backend)
		}
	}

	// An invalid initial preference is a programming error, so it is rejected
	// here rather than becoming a Failed event on the first Play. The pipeline
	// is validated the same way: building each stage is what resolves its kind
	// and coerces its parameters, so a bad preset fails here.
	if err := cfg.validate(cfg.Decoder, ValidateDecoder); err != nil {
		return nil, err
	}
	if err := ValidateBackend(cfg.Backend); err != nil {
		return nil, err
	}
	if err := ValidateVolume(cfg.Volume); err != nil {
		return nil, err
	}
	if err := ValidatePipeline(cfg.Pipeline); err != nil {
		return nil, err
	}

	s := &Session{
		cfg:     cfg,
		gain:    dsp.NewGain(cfg.Volume),
		chain:   dsp.NewChain(),
		tap:     newTap(),
		events:  newEventQueue(cfg.EventBuffer),
		v:       view{state: StateIdle, queueIndex: -1},
		wake:    make(chan struct{}, 1),
		closing: make(chan struct{}),
		stopped: make(chan struct{}),
		feed:    make(chan feedEvent, 8),
		openCh:  make(chan openResult, 4),
		probeCh: make(chan probeResult, 4),
		metaCh:  make(chan metaResult, 4),
		seekCh:  make(chan seekResult, 1),

		seamlessCh: make(chan *seekRequest, 1),
	}
	s.runtime.set(cfg.Decoder, cfg.Backend, *cfg.ProbeMode)
	s.runtime.setPipeline(cfg.Pipeline)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.provider = newRoutedProvider(s.chain, s.gain, s.tap, s.feed, s.closing)

	go s.run()

	return s, nil
}

// buildPost compiles the post-ring half of a pipeline into configured effects.
// Building is also validation: resolving each kind and coercing each parameter
// is what rejects an unknown kind or parameter. The effects are Configured for
// the canonical post-ring format and Reset before they are handed to a caller,
// so the first buffer an effect sees is never stale state from a previous
// install.
func buildPost(p dsp.Pipeline) ([]dsp.Effect, error) {
	if err := validateSpecIDs(p.Post); err != nil {
		return nil, err
	}
	effects, err := dsp.BuildPost(p)
	if err != nil {
		return nil, err
	}
	for _, e := range effects {
		if _, err := e.Configure(canonical); err != nil {
			return nil, fmt.Errorf("dsp: %s configure: %w", e.Name(), err)
		}
		if err := e.Reset(); err != nil {
			return nil, fmt.Errorf("dsp: %s reset: %w", e.Name(), err)
		}
	}

	return effects, nil
}

// validateSpecIDs rejects a pipeline with two stages sharing a non-empty ID. An
// empty ID means the stage is not addressable by the editor and is allowed, but
// a duplicate would make RemoveEffect and liveEffect address different stages:
// the first would drop both, the second would write the wrong one.
func validateSpecIDs(specs []dsp.Spec) error {
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if spec.ID == "" {
			continue
		}
		if seen[spec.ID] {
			return fmt.Errorf("dsp: duplicate effect stage ID %q", spec.ID)
		}
		seen[spec.ID] = true
	}

	return nil
}

// ValidatePipeline reports whether every post-ring stage can be built. It is
// the same check ApplyPipeline runs, exposed so a caller can reject a preset
// without applying it. The Pre half is never built here: it belongs to the
// streamer, which does not exist yet at validation time.
func ValidatePipeline(p dsp.Pipeline) error {
	_, err := buildPost(p)

	return err
}

// EffectSchema returns the parameter schema of the implementation automatic
// selection would build for a kind, so a UI can render controls without
// instantiating an effect. An unknown kind is an error, never an empty schema.
func (s *Session) EffectSchema(kind string) ([]dsp.Param, error) {
	return dsp.Default.Schema(kind)
}

// EffectKindNames lists the registered effect kinds, sorted, so a UI can offer
// the stages this build supports without hard-coding them. It is the plain name
// list; EffectKinds returns the detailed per-implementation list an editor
// needs.
func (s *Session) EffectKindNames() []string {
	return dsp.Default.Kinds()
}

// validateDecoderName runs the caller's override when one is set, so a test can
// drive fake codecs without touching the process-wide registry, and the real
// check otherwise. One rule, two sources.
func (c Config) validate(name string, real func(string) error) error {
	if c.validateDecoder != nil {
		return c.validateDecoder(name)
	}

	return real(name)
}

// Settings returns the mutable configuration currently in effect.
func (s *Session) Settings() Settings {
	decoder, backend, mode := s.runtime.snapshot()

	return Settings{
		Volume:    s.gain.Volume(),
		Decoder:   decoder,
		Backend:   backend,
		ProbeMode: mode,
		Pipeline:  s.runtime.Pipeline(),
	}
}

// ApplyPipeline validates every post-ring stage before applying any of them,
// matching SetSettings: a pipeline with an unknown kind or an unknown parameter
// is rejected whole and leaves the chain in force untouched. Validation happens
// on the caller's goroutine: building an effect is bounded and never touches
// the device, just as validation of a decoder name never opens a file.
//
// The accepted effects are handed to the control loop, which publishes them with
// an atomic swap. A successful call therefore means the pipeline is accepted,
// not that the audio path has already switched to it; the PipelineChanged event
// confirms the install.
func (s *Session) ApplyPipeline(p dsp.Pipeline) error {
	effects, err := buildPost(p)
	if err != nil {
		return err
	}
	s.installPipeline(effects, p, nil)

	return nil
}

// Pipeline returns the post-ring effect chain in force. The returned Pipeline
// is a copy: mutating it cannot reach the engine, and ApplyPipeline must be used
// to change anything.
func (s *Session) Pipeline() dsp.Pipeline { return s.runtime.Pipeline() }

// SetSettings validates every field before applying any of them, so a partial
// update is impossible: either the whole set is accepted or nothing changes.
// Validation runs here, on the caller's goroutine, so a bad value is reported
// as an error instead of surfacing as a Failed event on the next track.
//
// Pipeline is validated as part of the same all-or-nothing rule. It differs
// from the other fields in that it is applied by the control loop rather than
// here, because installing a chain must not race a build; the whole set is
// still rejected before anything is written.
func (s *Session) SetSettings(next Settings) error {
	if err := ValidateVolume(next.Volume); err != nil {
		return err
	}
	if err := s.cfg.validate(next.Decoder, ValidateDecoder); err != nil {
		return err
	}
	if err := ValidateBackend(next.Backend); err != nil {
		return err
	}
	if err := ValidateProbeMode(next.ProbeMode); err != nil {
		return err
	}

	// Build the effects before anything is written, so an unbuildable stage
	// rejects the whole update. A nil Post half is an empty chain, which is a
	// valid request: it clears the chain.
	effects, err := buildPost(next.Pipeline)
	if err != nil {
		return err
	}

	// Everything is valid, so apply. Volume is the only one that takes effect
	// immediately; decoder and probe mode are read per track, and backend is
	// read when a device is built.
	s.gain.SetVolume(next.Volume)
	s.runtime.set(next.Decoder, next.Backend, next.ProbeMode)
	s.installPipeline(effects, next.Pipeline, nil)

	return nil
}

// stageKeys names the stages a pipeline describes, in order. They are stored
// beside a built chain so the editor can resolve a stage by identity rather
// than by index into a generation.
func stageKeys(p dsp.Pipeline) []stageKey {
	keys := make([]stageKey, 0, len(p.Post))
	for _, spec := range p.Post {
		keys = append(keys, stageKey{id: spec.ID, kind: spec.Kind, impl: spec.Impl})
	}

	return keys
}

// installPipeline records a validated pipeline and, when it actually changed,
// hands its prebuilt effects to the control loop to publish. Building the
// effects already happened on the caller's goroutine, so the control loop only
// performs an atomic chain swap and never blocks on effect construction. An
// identical pipeline is a no-op: the effects are discarded and nothing is
// emitted, so a settings edit that leaves the chain alone cannot reset it.
func (s *Session) installPipeline(effects []dsp.Effect, p dsp.Pipeline, installed chan struct{}) {
	keys := stageKeys(p)
	changed, gen := s.runtime.setPipelineIfChanged(p, effects, keys)
	if !changed {
		// Nothing to publish, so a waiting caller must still be released.
		s.signalInstalled(installed)

		return
	}
	if !s.enqueue(command{kind: cmdApplyPipeline, effects: effects, keys: keys, pipelineGen: gen, installed: installed}) {
		// The session closed before the command could run; release the waiter
		// rather than leave it blocked.
		s.signalInstalled(installed)
	}
}

// applyPipelineWait is ApplyPipeline plus a wait for the control loop to publish
// the chain. The effect editor uses it so a stage it just added is already live
// when the call returns, which is what lets the caller set a parameter on it
// immediately instead of polling. The wait is bounded by the control loop, which
// only does bounded work, and by Close, so it cannot block a shutdown.
func (s *Session) applyPipelineWait(p dsp.Pipeline) error {
	effects, err := buildPost(p)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	s.installPipeline(effects, p, done)
	select {
	case <-done:
	case <-s.closing:
	}

	return nil
}

// Settings is the mutable half of a Session's configuration: the values a
// caller may change while playback is running. Everything else in Config is
// read once by New and cannot change afterwards.
//
// Three of these take effect at different moments, which callers must know:
//
//   - Volume applies immediately.
//   - Decoder and ProbeMode are read when the next track is built, so the
//     current track keeps what it started with.
//   - Backend is read when a device is built. An already-open device keeps
//     playing on the old backend, so the change lands on the next Play that
//     has to build one.
//   - Pipeline applies to the running audio as soon as the control loop
//     accepts it, which PipelineChanged confirms. Only the Post half is used;
//     Pre belongs to the streamer.
type Settings struct {
	Volume    float64
	Decoder   string
	Backend   string
	ProbeMode core.DurationMode

	// Pipeline is the post-ring effect chain. Zero is an empty chain, which is
	// today's behaviour: only the master gain runs.
	Pipeline dsp.Pipeline
}

// probeWithDefaultRegistry asks the process-wide decoder registry for a
// duration probe.
func probeWithDefaultRegistry(path string, opts decode.ProbeOptions) (core.StreamInfo, error) {
	p, ok := decode.Default.Probe(path)
	if !ok {
		return core.StreamInfo{Format: canonical, TotalFrames: -1}, ErrNoProber
	}

	return p.Probe(path, opts)
}

// providerFor returns the first configured provider that claims ref. The bool
// is false when nothing matches, including when the list is empty; callers tell
// "no providers configured" (legacy path) from "providers configured but
// nothing matched" (ErrNoProvider) by checking len(cfg.Providers).
func (s *Session) providerFor(ref string) (provider.AudioProvider, bool) {
	for _, p := range s.cfg.Providers {
		if p.Match(ref) {
			return p, true
		}
	}

	return nil, false
}

// swapRefused reports the path of a track whose decoder cannot be swapped, or
// the empty string when a swap is allowed. A provider ref carries an opaque
// Opener, so there is no way to rebuild it with a different decoder; only the
// session's own decoder path can. The flag is set by activate, so a swap during
// the build window is allowed and simply records the preference.
func (s *Session) swapRefused() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.v.providerRef {
		return s.v.path
	}

	return ""
}

// Snapshot reports the controller state. It is safe to call from any
// goroutine and is cheap enough to poll every frame.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	v := s.v
	s.mu.Unlock()

	pos := v.lastPos
	var stats stream.Stats
	if live := s.provider.current(); live != nil {
		// Position is converted through the streamer's one helper, so no
		// caller can pair a native-rate count with the output clock.
		pos = stream.FramesToDuration(live.Position())
		stats = live.Stats()
	}
	// The chain delays what is heard behind what the streamer has read, so the
	// reported position is the heard position. Latency is control-side only, so
	// a scripted effect's store lock is fine; the clamp stops a negative time
	// when the chain's latency exceeds the position at the start of a track.
	if latency := s.chain.Latency(); latency > 0 {
		if pos > latency {
			pos -= latency
		} else {
			pos = 0
		}
	}

	return Snapshot{
		State:         v.state,
		Path:          v.path,
		Meta:          v.meta,
		Position:      pos,
		Duration:      v.duration,
		Volume:        s.gain.Volume(),
		Format:        canonical,
		Backend:       s.runtime.backendName(),
		QueueIndex:    v.queueIndex,
		QueueLen:      v.queueLen,
		Stats:         stats,
		Decoder:       v.decoder,
		Parser:        v.parser,
		DroppedEvents: s.events.droppedCount(),
	}
}

// Events is the stream of controller events. It never blocks the engine: a
// producer that outruns this channel costs the oldest event, counted in
// Snapshot.DroppedEvents.
func (s *Session) Events() <-chan Event { return s.events.channel() }

// Play replaces the queue with one track and starts it. ref is a provider
// reference when a provider claims it, and a filesystem path otherwise.
func (s *Session) Play(path string) error {
	if path == "" {
		return ErrEmptyPath
	}
	s.enqueue(command{kind: cmdPlay, path: path})

	return nil
}

// PlayQueue replaces the queue and starts at the first track. Each entry is a
// provider reference when a provider claims it, and a filesystem path
// otherwise.
func (s *Session) PlayQueue(paths []string) error {
	if len(paths) == 0 {
		return ErrEmptyQueue
	}
	s.enqueue(command{kind: cmdPlayQueue, paths: append([]string(nil), paths...)})

	return nil
}

// PlayIndex starts the queue entry at index without replacing the queue, which
// is what a click on a row in a track list means: jump there, keep the rest.
// The bound is checked here so an out-of-range jump is an immediate error
// rather than a silent no-op, and the control loop re-checks it because the
// queue can change between the two.
func (s *Session) PlayIndex(index int) error {
	s.mu.Lock()
	out := index < 0 || index >= len(s.queue)
	s.mu.Unlock()
	if out {
		return fmt.Errorf("%w: %d", ErrIndexOutOfRange, index)
	}
	s.enqueue(command{kind: cmdPlayIndex, index: index})

	return nil
}

// InsertQueue inserts refs into the queue at index without touching the track
// that is playing. An index at or past the end appends. Insertion is what
// "play next" means: the current track keeps sounding, and the inserted refs
// are what the engine plays when it advances.
//
// The live track is not preserved by re-seating the queue; it is preserved
// because the queue and the current track's index are the only things that
// change. Nothing re-opens the current streamer, so playback is continuous.
func (s *Session) InsertQueue(index int, refs []string) error {
	if err := validateRefs(refs); err != nil {
		return err
	}
	s.enqueue(command{kind: cmdInsertQueue, index: index, paths: append([]string(nil), refs...)})

	return nil
}

// InsertQueueAndPlay inserts refs like InsertQueue and then starts the first
// inserted ref. With nothing playing that is simply "play this now"; with a
// track sounding it would cut it off, so the live case is refused with
// ErrNoLiveTrack and the caller keeps the refs to append instead. It is one
// command rather than an insert plus a PlayIndex because PlayIndex validates
// its index synchronously, before the control loop has grown the queue.
func (s *Session) InsertQueueAndPlay(index int, refs []string) error {
	if err := validateRefs(refs); err != nil {
		return err
	}

	// The state is read through the accessor rather than s.v directly: this
	// runs on the caller's goroutine, and the control loop writes s.v under the
	// same lock, so an unguarded read would be a data race. The control loop
	// re-checks before starting anything, so this is only the fast refusal.
	if s.hasLiveTrack() {
		return ErrNoLiveTrack
	}

	s.enqueue(command{kind: cmdInsertQueue, index: index, paths: append([]string(nil), refs...), play: true})

	return nil
}

// hasLiveTrack reports whether a track is loaded, playing or paused. It is the
// one definition of "the insert would cut something off".
func (s *Session) hasLiveTrack() bool {
	st := s.state()

	return st == StatePlaying || st == StatePaused
}

// validateRefs applies the shared input rule: at least one ref, none empty.
func validateRefs(refs []string) error {
	if len(refs) == 0 {
		return ErrEmptyQueue
	}
	for _, ref := range refs {
		if ref == "" {
			return ErrEmptyPath
		}
	}

	return nil
}

// Next advances within the queue. Past the last track it stops.
func (s *Session) Next() error {
	s.enqueue(command{kind: cmdNext})

	return nil
}

// Prev goes back one track; on the first track it restarts the current one.
func (s *Session) Prev() error {
	s.enqueue(command{kind: cmdPrev})

	return nil
}

// Queue returns a copy of the current queue.
func (s *Session) Queue() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.queue...)
}

// Providers lists the names of the configured providers in the order they are
// tried. It is empty for the default local-only setup, which has no explicit
// provider list.
func (s *Session) Providers() []string {
	names := make([]string, 0, len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		names = append(names, p.Name())
	}

	return names
}

// Pause stops the device. It is ignored unless the session is playing.
func (s *Session) Pause() error {
	s.enqueue(command{kind: cmdPause})

	return nil
}

// Resume restarts a paused device. It is ignored unless the session is paused.
func (s *Session) Resume() error {
	s.enqueue(command{kind: cmdResume})

	return nil
}

// Stop ends playback and keeps the queue. The device stays open, so the next
// Play reuses it.
func (s *Session) Stop() error {
	s.enqueue(command{kind: cmdStop})

	return nil
}

// Seek repositions the current track once the device has been paused, so the
// device never reads a half-flushed ring.
func (s *Session) Seek(d time.Duration) error {
	if d < 0 {
		return ErrNegativeSeek
	}
	s.enqueue(command{kind: cmdSeek, pos: d})

	return nil
}

// SwapDecoder forces the named decoder for the current track, reopening it at
// the position playback has reached. Empty means automatic selection. A bad
// name is rejected synchronously and changes nothing; an engine failure
// surfaces as a Failed event while the old decoder keeps playing.
//
// A track owned by a provider cannot switch decoder: the provider supplied an
// opaque Opener, so the session has no way to rebuild it with the named codec.
// That is refused synchronously with ErrProviderNoDecoderSwap. A local track is
// unaffected.
func (s *Session) SwapDecoder(name string) error {
	if err := s.cfg.validate(name, ValidateDecoder); err != nil {
		return err
	}
	if path := s.swapRefused(); path != "" {
		return fmt.Errorf("%w: %s", ErrProviderNoDecoderSwap, path)
	}
	s.enqueue(command{kind: cmdSwapDecoder, name: name})

	return nil
}

// SwapBackend forces the named playback backend for the current device,
// reopening it against the same stable provider. Empty means the default. An
// unknown name is rejected synchronously; an engine failure surfaces as a
// Failed event while the old device keeps playing.
func (s *Session) SwapBackend(name string) error {
	if err := ValidateBackend(name); err != nil {
		return err
	}
	s.enqueue(command{kind: cmdSwapBackend, name: name})

	return nil
}

// SetVolume changes the post-ring gain. It is atomic and takes effect on the
// next buffer, so it is safe to call while audio is running.
func (s *Session) SetVolume(v float64) { s.gain.SetVolume(v) }

// Tap is the real-time audio feed a visualizer reads. The engine writes mono
// downmixed frames after the gain and before the device, so a consumer sees
// exactly what is heard.
type Tap interface {
	// Read copies up to len(dst) mono frames in time order. It never blocks
	// and reports zero when nothing is buffered.
	Read(dst []float32) int
}

// Tap returns the session's visualizer feed. The feed becomes live on the
// first Read, so holding a Tap without consuming it costs the audio path
// nothing. The returned Tap stays valid across tracks and after Stop.
func (s *Session) Tap() Tap {
	return s.tap
}

// Close stops every goroutine and releases the device and streamer. It is
// idempotent and safe to call from any goroutine.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		close(s.closing)
		s.cancel()
		<-s.stopped
	})

	return nil
}

// enqueue appends a command and wakes the control loop. The wake is a
// non-blocking signal: a pending wake is as good as a new one because the loop
// drains the whole inbox.
//
// It reports whether the command was accepted. A closed session drops it, and a
// caller that would wait for the command to run needs to know so it does not
// wait forever.
func (s *Session) enqueue(c command) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return false
	}
	s.inbox = append(s.inbox, c)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}

	return true
}

// signalInstalled closes a command's installed channel, if it has one, to tell
// a waiting editor caller that the chain is live.
func (s *Session) signalInstalled(ch chan struct{}) {
	if ch != nil {
		close(ch)
	}
}

// run is the one control goroutine. It only ever does bounded work: the slow
// steps arrive as results on channels.
func (s *Session) run() {
	defer close(s.stopped)

	ticker := time.NewTicker(devicePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.closing:
			s.shutdown()

			return
		case <-s.wake:
			s.runCommands()
		case ev := <-s.feed:
			s.handleFeed(ev)
		case res := <-s.openCh:
			s.handleOpen(res)
		case res := <-s.probeCh:
			s.handleProbe(res)
		case res := <-s.metaCh:
			s.handleMeta(res)
		case res := <-s.seekCh:
			s.handleSeek(res)
		case req := <-s.seamlessCh:
			s.requestSeamless(req)
		case <-ticker.C:
			s.pollDevice()
		}
	}
}

// runCommands applies the inbox in order. Commands are cheap; nothing here
// waits on a file or a device, which is what keeps the loop responsive.
func (s *Session) runCommands() {
	s.mu.Lock()
	cmds := s.inbox
	s.inbox = nil
	s.mu.Unlock()

	for _, c := range cmds {
		s.apply(c)
	}
}

func (s *Session) apply(c command) {
	switch c.kind {
	case cmdPlay:
		s.setQueue([]string{c.path})
		s.startIndex(0)
	case cmdPlayQueue:
		s.setQueue(c.paths)
		s.startIndex(0)
	case cmdPlayIndex:
		// Re-check the range here: the queue is the control loop's, and a
		// PlayQueue processed since the synchronous check may have replaced it
		// with a shorter one.
		if c.index >= 0 && c.index < len(s.queue) {
			s.startIndex(c.index)
		}
	case cmdInsertQueue:
		s.insertQueue(c.index, c.paths, c.play)
	case cmdNext:
		s.next()
	case cmdPrev:
		s.prev()
	case cmdPause:
		if s.state() == StatePlaying && s.device != nil {
			_ = s.device.Pause()
			s.setState(StatePaused)
		}
	case cmdResume:
		if s.state() == StatePaused && s.device != nil {
			// A device installed by a paused backend swap was never started, so
			// Resume has to Start it; on a device that is already playing this
			// is the ordinary continue.
			if s.deviceStarted {
				_ = s.device.Resume()
			} else if err := s.device.Start(); err != nil {
				s.fail(err, true)

				return
			} else {
				s.deviceStarted = true
			}
			s.setState(StatePlaying)
		}
	case cmdStop:
		if st := s.state(); st == StatePlaying || st == StatePaused {
			s.stopToStopped()
		}
	case cmdSeek:
		s.requestSeek(c.pos)
	case cmdSwapDecoder:
		s.requestSwapDecoder(c.name)
	case cmdSwapBackend:
		s.requestSwapBackend(c.name)
	case cmdApplyPipeline:
		s.installChain(c.effects, c.pipelineGen, c.keys)
		s.signalInstalled(c.installed)
	}
}

// stageKey identifies a stage by what it is, not by where it sits. Two accepted
// changes can leave a description and a live chain from different generations,
// and an index would then pair one stage's identity with another's effect. The
// key is matched instead, and the kind and impl are part of it so a caller that
// reuses an ID for a different effect cannot be mis-paired either.
type stageKey struct {
	id   string
	kind string
	impl string
}

// installedChain is the live effect list and the pipeline generation it was
// built for, published as one atomic unit. Keeping the two together is what
// makes the editor's pairing sound: reading a generation and a list separately
// could catch the moment between the two stores.
type installedChain struct {
	effects []dsp.Effect
	gen     uint64
	keys    []stageKey
}

// effect returns the live effect for a stage, if this chain holds it.
func (c *installedChain) effect(spec dsp.Spec) (dsp.Effect, bool) {
	for i, k := range c.keys {
		if k.id == spec.ID && k.kind == spec.Kind && k.impl == spec.Impl && i < len(c.effects) {
			return c.effects[i], true
		}
	}

	return nil, false
}

// publishChain swaps the live effect list and its generation as one unit. It is
// the whole audio-path share of a pipeline change: Chain.Set is an atomic
// pointer store, so installing never builds anything here and never blocks. The
// audio thread picks the new list up on its next buffer.
//
// The pair is stored as one pointer so a concurrent reader sees the list and
// its generation together, never one change's list with another's generation.
func (s *Session) publishChain(effects []dsp.Effect, gen uint64, keys []stageKey) {
	s.chain.Set(effects)
	s.installed.Store(&installedChain{effects: effects, gen: gen, keys: keys})
}

// installChain publishes a built effect list and announces it. It is the
// control loop's answer to an ApplyPipeline command.
func (s *Session) installChain(effects []dsp.Effect, gen uint64, keys []stageKey) {
	s.publishChain(effects, gen, keys)
	// The effects are live now, so they are no longer pending. Clearing under
	// the same lock that guards the generation keeps the two views consistent.
	s.runtime.clearPending(gen)
	s.emit(PipelineChanged{Stages: len(effects)})
}

// resetChain clears the effect state after the device has been parked, so a
// discontinuity in the stream is not blended across by a stateful effect. The
// caller must have parked the device first: Reset touches the same filter state
// Process reads, and parking is what guarantees Process is not running.
func (s *Session) resetChain() {
	_ = s.chain.Reset()
}

// next advances the queue, or stops at the end. It is the command path; the
// natural end path shares startIndex but not the stop decision, because a
// track that merely ended has no more tracks by definition.
func (s *Session) next() {
	if len(s.queue) == 0 {
		return
	}
	if s.index+1 < len(s.queue) {
		s.startIndex(s.index + 1)

		return
	}
	s.stopToStopped()
}

// prev steps back, restarting the first track instead of leaving the queue.
func (s *Session) prev() {
	if len(s.queue) == 0 {
		return
	}
	if s.index > 0 {
		s.startIndex(s.index - 1)

		return
	}
	// Already on the first track: restart it if one is loaded, otherwise
	// start the queue. A UI Prev at the top of a list means "play this again".
	if s.provider.current() == nil {
		s.startIndex(0)

		return
	}
	s.requestSeek(0)
}

// setQueue replaces the queue under the lock Queue reads under. The control
// goroutine is the only writer, but Queue is public and may be called from any
// goroutine, so the write must be synchronised.
func (s *Session) setQueue(paths []string) {
	s.mu.Lock()
	s.queue = paths
	s.mu.Unlock()
}

// insertQueue splices refs into the queue at index and republishes the length.
// It deliberately does not touch the current track: no detach, no new open, no
// seek. The streamer keeps feeding the device, so a track that is playing
// carries on exactly where it was, and the inserted refs are simply what the
// engine reaches when it advances.
//
// The current index is left alone when the insertion lands after it, because
// the current track has not moved. Inserting at or before the current index
// would shift the current track, so it is treated as an append: the engine has
// no way to re-seat the live streamer's position, and pretending otherwise
// would make the next advance skip or repeat a track.
//
// play starts the first inserted ref instead of leaving it queued. The decision
// is re-made here, on the control loop that owns the queue: the caller's check
// ran on its own goroutine, so a track may have started since and a live track
// must never be cut off by an insert.
func (s *Session) insertQueue(index int, refs []string, play bool) {
	if len(refs) == 0 {
		return
	}
	at := index
	if at < 0 || at > len(s.queue) || at <= s.index {
		at = len(s.queue)
	}

	next := make([]string, 0, len(s.queue)+len(refs))
	next = append(next, s.queue[:at]...)
	next = append(next, refs...)
	next = append(next, s.queue[at:]...)

	s.setQueue(next)
	s.updateView(func(v *view) { v.queueLen = len(next) })

	if play && !s.hasLiveTrack() {
		s.startIndex(at)
	}
}

// startIndex tears the current track down and opens index asynchronously.
func (s *Session) startIndex(index int) {
	if index < 0 || index >= len(s.queue) {
		return
	}
	path := s.queue[index]
	s.index = index
	s.detach()
	s.seq++

	s.updateView(func(v *view) {
		v.path = path
		v.meta = meta.Meta{Path: path}
		v.duration = 0
		v.lastPos = 0
		v.decoder = ""
		v.parser = ""
		// Unknown until the provider resolves the ref: a swap during the build
		// window just records the preference, so false here is safe.
		v.providerRef = false
		v.queueIndex = index
		v.queueLen = len(s.queue)
	})

	s.openTrack(s.seq, index, path)
}

// detach drops the current stream and parks the device. The device is not
// closed: it is the resource the next track reuses.
func (s *Session) detach() {
	s.retireLive()
	s.park()
}

// park stops the device and drops whatever it had queued. Pause alone is not
// enough: the backend keeps the audio it already read, so a later resume would
// replay part of the position being left behind. Every path that abandons a
// position (track change, stop, seek) parks for that reason.
func (s *Session) park() {
	if s.device == nil || !s.deviceStarted {
		return
	}
	_ = s.device.Pause()
	_ = s.device.Flush()
}

// retireLive stops the provider from reading the current streamer and closes
// it. Setting the provider to nil first means an in-flight read returns
// silence rather than the ErrClosed that a planned close produces.
func (s *Session) retireLive() {
	s.inFlight = false
	// Any target that has not started belongs to the stream being retired, so
	// it is dropped rather than applied to the next track. An in-flight worker
	// is left alone; its stale result is discarded in handleSeek. A parked
	// source upgrade is dropped for the same reason, and its download is
	// cancelled because there is no track left to upgrade.
	s.seekPending = nil
	s.pendingSeamless = nil
	if s.upgradeCancel != nil {
		s.upgradeCancel()
		s.upgradeCancel = nil
	}
	if s.live == nil {
		return
	}
	pos := stream.FramesToDuration(s.live.Position())
	s.updateView(func(v *view) { v.lastPos = pos })

	s.provider.setCurrent(nil)
	_ = s.live.Close()
	s.live = nil
}

// openTrack builds the pipeline for one file off the control goroutine. The
// result is buffered, so a worker never blocks on the control loop; the
// closing case covers a send after shutdown.
func (s *Session) openTrack(seq uint64, index int, path string) {
	s.workerWG.Add(1)

	go func() {
		defer s.workerWG.Done()

		res := s.build(seq, index, path)
		select {
		case s.openCh <- res:
		case <-s.closing:
			if res.streamer != nil {
				_ = res.streamer.Close()
			}
		}
	}()
}

// build creates and starts a streamer over the path, and builds the post-ring
// effect chain that goes with it. The decoder's stream info is captured here
// because only the decoder knows it. An opener that runs after Close is
// refused, so shutdown cannot be extended by a new file open.
//
// The chain is built on this worker, not the control loop, because building an
// effect can be arbitrarily expensive and the control loop must stay
// responsive. The pipeline snapshot and its generation are captured before the
// effects are built, so the loop can tell whether a change landed while this
// ran and prefer the newer one.
//
// A reference claimed by a non-local provider is resolved here too, on this
// worker, because Open may block on the network. LocalAudio is transparent: it
// is treated as the legacy path so the decoder preference and the existing
// openDecoder seam keep working unchanged.
func (s *Session) build(seq uint64, index int, path string) openResult {
	select {
	case <-s.closing:
		return openResult{seq: seq, index: index, path: path, err: ErrClosed}
	default:
	}

	pipeline, gen := s.runtime.pipelineSnapshot()
	effects, err := buildPost(pipeline)
	if err != nil {
		return openResult{seq: seq, index: index, path: path, err: err}
	}
	// Record the stage IDs the built effects correspond to. The chain the
	// editor resolves against is identified by these IDs, not by the generation
	// alone: a generation can be read from a stale snapshot while the chain is
	// already current, so the IDs are what actually say which stages are live.
	keys := stageKeys(pipeline)

	var src *provider.Source
	if len(s.cfg.Providers) > 0 {
		p, ok := s.providerFor(path)
		if !ok {
			return openResult{seq: seq, index: index, path: path, err: fmt.Errorf("%w: %s", ErrNoProvider, path)}
		}
		source, err := p.Open(s.ctx, path)
		if err != nil {
			return openResult{seq: seq, index: index, path: path, err: err}
		}
		// Source.Local means the provider wrapped a filesystem source and the
		// session must keep its own opener, resolver and prober. Identity is
		// never inspected, so LocalAudio, &LocalAudio{} and any wrapper that
		// sets Local are all transparent.
		if !source.Local {
			// A Source with no Opener cannot produce audio, and passing it on
			// would panic the producer goroutine. The contract requires one, so
			// a violation is a provider bug surfaced as a track failure.
			if source.Opener == nil {
				return openResult{seq: seq, index: index, path: path, err: fmt.Errorf("%w: %s returned no Opener", ErrProviderNoOpener, p.Name())}
			}
			src = &source
		}
	}

	var info core.StreamInfo
	var decoderName, parserName string
	open := func(stop <-chan struct{}) (decode.Decoder, error) {
		select {
		case <-s.closing:
			return nil, ErrClosed
		default:
		}

		if src != nil {
			// The provider owns how the bytes are produced. Its Opener is
			// called again by the streamer on the reopen-and-discard seek
			// fallback and on a decoder swap, which is why it is a factory.
			d, err := src.Opener(stop)
			if err != nil {
				return nil, err
			}
			// A decoder plus a nil error but no decoder would panic Info below,
			// so the contract violation is a failed track instead.
			if d == nil {
				return nil, fmt.Errorf("%w: opener returned a nil decoder", ErrProviderNoDecoder)
			}
			info = d.Info()
			decoderName, parserName = decode.Describe(d)

			return d, nil
		}

		// Read the preference here, not at New: changing it must affect the
		// next track, and this is the moment the next track's decoder is
		// chosen. The current track keeps the decoder it was built with.
		d, err := s.cfg.openDecoder(s.runtime.decoderName(), path)
		if err != nil {
			return nil, err
		}
		info = d.Info()
		decoderName, parserName = decode.Describe(d)

		return d, nil
	}

	st, err := stream.New(open, stream.Config{
		RingFrames:          s.cfg.RingFrames,
		ForwardSeekFallback: s.cfg.Experimental.SourceUpgrade,
	})
	if err != nil {
		return openResult{seq: seq, index: index, path: path, err: err}
	}
	if err := st.Start(s.ctx); err != nil {
		_ = st.Close()

		return openResult{seq: seq, index: index, path: path, err: err}
	}

	res := openResult{
		seq: seq, index: index, path: path,
		streamer: st, effects: effects, pipelineGen: gen, keys: keys,
		info: info, decoder: decoderName, parser: parserName,
	}
	if src != nil {
		res.providerMeta = true
		res.meta = src.Meta
		res.probe = src.Probe
		// The upgrade is opt-in. With the flag off nothing is carried and the
		// source behaves exactly as it did before this path existed.
		if s.cfg.Experimental.SourceUpgrade {
			res.upgrade = src.Upgrade
		}
	}

	return res
}

// handleOpen installs a finished build, or discards it when the queue has
// already moved on.
func (s *Session) handleOpen(res openResult) {
	if res.seq != s.seq {
		// The queue moved on while this build ran, so the result is stale. A
		// successful build owns a live streamer that must be closed; a failed
		// one has nothing to release.
		if res.streamer != nil {
			_ = res.streamer.Close()
		}

		return
	}
	if res.err != nil {
		s.fail(res.err, false)

		return
	}

	s.activate(res)
}

// activate points the device at the new streamer. The device is opened lazily
// on the first track and reused from then on.
func (s *Session) activate(res openResult) {
	if s.device == nil {
		// A backend change is read here, at the only moment a Device is built.
		// An already-open device keeps its backend: a Device cannot be
		// re-bound, so the switch lands on the next build, not immediately.
		_, backend, _ := s.runtime.snapshot()
		dev, err := s.cfg.newDevice(backend)
		if err != nil {
			_ = res.streamer.Close()
			s.fail(err, false)

			return
		}
		if err := dev.Open(canonical, s.provider); err != nil {
			_ = dev.Close()
			_ = res.streamer.Close()
			s.fail(err, false)

			return
		}
		s.device = dev
	}

	s.live = res.streamer
	// Install the effects the worker built for this track before the provider
	// is pointed at the streamer, so the first buffer the device reads already
	// has the chain. If a pipeline change landed while this build ran, its
	// generation is newer and its chain is already in force; installing this
	// older list would silently undo the change, so it is dropped.
	if res.pipelineGen == s.runtime.pipelineGenNow() {
		s.publishChain(res.effects, res.pipelineGen, res.keys)
	}
	s.provider.setCurrent(res.streamer)
	s.inFlight = true

	s.updateView(func(v *view) {
		v.path = res.path
		if res.providerMeta {
			// The provider described the track. Its Meta.Path and Stream are
			// overwritten with what the session knows for sure: the ref is the
			// identity the queue uses, and only the decoder can report the true
			// stream shape, so the provider's guess is never trusted for those.
			m := res.meta
			m.Path = res.path
			m.Stream = res.info
			v.meta = m
		} else {
			v.meta = meta.Meta{Path: res.path, Stream: res.info}
		}
		v.duration = 0
		v.decoder = res.decoder
		v.parser = res.parser
		v.providerRef = res.providerMeta
		v.queueIndex = res.index
		v.queueLen = len(s.queue)
	})

	if !s.deviceStarted {
		if err := s.device.Start(); err != nil {
			s.fail(err, true)

			return
		}
		s.deviceStarted = true
	} else if err := s.device.Resume(); err != nil {
		s.fail(err, true)

		return
	}

	s.setState(StatePlaying)
	s.emit(TrackChanged{Index: res.index, Path: res.path})
	s.enrich(res)

	// Start the upgrade only after the track is actually playing: a build that
	// failed to start the device must not launch a download for it. The context
	// is scoped to this track rather than the session, so an upgrade that is
	// still waiting when the track retires is cancelled instead of downloading
	// until Close; retireLive owns the cancel.
	if res.upgrade != nil {
		upgradeCtx, cancel := context.WithCancel(s.ctx)
		s.upgradeCancel = cancel
		s.startUpgrade(upgradeCtx, res)
	}
}

// startUpgrade runs a Source.Upgrade off the control loop and hands the result
// back as a request. A failure is silent by design: the upgrade is a best-effort
// optimisation over a stream that is already playing, so it is not a track
// failure. It must not emit Failed and must not close the streamer; the track
// keeps the decoder it was built with. workerWG tracks it so Close waits for a
// download that is still running, and ctx is cancelled on Close and on a track
// change.
func (s *Session) startUpgrade(ctx context.Context, res openResult) {
	s.workerWG.Add(1)

	go func() {
		defer s.workerWG.Done()

		open, err := res.upgrade(ctx)
		if err != nil || open == nil {
			return
		}
		wrapped, labels := s.seamlessOpener(open)
		req := &seekRequest{
			seq:    res.seq,
			stream: res.streamer,
			kind:   requestSwapSeamless,
			open:   wrapped,
			labels: labels,
		}

		select {
		case s.seamlessCh <- req:
		case <-s.closing:
		}
	}()
}

// enrich starts the asynchronous metadata and duration lookups. Play never
// waits for either: the snapshot reports an unknown duration until the probe
// lands. These workers own no engine resource, so Close does not wait for
// them; their channel sends are released by the closing signal.
//
// A provider ref is described by its Source, so the local resolver is skipped:
// meta.Default() would read a file that does not exist and invent a title from
// the ref string. The duration likewise comes from Source.Probe when one was
// supplied; without it the total stays unknown, because the session's own
// prober only understands a local path.
func (s *Session) enrich(res openResult) {
	go func() {
		var info core.StreamInfo
		var err error
		switch {
		case res.providerMeta && res.probe != nil:
			info, err = res.probe(s.runtime.probeDurationMode())
		case res.providerMeta:
			info = core.StreamInfo{Format: canonical, TotalFrames: -1}
		default:
			mode := s.runtime.probeDurationMode()
			info, err = s.cfg.probeStream(res.path, decode.ProbeOptions{Duration: mode})
		}
		select {
		case s.probeCh <- probeResult{seq: res.seq, index: res.index, info: info, err: err}:
		case <-s.closing:
		}
	}()

	go func() {
		if res.providerMeta {
			// Path is the ref the queue holds, not whatever the provider put
			// in Meta: it is the track's identity in the session, and handleMeta
			// replaces the whole meta, so leaving it out here would erase the
			// path activate already published.
			m := res.meta
			m.Path = res.path
			m.Stream = res.info
			select {
			case s.metaCh <- metaResult{seq: res.seq, index: res.index, m: &m}:
			case <-s.closing:
			}

			return
		}

		m, err := s.cfg.Resolver.Resolve(s.ctx, res.path)
		select {
		case s.metaCh <- metaResult{seq: res.seq, index: res.index, m: m, err: err}:
		case <-s.closing:
		}
	}()
}

// handleProbe records a duration for the track it was started for. A probe for
// a track that is no longer current is discarded.
func (s *Session) handleProbe(res probeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if res.seq != s.seq || res.index != s.v.queueIndex {
		return
	}

	s.v.duration = res.info.Duration()
}

// handleMeta records resolved tags, merging in the stream info the decoder
// reported: only the decoder can describe the stream truthfully.
func (s *Session) handleMeta(res metaResult) {
	if res.err != nil || res.m == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if res.seq != s.seq || res.index != s.v.queueIndex {
		return
	}

	merged := *res.m
	merged.Stream = s.v.meta.Stream
	s.v.meta = merged
}

// handleFeed reacts to the provider's view of the current stream. Events for a
// stream that is no longer live are stale and ignored.
func (s *Session) handleFeed(ev feedEvent) {
	if ev.stream == nil || ev.stream != s.live {
		return
	}

	switch ev.kind {
	case feedExhausted:
		s.emit(TrackEnded{})
		s.retireLive()

		if s.index+1 < len(s.queue) {
			s.startIndex(s.index + 1)

			return
		}
		s.setState(StateStopped)
		if s.device != nil && s.deviceStarted {
			_ = s.device.Pause()
		}
	case feedError:
		s.fail(ev.err, true)
	}
}

// pollDevice surfaces an asynchronous backend failure. It only runs while a
// track is active, so a device that errored after a planned stop cannot
// produce a spurious Failed.
func (s *Session) pollDevice() {
	if !s.inFlight || s.device == nil {
		return
	}
	if err := s.device.Err(); err != nil {
		s.fail(err, true)
	}
}

// fail tears the pipeline down after a decode or device error and reports it.
// closeDevice is false for an open failure, where the existing device is still
// healthy and can be reused by the next Play. Bumping seq discards any build
// still in flight so a stale track cannot activate after the failure.
func (s *Session) fail(err error, closeDevice bool) {
	s.seq++
	s.retireLive()

	if closeDevice && s.device != nil {
		_ = s.device.Pause()
		_ = s.device.Flush()
		_ = s.device.Close()
		s.device = nil
		s.deviceStarted = false
	}

	s.emit(Failed{Err: err})
	s.setState(StateStopped)
}

// stopToStopped ends playback on request or at the end of the queue. The
// device stays open, paused, ready for the next track. Bumping seq discards any
// build that is still in flight, so a Stop followed by a stale open cannot
// resurrect a track the user already stopped.
func (s *Session) stopToStopped() {
	s.seq++
	s.retireLive()
	s.park()
	s.setState(StateStopped)
}

// requestSeek accepts a seek from the control loop without performing it. The
// device is parked here, so the origin is frozen and no device read races the
// worker's flush, and the intent to resume is decided now. The reposition then
// runs on a worker; the control loop keeps serving commands and events.
func (s *Session) requestSeek(d time.Duration) {
	live := s.live
	if live == nil {
		return
	}

	// Park before reading the origin and before the reposition: the device must
	// not be mid-read while the streamer flushes, and the audio it had queued is
	// the pre-seek position, so it goes too.
	s.park()

	// The chain must not blend the tail before the jump into the audio after
	// it. Reset is safe here, and only here: the device is parked, so no
	// Process is running against the filter state Reset clears.
	s.resetChain()

	// Capture the origin once the device is parked, so the reported position is
	// the frame the seek actually started from rather than a moving target.
	frame := live.Position()
	req := &seekRequest{
		seq:    s.seq,
		stream: live,
		kind:   requestSeek,
		target: d,
		frame:  frame,
		from:   stream.FramesToDuration(frame),
		resume: s.state() == StatePlaying,
	}

	s.submit(req)
}

// requestSwapDecoder accepts a live decoder change. With a streamer playing it
// parks the device and queues a reopen at the current frame; with none it just
// records the preference, so the next track uses it. The reopen runs on the
// same single worker as a seek, so only one engine step ever touches a
// streamer.
func (s *Session) requestSwapDecoder(name string) {
	live := s.live
	if live == nil {
		s.runtime.setDecoder(name)

		return
	}

	s.park()
	// A decoder swap reopens the stream at a frame, which is a discontinuity
	// like a seek, so the chain state is cleared for the same reason: the
	// device is parked, so nothing is in Process.
	s.resetChain()
	frame := live.Position()
	req := &seekRequest{
		seq:         s.seq,
		stream:      live,
		kind:        requestSwapDecoder,
		name:        name,
		path:        s.trackPath(),
		frame:       frame,
		from:        stream.FramesToDuration(frame),
		resume:      s.state() == StatePlaying,
		providerRef: s.v.providerRef,
	}

	s.submit(req)
}

// requestSwapBackend accepts a live backend change. With no device open it just
// records the preference for the next build; with one it parks the old device
// and queues a rebuild on the worker, because opening an audio server is slow
// and must not run on the control loop. Empty means the session's default
// backend, resolved here so the runtime never stores a nameless backend that
// the next Open would reject.
func (s *Session) requestSwapBackend(name string) {
	if name == "" {
		name = s.cfg.Backend
	}
	if s.device == nil {
		s.runtime.setBackend(name)

		return
	}

	// Park first so the old device is not mid-read while it is replaced, and so
	// the queued pre-swap audio goes with it.
	s.park()

	req := &seekRequest{
		seq:  s.seq,
		kind: requestSwapBackend,
		name: name,
	}

	s.submit(req)
}

// requestSeamless accepts a finished source upgrade. The worker runs it without
// parking the device or resetting the chain: the swap keeps the ring, so audio
// is continuous, which is the reason the path exists. The user's seek or swap
// owns the one worker slot, so when one is in flight or queued the upgrade is
// parked in pendingSeamless rather than overwriting it, and promoted once the
// worker falls idle.
func (s *Session) requestSeamless(req *seekRequest) {
	if !s.requestCurrent(req) {
		return
	}
	if s.seekBusy || s.seekPending != nil {
		s.pendingSeamless = req

		return
	}
	s.startRequest(req)
}

// promoteSeamless starts a parked upgrade once the worker is idle and no seek
// or swap is waiting. It runs after every finished request so an upgrade that
// was preempted by a user seek is not stranded behind it. A request bound to a
// track that has moved on is dropped.
func (s *Session) promoteSeamless() {
	req := s.pendingSeamless
	if req == nil || s.seekBusy || s.seekPending != nil {
		return
	}
	s.pendingSeamless = nil
	if !s.requestCurrent(req) {
		return
	}
	s.startRequest(req)
}

// submit queues a request, collapsing a burst to the newest one. The in-flight
// worker is not disturbed; its completion starts the newest not-yet-started
// request.
func (s *Session) submit(req *seekRequest) {
	if s.seekBusy {
		s.seekPending = req

		return
	}

	s.startRequest(req)
}

// trackPath returns the path of the current track, which a swap needs to reopen
// it. The control goroutine owns the view, so reading it here is safe.
func (s *Session) trackPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.v.path
}

// openerLabels holds what a swapped decoder reports about itself. The opener
// runs inside the producer goroutine, so these are read only after the streamer
// has acknowledged the swap that used them.
type openerLabels struct {
	decoder string
	parser  string
}

// decoderOpener builds an Opener for the named decoder plus the place its
// labels land. It is a fresh opener per request, because the preference can
// change between requests and the streamer must be free to rebuild with the
// exact implementation that was asked for.
func (s *Session) decoderOpener(name, path string) (stream.Opener, *openerLabels) {
	labels := &openerLabels{}
	open := func(<-chan struct{}) (decode.Decoder, error) {
		select {
		case <-s.closing:
			return nil, ErrClosed
		default:
		}

		d, err := s.cfg.openDecoder(name, path)
		if err != nil {
			return nil, err
		}
		labels.decoder, labels.parser = decode.Describe(d)

		return d, nil
	}

	return open, labels
}

// seamlessOpener wraps an upgrade Opener so the labels of the decoder it builds
// are recorded for the live view, mirroring decoderOpener. The opener runs in
// the producer goroutine, so the labels are read only after the streamer has
// acknowledged the swap that used them. A (nil, nil) result is turned into an
// error here because the streamer would call Info on it and panic the producer.
func (s *Session) seamlessOpener(open stream.Opener) (stream.Opener, *openerLabels) {
	labels := &openerLabels{}
	wrapped := func(stop <-chan struct{}) (decode.Decoder, error) {
		select {
		case <-s.closing:
			return nil, ErrClosed
		default:
		}

		d, err := open(stop)
		if err != nil {
			return nil, err
		}
		if d == nil {
			return nil, fmt.Errorf("%w: upgrade opener returned a nil decoder", ErrProviderNoDecoder)
		}
		labels.decoder, labels.parser = decode.Describe(d)

		return d, nil
	}

	return wrapped, labels
}

// startRequest hands one accepted request to a worker. At most one worker runs
// at a time, so two engine steps never share a streamer or a device. The worker
// owns the slow call and reports back on seekCh; it must not touch session
// state.
func (s *Session) startRequest(req *seekRequest) {
	s.seekBusy = true
	s.workerWG.Add(1)

	go func() {
		defer s.workerWG.Done()

		start := time.Now()
		res := seekResult{
			seq:    req.seq,
			stream: req.stream,
			kind:   req.kind,
			name:   req.name,
			target: req.target,
			from:   req.from,
			resume: req.resume,
		}

		switch req.kind {
		case requestSwapBackend:
			res.device, res.err = s.buildDevice(req.name)
		case requestSwapDecoder:
			// SwapDecoder refuses a provider ref on the caller's goroutine, so
			// this is a belt-and-braces guard for a track that changed between
			// the check and the worker. The error is reported as Failed below.
			if req.providerRef {
				res.err = fmt.Errorf("%w: %s", ErrProviderNoDecoderSwap, req.path)

				break
			}
			open, labels := s.decoderOpener(req.name, req.path)
			res.err = req.stream.SwapDecoder(req.frame, open)
			if res.err == nil {
				res.decoder, res.parser = labels.decoder, labels.parser
			}
		case requestSwapSeamless:
			// The ring is kept, so this deliberately does not park the device
			// or reset the chain: that is the whole point of the path. The
			// streamer refuses a decoder that cannot be positioned, and every
			// failure leaves the built decoder playing; handleSeamlessSwap
			// treats it as a silent no-op rather than a Failed track.
			res.err = req.stream.SwapDecoderSeamless(req.open)
			if res.err == nil {
				res.decoder, res.parser = req.labels.decoder, req.labels.parser
			}
		default:
			res.err = req.stream.SeekFrame(framesFor(req.target))
		}
		res.elapsed = time.Since(start)

		select {
		case s.seekCh <- res:
		case <-s.closing:
			// Shutdown is already tearing down; a device this worker built has
			// no owner to close it, so it does here. The control loop drains
			// whatever made it into the channel.
			if res.device != nil {
				_ = res.device.Close()
			}
		}
	}()
}

// buildDevice constructs and opens a device for the named backend against the
// session's stable provider. A failure closes the half-built device so a failed
// swap leaks nothing.
func (s *Session) buildDevice(name string) (playback.Device, error) {
	dev, err := s.cfg.newDevice(name)
	if err != nil {
		return nil, err
	}
	if err := dev.Open(canonical, s.provider); err != nil {
		_ = dev.Close()

		return nil, err
	}

	return dev, nil
}

// startPendingRequest launches the newest not-yet-started request, if any, and
// reports whether it did. A pending request for a track that has since moved on
// is stale and dropped.
func (s *Session) startPendingRequest() bool {
	req := s.seekPending
	s.seekPending = nil
	if !s.requestCurrent(req) {
		return false
	}
	s.startRequest(req)

	return true
}

// requestCurrent reports whether a queued request still belongs to the live
// track. A backend swap is about the device, which outlives a track, so it only
// needs the session generation; a seek or decoder swap is bound to its streamer
// and must also still be the live one.
func (s *Session) requestCurrent(req *seekRequest) bool {
	if req == nil {
		return false
	}
	if req.kind == requestSwapBackend {
		return req.seq == s.seq
	}

	return req.stream == s.live && req.seq == s.seq
}

// handleSeek applies a finished engine step. A result whose track is no longer
// live is stale and ignored whole: it must not move the new track or resume a
// device the user asked to leave alone.
func (s *Session) handleSeek(res seekResult) {
	s.seekBusy = false

	switch res.kind {
	case requestSwapBackend:
		s.handleBackendSwap(res)
	case requestSwapDecoder:
		s.handleDecoderSwap(res)
	case requestSwapSeamless:
		s.handleSeamlessSwap(res)
	default:
		s.handleReposition(res)
	}

	// The worker is idle again, so a source upgrade parked behind whatever ran
	// last can take the slot. A handler that already started a pending seek or
	// swap leaves seekBusy true and this is a no-op.
	s.promoteSeamless()
}

// handleReposition applies a finished seek. The device is resumed only when the
// seek intended it and the session is still playing.
func (s *Session) handleReposition(res seekResult) {
	if res.stream != s.live || res.seq != s.seq {
		s.startPendingRequest()

		return
	}

	if res.err != nil {
		s.emit(Failed{Err: res.err})
		s.startPendingRequest()

		return
	}

	// A newer target replaced this one, so stay parked until it lands: a resumed
	// device would be handed a half-flushed ring by the next reposition.
	if s.startPendingRequest() {
		// This result was superseded before anyone could act on it, so it is an
		// internal step rather than an observable seek: emitting Seeked here
		// would tell a consumer "the position is now X" when the target has
		// already moved on, and it would do so once per queued seek.
		return
	}

	if res.resume && s.state() == StatePlaying && s.device != nil {
		_ = s.device.Resume()
	}

	s.emit(Seeked{Position: res.target, From: res.from, Elapsed: res.elapsed})
}

// handleDecoderSwap applies a finished live decoder change. On success it
// records the preference and the new labels and emits Swapped; on failure it
// reports Failed and resumes the old decoder, which the streamer already
// restored at the swap position.
func (s *Session) handleDecoderSwap(res seekResult) {
	if res.stream != s.live || res.seq != s.seq {
		s.startPendingRequest()

		return
	}

	if res.err != nil {
		s.emit(Failed{Err: res.err})
		if s.startPendingRequest() {
			// A newer swap replaced this one; stay parked until it lands.
			return
		}
		if res.resume && s.state() == StatePlaying && s.device != nil {
			_ = s.device.Resume()
		}

		return
	}

	// The decoder really is the new one now, so the labels must say so even
	// when a newer swap supersedes this result: if that swap later fails, the
	// streamer restores this decoder and the snapshot has to match it.
	s.runtime.setDecoder(res.name)
	s.updateView(func(v *view) {
		v.decoder = res.decoder
		v.parser = res.parser
	})

	if s.startPendingRequest() {
		// This swap was superseded before it could be reported. It still
		// happened, which is why the labels above were updated, but emitting
		// Swapped here would name a decoder that is about to be replaced.
		return
	}

	if res.resume && s.state() == StatePlaying && s.device != nil {
		_ = s.device.Resume()
	}

	s.emit(Swapped{Kind: "decoder", Name: res.name, Elapsed: res.elapsed})
}

// handleSeamlessSwap applies a finished source upgrade. Nothing is reported: no
// event, no preference change. On success only the live decoder and parser
// labels move; on failure they stand, because the streamer kept the old decoder
// and the upgrade is an optimisation, not a track failure. Either way a seek or
// swap queued behind the upgrade is started, since the upgrade never parked the
// device the way a seek does.
func (s *Session) handleSeamlessSwap(res seekResult) {
	if res.stream != s.live || res.seq != s.seq {
		s.startPendingRequest()

		return
	}

	if res.err == nil {
		s.updateView(func(v *view) {
			v.decoder = res.decoder
			v.parser = res.parser
		})
	}

	s.startPendingRequest()
}

// handleBackendSwap installs a finished device rebuild. The old device is
// closed only after the new one opened, so the silent gap stays as short as
// possible. A device that started successfully keeps deviceStarted true; one
// installed while paused is left unstarted, so a later Resume starts it
// instead of asking an unstarted device to continue.
func (s *Session) handleBackendSwap(res seekResult) {
	if res.seq != s.seq {
		if res.device != nil {
			_ = res.device.Close()
		}
		s.startPendingRequest()

		return
	}

	if res.err != nil {
		s.emit(Failed{Err: res.err})
		if s.startPendingRequest() {
			// A newer swap replaced this one; stay parked until it lands.
			return
		}
		if s.state() == StatePlaying && s.device != nil {
			_ = s.device.Resume()
		}

		return
	}

	old := s.device
	s.device = res.device
	if old != nil {
		_ = old.Close()
	}

	// Follow the live state, not the state captured at request time: the user
	// may have paused or resumed while the swap was building. A new device must
	// be Started when the session is playing, and left unstarted when paused so
	// a later Resume starts it rather than asking it to continue.
	if s.state() == StatePlaying {
		if err := res.device.Start(); err != nil {
			s.startPendingRequest()
			s.fail(err, true)

			return
		}
		s.deviceStarted = true
	} else {
		s.deviceStarted = false
	}

	s.runtime.setBackend(res.name)

	if s.startPendingRequest() {
		// A newer swap is already building, so this one is an internal step:
		// emitting Swapped would name a backend about to be replaced.
		return
	}

	s.emit(Swapped{Kind: "backend", Name: res.name, Elapsed: res.elapsed})
}

// shutdown is the single teardown path. It runs on the control goroutine, so
// no other engine mutation can race it. Builds that were in flight are waited
// out and any streamer they delivered is closed, so nothing survives Close.
func (s *Session) shutdown() {
	s.retireLive()
	if s.device != nil {
		_ = s.device.Pause()
		_ = s.device.Close()
		s.device = nil
		s.deviceStarted = false
	}

	s.workerWG.Wait()
	for {
		select {
		case res := <-s.openCh:
			if res.streamer != nil {
				_ = res.streamer.Close()
			}
		case res := <-s.seekCh:
			// A finished backend swap may have delivered a device the control
			// loop never got to install. Closing it here is what keeps Close
			// from leaking an audio server handle.
			if res.device != nil {
				_ = res.device.Close()
			}
		default:
			s.events.close()

			return
		}
	}
}

// setState applies a transition if it is legal and emits the change. An
// illegal transition is a no-op, never a panic: commands arrive from a UI and
// a wrong one must not take the engine down.
func (s *Session) setState(to State) {
	s.mu.Lock()
	from := s.v.state
	if from == to || !canTransition(from, to) {
		s.mu.Unlock()

		return
	}
	s.v.state = to
	s.mu.Unlock()

	s.emit(StateChanged{From: from, To: to})
}

func (s *Session) state() State {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.v.state
}

func (s *Session) updateView(fn func(*view)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.v)
}

func (s *Session) emit(ev Event) { s.events.push(ev) }

// framesFor converts a duration into the streamer's output-frame domain. It is
// the inverse of stream.FramesToDuration and lives here so the rate used for
// seeking is the same constant the position helper uses.
func framesFor(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}

	return int64(d) * int64(canonical.Rate) / int64(time.Second)
}
