package session

import (
	"errors"
	"io"
	"sync/atomic"

	"github.com/dlcuy22/player/dsp"
	"github.com/dlcuy22/player/stream"
)

// feedKind classifies an internal engine notification. These are separate from
// user events: they are facts the controller must act on, not things a UI
// displays, and they must not be dropped.
type feedKind uint8

const (
	// feedExhausted means the current stream delivered its last frame.
	feedExhausted feedKind = iota
	// feedError means the current stream or the provider failed.
	feedError
)

type feedEvent struct {
	kind   feedKind
	stream *stream.Streamer
	err    error
}

// routedProvider is the stable Provider the device is opened against once.
//
// It exists because playback.Device has no way to swap its Provider: a device
// is opened against one Provider for its whole life. If the session handed the
// device a *stream.Streamer directly, every track change would need a new
// device. Instead this indirection points at whichever streamer is current, so
// the device is reused across tracks and the underlying audio server is
// opened exactly once.
//
// The provider never returns io.EOF while a session is alive: when a track is
// exhausted it reports that to the controller and answers with silence until
// the next track is installed. That keeps a push backend's player from
// entering its terminal end-of-source state, which is what makes Stop followed
// by Play work on the same device.
type routedProvider struct {
	gain *dsp.Gain

	// tap observes the post-gain samples on their way to the device. It is
	// created once and reused for the session's life, so a visualizer keeps
	// reading across track changes. publish is a single atomic load until the
	// consumer's first Read activates it.
	tap *tap

	// cur is the streamer the device reads from. It is nil between tracks and
	// while stopped, which is when the provider emits silence.
	cur atomic.Pointer[stream.Streamer]

	// feed carries exhaustion and error notifications to the controller. The
	// sends here block briefly if the controller is busy, which is safe: the
	// controller's loop never performs blocking I/O, and it selects on this
	// channel. Losing a notification would strand the session, so they are
	// never dropped.
	feed chan<- feedEvent

	// done is closed when the session shuts down. It releases a send that
	// would otherwise wait for a controller that has already stopped.
	done <-chan struct{}
}

func newRoutedProvider(gain *dsp.Gain, t *tap, feed chan<- feedEvent, done <-chan struct{}) *routedProvider {
	return &routedProvider{gain: gain, tap: t, feed: feed, done: done}
}

// setCurrent points the device at a new streamer, or at nothing when s is nil.
func (p *routedProvider) setCurrent(s *stream.Streamer) {
	p.cur.Store(s)
}

// current reports the streamer the device is reading, or nil between tracks.
func (p *routedProvider) current() *stream.Streamer {
	return p.cur.Load()
}

// ReadFrames satisfies playback.Provider. It is the seam where the post-ring
// gain runs, so volume is applied to exactly the samples the device receives,
// with no allocation and no lock on the hot path.
func (p *routedProvider) ReadFrames(dst []float32) (int, error) {
	for {
		s := p.cur.Load()
		if s == nil {
			return silence(dst), nil
		}

		n, err := s.ReadFrames(dst)
		if n > 0 {
			if perr := p.gain.Process(dst, n); perr != nil {
				p.notify(feedEvent{kind: feedError, stream: s, err: perr})

				return n, perr
			}
			// The tap sits after the gain and before the device: it must
			// observe what is actually heard, not the pre-volume samples. With
			// no consumer, publish is one atomic load and returns.
			p.tap.publish(dst, n, canonical.Ch)

			return n, nil
		}
		if err == nil {
			// A (0, nil) read made no progress. It is rare (a seek flushing the
			// ring between the streamer's availability check and its copy) but
			// real, so loop rather than hand the device a read that produced
			// nothing and would spin the audio thread.
			continue
		}

		if errors.Is(err, io.EOF) {
			// Claim the exhaustion once. A second reader (or a stale one)
			// must not report the same track twice.
			if p.cur.CompareAndSwap(s, nil) {
				p.notify(feedEvent{kind: feedExhausted, stream: s})
			}

			return silence(dst), nil
		}

		// ErrClosed from a streamer that is no longer current is a planned
		// shutdown, not a failure: the controller is switching tracks or
		// stopping. Loop to pick up whatever replaced it.
		if errors.Is(err, stream.ErrClosed) && p.cur.Load() != s {
			continue
		}

		p.notify(feedEvent{kind: feedError, stream: s, err: err})

		return 0, err
	}
}

// silence fills dst with whole frames of silence and returns the frame count.
// It is how the provider stays alive between tracks: a real device cannot be
// handed io.EOF without ending its player for good.
func silence(dst []float32) int {
	frames := len(dst) / canonical.Ch
	clear(dst[:frames*canonical.Ch])

	return frames
}

// notify hands an engine event to the controller. The send can block if the
// controller is momentarily busy, which is bounded because the controller
// never waits on I/O, and is released by shutdown so Close cannot deadlock.
func (p *routedProvider) notify(ev feedEvent) {
	select {
	case p.feed <- ev:
	case <-p.done:
	}
}
