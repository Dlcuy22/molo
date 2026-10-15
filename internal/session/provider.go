package session

import (
	"errors"
	"io"
	"sync/atomic"

	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/stream"
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
	// feedCrossfadeDone means the audio thread finished a crossfade ramp, so
	// the controller can promote the incoming track and retire the outgoing one.
	// It is sent from ReadFrames, which must not block, so the controller
	// re-reads the current crossfade state on receipt rather than trusting a
	// payload.
	feedCrossfadeDone
)

type feedEvent struct {
	kind   feedKind
	stream *stream.Streamer
	err    error
}

// routedProvider is the stable Provider the device is opened against once.
//
// playback.Device has no way to swap its Provider: a device is opened against
// one Provider for its whole life. If the session handed the device a
// *stream.Streamer directly, every track change would need a new device. This
// indirection points at whichever streamer is current, so the device is reused
// across tracks and the underlying audio server is opened exactly once.
//
// The provider never returns io.EOF while a session is alive: when a track is
// exhausted it reports that to the controller and answers with silence until
// the next track is installed. That keeps a push backend's player from entering
// its terminal end-of-source state, which is what makes Stop followed by Play
// work on the same device.
type routedProvider struct {
	gain *dsp.Gain

	// chain is the post-ring effect chain, applied before the master gain. It
	// is shared with the controller, which may publish a new effect list at
	// any time; Process loads the list once per buffer, so a swap here can
	// never make one buffer run a mix of the old and new chains.
	chain *dsp.Chain

	// tap observes the post-gain samples on their way to the device. It is
	// created once and reused for the session's life, so a visualizer keeps
	// reading across track changes. publish is a single atomic load until the
	// consumer's first Read activates it.
	tap *tap

	// cur is the streamer the device reads from. It is nil between tracks and
	// while stopped, which is when the provider emits silence.
	cur atomic.Pointer[stream.Streamer]

	// xfade is the crossfade in force, or nil. It is published by the control
	// loop when a ramp starts and cleared when the ramp completes or is
	// aborted. The audio thread loads it once per buffer.
	xfade atomic.Pointer[crossfade]

	// mix is the scratch buffer the incoming stream is read into during a
	// crossfade, allocated once so the audio thread never allocates. It is
	// sized for the largest device buffer the engine hands in.
	mix []float32

	// feed carries exhaustion and error notifications to the controller. The
	// send blocks briefly if the controller is busy, which is safe because the
	// controller's loop never performs blocking I/O. Losing a notification
	// would strand the session, so they are never dropped.
	feed chan<- feedEvent

	// done is closed when the session shuts down. It releases a send that
	// would otherwise wait for a controller that has already stopped.
	done <-chan struct{}
}

// crossfade is the live overlap between two streamers. from is the outgoing
// track the device is reading; to is the incoming one being read alongside it.
// total is the ramp length in frames and done is how many frames the audio
// thread has blended, so the two gains can be derived from one counter. Only
// the audio thread advances done; the control loop owns the pointer.
type crossfade struct {
	from  *stream.Streamer
	to    *stream.Streamer
	total int64
	done  atomic.Int64

	// rampDone guards the one-shot completion notification: the audio thread
	// sets it so the controller is told once, not on every buffer past the end.
	rampDone atomic.Bool
}

// finished reports whether the ramp has consumed its whole length.
func (x *crossfade) finished() bool {
	total := x.total
	if total <= 0 {
		return true
	}

	return x.done.Load() >= total
}

func newRoutedProvider(chain *dsp.Chain, gain *dsp.Gain, t *tap, feed chan<- feedEvent, done <-chan struct{}) *routedProvider {
	return &routedProvider{
		chain: chain,
		gain:  gain,
		tap:   t,
		feed:  feed,
		done:  done,
		// The device reads canonical stereo frames in bounded buffers; 8192
		// frames (about 170 ms) covers any realistic read with room to spare.
		mix: make([]float32, 8192*canonical.Ch),
	}
}

// setCurrent points the device at a new streamer, or at nothing when s is nil.
// It also clears any crossfade in flight: a track change is a hard cut, so a
// ramp from a track that is being replaced must not keep mixing.
func (p *routedProvider) setCurrent(s *stream.Streamer) {
	p.xfade.Store(nil)
	p.cur.Store(s)
}

// beginCrossfade publishes a ramp from the current streamer to next. It is
// called from the control loop; the audio thread picks the pair up on its next
// buffer. The caller is responsible for closing next if the ramp is aborted.
//
// It reports whether a ramp was published, so the caller sets its "blending"
// flag only when there is a blend to complete: a rejected request must not
// latch the session out of later ones.
func (p *routedProvider) beginCrossfade(from, to *stream.Streamer, total int64) bool {
	if from == nil || to == nil || total <= 0 {
		return false
	}
	p.xfade.Store(&crossfade{from: from, to: to, total: total})

	return true
}

// abortCrossfade drops a ramp without promoting, returning the incoming
// streamer so the caller can close it. It is a no-op when none is in flight.
func (p *routedProvider) abortCrossfade() *stream.Streamer {
	x := p.xfade.Swap(nil)
	if x == nil {
		return nil
	}

	return x.to
}

// current reports the streamer the device is reading, or nil between tracks.
func (p *routedProvider) current() *stream.Streamer {
	return p.cur.Load()
}

// ReadFrames satisfies playback.Provider. It is the seam where the post-ring
// effects and the master gain run, so both are applied to exactly the samples
// the device receives, with no allocation and no lock on the hot path.
//
// During a crossfade the outgoing and incoming streams are read together and
// summed with an equal-power ramp before the chain runs, so the effects and the
// master gain see one blended signal, and the visualizer tap sees exactly what
// is heard.
func (p *routedProvider) ReadFrames(dst []float32) (int, error) {
	for {
		s := p.cur.Load()
		if s == nil {
			return silence(dst), nil
		}

		// A crossfade in force replaces the plain read: both streams are mixed
		// into dst before the shared post-processing below.
		if x := p.xfade.Load(); x != nil {
			n, _ := p.readCrossfade(x, dst)
			if n > 0 {
				return p.finishRead(dst, n, s, nil)
			}
			// No frames: the outgoing track ended (err != nil), or nothing was
			// available this call (err == nil, which a live streamer does not
			// produce). Either way the blend cannot advance, so the ramp is
			// dropped and the plain read below either produces frames or
			// reports the end. Dropping it here also stops a (0, nil) from
			// spinning the loop.
			p.xfade.Store(nil)
		}

		n, err := s.ReadFrames(dst)
		if n > 0 {
			return p.finishRead(dst, n, s, nil)
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

// readCrossfade fills dst with the blend of the outgoing and incoming streams.
// It reads the outgoing stream blocking (it is what the device is playing) and
// the incoming one non-blocking, so a slow build on the incoming side cannot
// stall the audio thread: a short or empty read there contributes silence and
// the outgoing track keeps sounding.
//
// The blend covers exactly the frames the outgoing produced, so a short read
// from it is passed on as a short read, never padded with silence that would
// clip audio the producer has not handed over yet. The one exception is a
// terminal outgoing read: when the outgoing track ends before the ramp does
// (its probed duration was longer than its real audio, common for a network
// source), its remaining frames are silence and the incoming side keeps fading
// in over them. Dropping the whole buffer to silence the moment the outgoing
// ends is heard as a skip at the handoff, which is the bug this guards against.
//
// It reports the frames mixed. Once the ramp reaches its end it notifies the
// controller that the blend is done, once.
func (p *routedProvider) readCrossfade(x *crossfade, dst []float32) (int, error) {
	ch := canonical.Ch
	want := len(dst) / ch
	if want == 0 {
		return 0, nil
	}
	// One blend step never exceeds the scratch buffer.
	if max := len(p.mix) / ch; want > max {
		want = max
	}
	dst = dst[:want*ch]

	nFrom, errFrom := x.from.ReadFrames(dst)
	if nFrom > 0 {
		// The outgoing track is sounding. The blend spans exactly its frames,
		// and the incoming is read for the same count so the two stay aligned.
		scratch := p.mix[:nFrom*ch]
		nTo := x.tryReadTo(scratch)
		if nTo < nFrom {
			clear(scratch[nTo*ch:])
		}
		p.mixRamp(x, dst[:nFrom*ch], scratch)
		x.done.Add(int64(nFrom))
		p.notifyRampDone(x)

		return nFrom, nil
	}

	if errFrom == nil {
		// No frames and no end: nothing to mix this call.
		return 0, nil
	}

	// The outgoing track has ended before the ramp did. Its remaining frames
	// are silence, so the incoming track keeps fading in over them rather than
	// the whole buffer dropping to silence.
	scratch := p.mix[:want*ch]
	nTo := x.tryReadTo(scratch)
	if nTo == 0 {
		// Neither side has anything: the blend is over. The outgoing's terminal
		// error lets the controller promote the ready incoming track, or fall
		// back to the ordinary end-of-track path.
		return 0, errFrom
	}
	clear(dst[:nTo*ch])
	p.mixRamp(x, dst[:nTo*ch], scratch[:nTo*ch])
	x.done.Add(int64(nTo))
	p.notifyRampDone(x)

	return nTo, nil
}

// tryReadTo reads the incoming stream without waiting, returning the frames it
// produced. A nil incoming side is silence, so the mix stays safe even if a
// crossfade were ever published without one.
func (x *crossfade) tryReadTo(dst []float32) int {
	if x.to == nil {
		return 0
	}
	n, _ := x.to.TryReadFrames(dst)

	return n
}

// mixRamp sums inc into dst with the equal-power crossfade gains at the ramp's
// current position. The caller advances the ramp by adding the mixed frames to
// x.done, so the two steps stay in one place per buffer.
func (p *routedProvider) mixRamp(x *crossfade, dst, inc []float32) {
	ch := canonical.Ch
	done := x.done.Load()
	total := x.total
	for i := 0; i < len(dst)/ch; i++ {
		t := 1.0
		if total > 0 {
			t = float64(done+int64(i)) / float64(total)
		}
		out, in := dsp.CrossfadeGains(t)
		base := i * ch
		for c := 0; c < ch; c++ {
			dst[base+c] = float32(float64(dst[base+c])*out + float64(inc[base+c])*in)
		}
	}
}

// notifyRampDone tells the controller once that the ramp has reached its end.
func (p *routedProvider) notifyRampDone(x *crossfade) {
	if x.total <= 0 || x.done.Load() < x.total || x.rampDone.Load() {
		return
	}
	if x.rampDone.CompareAndSwap(false, true) {
		p.notify(feedEvent{kind: feedCrossfadeDone, stream: x.from})
	}
}

// finishRead runs the post-ring chain, the master gain and the tap over the
// first n frames of dst, then returns n. It is the shared tail of both the
// plain and the crossfaded read, so the effects and the master volume are
// applied once to the signal the listener actually hears.
func (p *routedProvider) finishRead(dst []float32, n int, s *stream.Streamer, err error) (int, error) {
	// Effects run first, then the master gain, then the tap observes. That
	// order is fixed: the master volume is final, so an effect that trims its
	// own output cannot be overridden by the user's volume, and the tap sees
	// exactly what the listener hears. Both stages load their state atomically,
	// so a chain or volume swap mid-buffer is safe and Process never blocks.
	if perr := p.chain.Process(dst, n); perr != nil {
		p.notify(feedEvent{kind: feedError, stream: s, err: perr})

		return n, perr
	}
	if perr := p.gain.Process(dst, n); perr != nil {
		p.notify(feedEvent{kind: feedError, stream: s, err: perr})

		return n, perr
	}
	// The tap sits after the gain and before the device: it must observe what
	// is actually heard, not the pre-volume samples. With no consumer, publish
	// is one atomic load and returns.
	p.tap.publish(dst, n, canonical.Ch)

	return n, nil
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
