package playback

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/dlcuy22/player/core"
)

// ErrUnknownBackend means no backend was registered under the requested name.
var ErrUnknownBackend = errors.New("playback: unknown backend")

// ErrClosed is returned by every device operation after Close. Close itself
// stays callable and returns nil, so shutdown is idempotent.
var ErrClosed = errors.New("playback: device is closed")

// ErrNotOpen means a device was started or reopened before Open succeeded.
var ErrNotOpen = errors.New("playback: device is not open")

// ErrOpen means Open was called on a device that is already open. A Device is
// a single-output resource; opening it twice would silently drop the first
// Provider.
var ErrOpen = errors.New("playback: device is already open")

// ErrUnsupportedFormat means the caller asked for a layout the backend cannot
// produce. The engine's device format is fixed at 48 kHz stereo float32, so a
// mismatch is a bug above this layer, not something to resample here.
var ErrUnsupportedFormat = errors.New("playback: device format must be 48000 Hz stereo float32")

// Provider is the pull side of the engine: it hands out interleaved float32
// frames at the device format. stream.Streamer.ReadFrames matches this exactly,
// which is why this package never imports stream and a *stream.Streamer is
// accepted structurally.
type Provider interface {
	// ReadFrames fills dst with whole interleaved frames. It blocks while the
	// stream is live but has nothing buffered, and returns io.EOF once the
	// last frame has been delivered.
	ReadFrames(dst []float32) (frames int, err error)
}

// Device is one open output. It lives across tracks because the format is
// fixed, which is why Open is separate from Start: the sink is prepared once
// and fed many streams.
//
// Err is the one method beyond the plan's sketch. It is there because a push
// backend reads the Provider on its own thread: a decode failure or a dead
// audio server has no call site to be returned from, so without Err the engine
// can only go silent. It is the asynchronous counterpart to the error the
// Provider already returns, and it must stay cheap and safe to poll.
type Device interface {
	// Open binds the device to a format and a Provider. It must be called
	// before Start.
	Open(f core.FrameFormat, p Provider) error
	// Start begins consuming the Provider. It is safe on a device that is
	// already started.
	Start() error
	// Pause stops playback and input consumption. It may be called from a
	// goroutine other than the one that called Start.
	Pause() error
	// Resume continues after Pause. It is a no-op on a device that is not
	// started.
	Resume() error
	// Flush discards audio the backend has queued but not yet played, without
	// changing whether it is playing. It exists because Pause alone keeps that
	// queued audio: resuming would replay up to a buffer of the old position,
	// which is exactly what a seek or a track change must not do. A device that
	// is not open, or has nothing queued, treats it as a no-op.
	Flush() error
	// Close releases the sink. It is idempotent and safe to call from any
	// goroutine, including while audio is playing.
	Close() error
	// Latency reports how much audio the backend has accepted but not yet
	// played. It is a real queue measurement, not a fixed device figure.
	Latency() time.Duration
	// Err reports a failure that happened on the backend's own thread since
	// the last call. A nil error means the device is still healthy; reaching
	// the end of the Provider is not an error.
	Err() error
}

// Registry maps backend names to factories. It mirrors decode.Registry so a
// second backend is one file plus one init, with no edit to this one.
type Registry interface {
	Register(name string, factory func() Device)

	// RegisterDev adds a backend that exists for development and testing only.
	// It is openable by name and accepted by validation, so a test can select
	// it, but it is left out of UserNames so a UI never offers it as a real
	// output device.
	RegisterDev(name string, factory func() Device)

	// Open resolves a name and constructs a fresh, unopened Device.
	Open(name string) (Device, error)

	// Names lists every registered backend, sorted. It is the engine's view: a
	// name here can be opened and passes validation.
	Names() []string

	// UserNames lists the backends a UI should offer, sorted. It is Names
	// minus the dev-only entries, so a null or fake sink never reaches a
	// dropdown a listener sees.
	UserNames() []string
}

type registry struct {
	mu       sync.Mutex
	backends map[string]func() Device
	// dev marks the backends registered through RegisterDev. It is separate
	// from backends so the two views stay consistent without a second copy of
	// the factories.
	dev map[string]bool
}

// NewRegistry returns an empty registry. The package-level Default is what
// backends register into; this exists for tests and isolated embedders.
func NewRegistry() Registry {
	return &registry{
		backends: make(map[string]func() Device),
		dev:      make(map[string]bool),
	}
}

// Default is the registry populated by the init functions in this package.
var Default = NewRegistry()

// Register adds a backend to the process-wide Default registry. It panics on
// an empty or duplicate name: a shadowed backend is a bug that would otherwise
// only surface as the wrong sound card in production.
func Register(name string, factory func() Device) {
	Default.Register(name, factory)
}

// RegisterDev adds a development-only backend to the process-wide registry.
// See Registry.RegisterDev for what that hides.
func RegisterDev(name string, factory func() Device) {
	Default.RegisterDev(name, factory)
}

func (r *registry) Register(name string, factory func() Device) {
	r.register(name, factory, false)
}

func (r *registry) RegisterDev(name string, factory func() Device) {
	r.register(name, factory, true)
}

func (r *registry) register(name string, factory func() Device, dev bool) {
	if name == "" {
		panic("playback: Register requires a backend name")
	}
	if factory == nil {
		panic("playback: Register requires a factory for " + name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.backends[name]; dup {
		panic("playback: backend " + name + " is already registered")
	}
	r.backends[name] = factory
	if dev {
		r.dev[name] = true
	}
}

// Open resolves a backend by name. The returned Device is constructed but not
// opened, so the caller still chooses the format and Provider.
func Open(name string) (Device, error) {
	return Default.Open(name)
}

func (r *registry) Open(name string) (Device, error) {
	r.mu.Lock()
	factory, ok := r.backends[name]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, name)
	}

	d := factory()
	if d == nil {
		return nil, fmt.Errorf("playback: backend %q returned no device", name)
	}

	return d, nil
}

// Names lets a caller enumerate backends without constructing or opening any
// of them, which matters once a backend like malgo can be compiled in but
// refuse at runtime. It includes dev-only backends, so it is the list a CLI
// flag or a validation check should use.
func Names() []string { return Default.Names() }

func (r *registry) Names() []string {
	r.mu.Lock()
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	r.mu.Unlock()
	slices.Sort(names)

	return names
}

// UserNames lists the backends a UI should offer. It is Names without the
// dev-only entries, so a fake or null sink cannot appear as a real output.
func UserNames() []string { return Default.UserNames() }

func (r *registry) UserNames() []string {
	r.mu.Lock()
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		if r.dev[name] {
			continue
		}
		names = append(names, name)
	}
	r.mu.Unlock()
	slices.Sort(names)

	return names
}
