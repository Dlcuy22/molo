// Oto playback backend.
//
// oto is a push API: it owns the thread that reads PCM from an io.Reader and
// hands it to the driver. This file adapts that to the pull-shaped Provider
// contract with pcmReader, so every Device looks the same to the engine.
//
// oto supports exactly one Context per process, so the context is created once
// and shared. That is sound here because the device format is fixed at 48 kHz
// stereo float32 for the life of the program: one context can only carry one
// format anyway.

//go:build !freebsd && !android && !ios

package playback

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/ebitengine/oto/v3"
)

// otoBackendName is the registry key. The stub registers the same name on the
// platforms where oto cannot build, so callers never branch on GOOS.
const otoBackendName = "oto"

// contextReadyTimeout bounds the wait for the driver to come up. Without it a
// wedged audio server would hang Open (and every test) instead of failing.
const contextReadyTimeout = 10 * time.Second

// The one context. sync.Once is the only correct construction because oto
// rejects a second NewContext outright, and the failure is cached: a process
// that starts before the audio server does cannot recover, because oto marks
// the context created even when the driver never comes up.
var (
	otoCtxOnce sync.Once
	otoCtx     *oto.Context
	otoCtxErr  error
)

// otoContext returns the process-wide context, creating it on first use. The
// returned error is sticky: once the driver fails to initialize there is no
// retry, which is documented on ErrAudioInit.
func otoContext() (*oto.Context, error) {
	otoCtxOnce.Do(func() {
		ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
			SampleRate:      deviceRate,
			ChannelCount:    deviceChannels,
			Format:          oto.FormatFloat32LE,
			ApplicationName: "player",
		})
		if err != nil {
			otoCtxErr = err

			return
		}

		select {
		case <-ready:
		case <-time.After(contextReadyTimeout):
			otoCtxErr = fmt.Errorf("playback/oto: audio driver did not initialize within %v", contextReadyTimeout)

			return
		}
		if err := ctx.Err(); err != nil {
			otoCtxErr = err

			return
		}
		otoCtx = ctx
	})

	return otoCtx, otoCtxErr
}

// ErrAudioInit means the audio driver could not be brought up. It wraps the
// driver's own error so a caller can log the real cause (no server, no
// permission) instead of a generic failure.
var ErrAudioInit = errors.New("playback/oto: audio init failed")

// The device format is pinned in one place so Open and the shared context can
// never disagree about what the backend is playing. It is derived from the
// engine's canonical format rather than restated here, so a change to the
// canonical rate cannot leave this backend behind.
var (
	deviceRate     = core.CanonicalFormat.Rate
	deviceChannels = core.CanonicalFormat.Ch
)

// playerBufferFrames is how far ahead of the device oto is allowed to read:
// 100 ms at the device rate. oto's default is 500 ms, which is larger than the
// streamer's 300 ms ring, so every pull would ask for more than the ring can
// ever hold, come back short, and be counted as an underrun even when audio is
// flowing perfectly. Matching the engine's own chunk size keeps reads whole and
// the underrun counter meaningful, at the cost of a little robustness margin.
var playerBufferFrames = deviceRate * 100 / 1000

type deviceState uint8

const (
	stateIdle deviceState = iota
	stateOpen
	statePlaying
	statePaused
	stateClosed
)

// otoDevice is the oto implementation of Device. It is a thin state machine
// around one oto.Player, because the hard parts (format, lifecycle, single
// context) live in Open and in oto itself.
type otoDevice struct {
	mu     sync.Mutex
	state  deviceState
	ctx    *oto.Context
	player *oto.Player
	src    *pcmReader
	format core.FrameFormat
}

var _ Device = (*otoDevice)(nil)

func newOtoDevice() Device { return &otoDevice{state: stateIdle} }

func init() {
	Register(otoBackendName, newOtoDevice)
}

// Open binds the device to a format and a Provider. The engine only ever
// speaks 48 kHz stereo float32, so any other format is a bug above this layer
// and is rejected instead of resampled.
//
// The Context is created here rather than in Start because a failed driver
// should surface as an Open error, not as silence after a successful Start.
func (d *otoDevice) Open(f core.FrameFormat, p Provider) error {
	if p == nil {
		return errors.New("playback/oto: Open needs a provider")
	}
	if f.Rate != deviceRate || f.Ch != deviceChannels || f.Fmt != core.F32 {
		return fmt.Errorf("%w: got %+v", ErrUnsupportedFormat, f)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case stateClosed:
		return ErrClosed
	case stateIdle:
	default:
		return ErrOpen
	}

	ctx, err := otoContext()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAudioInit, err)
	}

	src := newPCMReader(p, f.Ch)
	player := ctx.NewPlayer(src)
	// oto's default player buffer is 500 ms, larger than the streamer's 300 ms
	// ring: every pull would ask for more than the ring can hold and come back
	// short, which the streamer counts as an underrun even when audio is
	// flowing. Matching the engine's own 100 ms chunk keeps reads whole.
	// NewPlayer returns a paused player that does not touch the source until
	// Play, so Open does not consume any of the stream.
	player.SetBufferSize(playerBufferFrames * f.Ch * 4)

	d.player = player
	d.ctx = ctx
	d.src = src
	d.format = f
	d.state = stateOpen

	return nil
}

// Start begins playback. It is a no-op on a device that is already playing, so
// a redundant Start cannot skip audio.
func (d *otoDevice) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case stateClosed:
		return ErrClosed
	case stateIdle:
		return ErrNotOpen
	case statePlaying:
		return nil
	}

	d.player.Play()
	d.state = statePlaying

	return nil
}

// Pause stops playback and stops consuming the Provider. PauseAndStopReading,
// not Pause, is deliberate: the plain Pause keeps draining the source to fill
// oto's buffer, so a paused player would still advance the stream. It blocks
// until an in-flight read finishes, which bounds it to one buffer fill.
func (d *otoDevice) Pause() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case stateClosed:
		return ErrClosed
	case stateIdle:
		return ErrNotOpen
	case stateOpen, statePaused:
		return nil
	}

	d.player.PauseAndStopReading()
	d.state = statePaused

	return nil
}

// Resume continues after Pause. On an open-but-never-started device it is a
// no-op rather than an implicit Start, so Resume cannot begin playback that a
// caller only asked to keep running.
func (d *otoDevice) Resume() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case stateClosed:
		return ErrClosed
	case stateIdle:
		return ErrNotOpen
	case stateOpen, statePlaying:
		return nil
	}

	d.player.Play()
	d.state = statePlaying

	return nil
}

// Close releases the device. It is idempotent and safe to call from any
// goroutine, including one other than Start's, and while audio is playing.
//
// Both steps are ordered for a reason: the reader is closed first so the pull
// side stops handing out new frames, then PauseAndStopReading waits out the one
// read that may already be in flight. After Close returns the Provider is not
// touched again, which is what lets a caller close or reuse the stream safely.
//
// Close can block while that in-flight read finishes. The Provider contract
// makes it bounded: stream.Streamer.ReadFrames never parks once the stream is
// closed or exhausted, so the read it is waiting on returns promptly. A
// Provider that blocks indefinitely would make Close block too, which is why
// the adapter also checks its closed flag on every loop.
func (d *otoDevice) Close() error {
	d.mu.Lock()
	if d.state == stateClosed {
		d.mu.Unlock()

		return nil
	}
	d.state = stateClosed
	player, src := d.player, d.src
	d.player, d.src = nil, nil
	d.mu.Unlock()

	if src != nil {
		src.close()
	}
	if player != nil {
		player.PauseAndStopReading()
		// oto v3.5's Player.Close is a no-op and the mux player is reclaimed by
		// oto's own finalizer, so dropping the reference is the whole release.
	}

	return nil
}

// Latency reports how much audio has been queued to the backend but not yet
// played, from oto's BufferedSize. The backend's own device buffer (about
// 100 ms on PulseAudio) is not included, so this is a lower bound on
// end-to-end latency and an exact measure of the audio still waiting in oto's
// buffer, which is what a Pause leaves for Resume and what a seek must flush.
func (d *otoDevice) Latency() time.Duration {
	d.mu.Lock()
	player, f := d.player, d.format
	d.mu.Unlock()

	if player == nil || f.Rate <= 0 || f.Ch <= 0 {
		return 0
	}
	queued := player.BufferedSize()
	if queued <= 0 {
		return 0
	}
	bytesPerSecond := f.Rate * f.Ch * 4

	return time.Duration(int64(queued) * int64(time.Second) / int64(bytesPerSecond))
}

// Err surfaces a failure from oto's reader thread. The Provider's errors have
// no synchronous call site, so this is the only way a decode failure reaches a
// caller that is merely polling. End of stream is not an error: oto reports it
// as EOF and the player just stops.
func (d *otoDevice) Err() error {
	d.mu.Lock()
	player, ctx := d.player, d.ctx
	d.mu.Unlock()

	if player == nil {
		return nil
	}
	if err := player.Err(); err != nil {
		return err
	}
	if ctx == nil {
		return nil
	}

	return ctx.Err()
}
