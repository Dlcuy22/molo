package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
)

// ErrClosed is returned by every operation after Close. It is deliberately not
// io.EOF: a consumer that is shut down should be able to tell a planned stop
// from the track ending.
var ErrClosed = errors.New("stream: closed")

// ErrRunning means Start was called on a streamer that is already running.
var ErrRunning = errors.New("stream: already started")

// ErrNotStarted means Start has not been called, so there is no producer to
// apply a seek or fill the ring.
var ErrNotStarted = errors.New("stream: not started")

// ErrEndOfStream means the decoder is exhausted, so a seek cannot be applied.
var ErrEndOfStream = errors.New("stream: end of stream")

// ErrSeekRange means a seek target lies outside the stream.
var ErrSeekRange = errors.New("stream: seek target out of range")

// DefaultRingFrames is the buffered-audio target at the 48 kHz output rate,
// about 300 ms. The plan allows 150 to 500 ms.
const DefaultRingFrames = 48000 * 300 / 1000

// DefaultChunkFrames bounds one decoder read, about 100 ms.
const DefaultChunkFrames = 48000 * 100 / 1000

// canonical is the one format the ring and every consumer see. Pre-modules may
// change the format inside the producer, but the chain must end here.
var canonical = core.CanonicalFormat

// Opener builds a decoder for the stream. It is called once at construction and
// again by the reopen-and-discard seek fallback, which is why the streamer
// cannot own a single decoder. The stop channel is closed on shutdown so an
// opener that reads from an interruptible source can return a decoder that does
// not strand the producer in a blocking read.
type Opener func(stop <-chan struct{}) (decode.Decoder, error)

// Config tunes the buffer. Zero fields take the defaults documented on each
// field, so the empty Config is the intended 300 ms setup.
type Config struct {
	// RingFrames is the ring capacity in output frames, rounded up to a power
	// of two. Zero selects DefaultRingFrames.
	RingFrames int

	// HighWater is the level the producer refills to, in output frames. The
	// producer stops decoding once the ring reaches it, which bounds memory to
	// the ring plus one chunk. Zero selects the ring capacity.
	HighWater int

	// LowWater is the level at which a parked producer resumes. Setting it
	// below HighWater gives hysteresis so a single consumer read does not wake
	// the decoder for one frame. Zero selects half the ring capacity.
	LowWater int

	// ChunkFrames is the largest decoder read per iteration, in output frames.
	// Zero selects DefaultChunkFrames; a value larger than the ring is clamped,
	// because one chunk must never be able to overrun the ring.
	ChunkFrames int

	// Modules are the pre-ring stages, applied in order after decode. They run
	// in the producer goroutine and may allocate.
	Modules []core.Module
}

// Stats is a snapshot of the streamer's counters. Buffered is read at call
// time; the rest are cumulative since construction.
type Stats struct {
	Underruns int64
	Buffered  int64
	Decoded   int64
}

// Streamer owns the decoder goroutine and the ring that feeds the consumer. Its
// position domain is output frames at 48 kHz; nothing else here counts frames,
// so the native-rate, output-rate and sample-count mixing that broke the
// predecessor engine has no place to happen.
type Streamer struct {
	open Opener
	dec  decode.Decoder
	mods []core.Module

	// formats is the Configure chain: formats[0] is the decoder's output and
	// formats[i+1] is what mods[i] produces. Only its end is delivered.
	formats []core.FrameFormat
	decCh   int
	ch      int

	ring        *Ring
	high        int64
	low         int64
	chunkFrames int

	// work is the single scratch buffer of the producer. pending aliases a
	// slice of it and holds processed output not yet accepted by the ring.
	work    []float32
	pending []float32

	gate *gate
	pos  atomic.Int64

	seekTarget  int64
	seekSeq     uint64
	seekApplied uint64
	seekErr     error
	// A swap request carries its opener through the same gated slot as a seek
	// target, because only the producer may replace s.dec/s.open. The fields
	// are read together with seekSeq, so a newer request always overwrites
	// them whole.
	seekOpen Opener
	seekSwap bool

	eos    bool
	eosErr error
	// finished guards the one-shot transition to a terminal state.
	finished atomic.Bool
	readErr  error

	underruns atomic.Int64
	decoded   atomic.Int64

	done    chan struct{}
	stopped chan struct{}

	stopDec     chan struct{}
	stopDecOnce sync.Once

	startMu  sync.Mutex
	started  bool
	shutdown bool
	stopCtx  func() bool
}

// New opens the decoder, configures the pre-module chain and validates that it
// ends at 48 kHz stereo float32. Every setup error surfaces here rather than
// from the producer goroutine, so a bad format fails loudly at construction.
func New(open Opener, cfg Config) (*Streamer, error) {
	if open == nil {
		return nil, errors.New("stream: New requires an opener")
	}

	stopDec := make(chan struct{})
	dec, err := open(stopDec)
	if err != nil {
		return nil, err
	}

	s := &Streamer{
		open:    open,
		dec:     dec,
		gate:    newGate(),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
		stopDec: stopDec,
	}
	if err := s.configure(cfg); err != nil {
		dec.Close()

		return nil, err
	}

	return s, nil
}

func (s *Streamer) configure(cfg Config) error {
	in := s.dec.Info().Format
	if in.Rate <= 0 || in.Ch <= 0 {
		return fmt.Errorf("stream: decoder reports an unusable format %+v", in)
	}
	if in.Fmt != core.F32 {
		return fmt.Errorf("stream: decoder reports sample format %d; the ring carries F32 only", in.Fmt)
	}

	s.decCh = in.Ch
	s.ch = canonical.Ch
	s.mods = cfg.Modules

	s.formats = append(s.formats, in)
	for _, m := range s.mods {
		out, err := m.Configure(s.formats[len(s.formats)-1])
		if err != nil {
			return fmt.Errorf("stream: module %s configure: %w", m.Name(), err)
		}
		s.formats = append(s.formats, out)
	}
	if final := s.formats[len(s.formats)-1]; !final.Equal(canonical) {
		return fmt.Errorf("stream: pre-module chain ends at %+v, want %+v", final, canonical)
	}
	// A module may be reused across streams, so start it from a known state.
	for _, m := range s.mods {
		if err := m.Reset(); err != nil {
			return fmt.Errorf("stream: module %s reset: %w", m.Name(), err)
		}
	}

	frames := cfg.RingFrames
	if frames <= 0 {
		frames = DefaultRingFrames
	}
	s.ring = NewRing(frames, canonical.Ch)
	capacity := s.ring.Cap()

	s.high = int64(cfg.HighWater)
	if s.high == 0 {
		s.high = capacity
	}
	s.low = int64(cfg.LowWater)
	if s.low == 0 {
		s.low = capacity / 2
	}
	if s.low < 1 || s.low >= s.high || s.high > capacity {
		return fmt.Errorf("stream: watermarks low=%d high=%d do not fit a %d frame ring", s.low, s.high, capacity)
	}

	s.chunkFrames = cfg.ChunkFrames
	if s.chunkFrames <= 0 {
		s.chunkFrames = DefaultChunkFrames
	}
	if s.chunkFrames > int(capacity) {
		s.chunkFrames = int(capacity)
	}

	maxCh := 0
	for _, f := range s.formats {
		maxCh = max(maxCh, f.Ch)
	}
	s.work = make([]float32, s.chunkFrames*maxCh)

	return nil
}

// Start launches the decoder goroutine. Cancelling ctx shuts the streamer down
// exactly like Close. It is an error to start twice or to start after Close.
func (s *Streamer) Start(ctx context.Context) error {
	s.startMu.Lock()
	if s.shutdown {
		s.startMu.Unlock()

		return ErrClosed
	}
	if s.started {
		s.startMu.Unlock()

		return ErrRunning
	}
	s.started = true
	if ctx != nil {
		s.stopCtx = context.AfterFunc(ctx, func() { s.Close() })
	}
	s.startMu.Unlock()

	go s.run()

	return nil
}

// Done is closed when the decoder is exhausted or fails, which is the only way
// EOS is signalled. Close does not close it: a planned shutdown is reported by
// the read methods returning ErrClosed.
func (s *Streamer) Done() <-chan struct{} { return s.done }

// Position is the output frame index of the next frame the consumer will
// receive. It advances as frames are read and jumps to the target on seek.
func (s *Streamer) Position() int64 { return s.pos.Load() }

// FramesToDuration converts a position in the streamer's one frame domain
// (output frames at 48 kHz) into wall clock time. Every frame-to-time
// conversion goes through here so no caller can pair a native-rate count with
// the output clock, the mistake that broke the predecessor engine.
func FramesToDuration(frames int64) time.Duration {
	if frames <= 0 {
		return 0
	}

	return time.Duration(frames) * time.Second / time.Duration(canonical.Rate)
}

// Stats reports the counters. Buffered is the current ring occupancy.
func (s *Streamer) Stats() Stats {
	return Stats{
		Underruns: s.underruns.Load(),
		Buffered:  s.ring.Len(),
		Decoded:   s.decoded.Load(),
	}
}

// ReadFrames copies up to len(dst)/2 frames and returns how many it wrote. It
// waits only while the ring is empty and the stream is still live, so it never
// spins; once the decoder is exhausted it returns io.EOF (or the decoder's
// error) after the buffered tail drains.
//
// The signature is exactly playback.Provider's, so Phase 3 can pass a Streamer
// straight to a device; this package does not import playback to say so.
func (s *Streamer) ReadFrames(dst []float32) (int, error) {
	want := len(dst) / s.ch
	if want == 0 {
		return 0, nil
	}

	s.gate.mu.Lock()
	for !s.gate.closed && !s.eos && s.ring.Len() == 0 {
		s.gate.cond.Wait()
	}
	if s.gate.closed {
		s.gate.mu.Unlock()

		return 0, ErrClosed
	}

	n := s.ring.Read(dst)
	if n > 0 {
		s.pos.Add(int64(n))
	}
	eos, err := s.eos, s.eosErr
	s.gate.mu.Unlock()

	return s.finishRead(n, want, eos, err)
}

// TryReadFrames is the read for a callback-based device that must not wait on
// the audio thread. It never parks for data or blocks on a decode; it copies
// whatever is buffered and returns. The only critical section is the bounded
// ring copy, shared with the seek flush, so a seek is the sole operation that
// can delay it. It reports io.EOF only once the stream is both exhausted and
// drained.
func (s *Streamer) TryReadFrames(dst []float32) (int, error) {
	want := len(dst) / s.ch
	if want == 0 {
		return 0, nil
	}

	s.gate.mu.Lock()
	if s.gate.closed {
		s.gate.mu.Unlock()

		return 0, ErrClosed
	}

	n := s.ring.Read(dst)
	if n > 0 {
		s.pos.Add(int64(n))
	}
	eos, err := s.eos, s.eosErr
	s.gate.mu.Unlock()

	return s.finishRead(n, want, eos, err)
}

// finishRead applies the shared bookkeeping of both read paths: wake a producer
// that was blocked on a full ring, count a shortfall that is not end of stream,
// and translate a drained terminal stream into io.EOF.
func (s *Streamer) finishRead(n, want int, eos bool, err error) (int, error) {
	if n > 0 {
		s.gate.wake()
	}
	if n < want && !eos {
		s.underruns.Add(1)
	}
	if n == 0 && eos {
		if err != nil {
			return 0, err
		}

		return 0, io.EOF
	}

	return n, nil
}

// SeekFrame repositions the stream so the next frame the consumer receives is
// frame, counted in 48 kHz output frames. It returns once the whole sequence
// has run: the ring is flushed, the decoder is repositioned, position is reset
// and the ring is refilled. Concurrent seeks collapse to the latest target.
func (s *Streamer) SeekFrame(frame int64) error {
	return s.requestPosition(frame, nil, false)
}

// SwapDecoder reopens the stream with a different opener, repositions to
// target, resets the pre-modules and refills. Same gate and latest-wins
// discipline as SeekFrame.
//
// A swap always builds a new decoder: it never reuses the current one's native
// seek, because that would keep the implementation being replaced. The opener
// is handed through the gated slot rather than written to s.open here, since
// the producer goroutine is the only one that may replace the decoder.
func (s *Streamer) SwapDecoder(target int64, open Opener) error {
	if open == nil {
		return errors.New("stream: SwapDecoder requires an opener")
	}

	return s.requestPosition(target, open, true)
}

// requestPosition is the shared gate for SeekFrame and SwapDecoder. carrying an
// optional opener and a swap flag through the same sequence is what serialises
// the two: at most one reposition runs at a time, and the newest request wins.
func (s *Streamer) requestPosition(target int64, open Opener, swap bool) error {
	if target < 0 {
		return fmt.Errorf("%w: negative target %d", ErrSeekRange, target)
	}

	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if !started {
		return ErrNotStarted
	}

	s.gate.mu.Lock()
	if s.gate.closed {
		s.gate.mu.Unlock()

		return ErrClosed
	}
	if s.finished.Load() {
		s.gate.mu.Unlock()

		return ErrEndOfStream
	}

	s.seekSeq++
	seq := s.seekSeq
	s.seekTarget = target
	s.seekOpen = open
	s.seekSwap = swap
	s.gate.cond.Broadcast()

	for s.seekApplied < seq && !s.gate.closed && !s.finished.Load() {
		s.gate.cond.Wait()
	}
	closed := s.gate.closed
	applied := s.seekApplied >= seq
	err := s.seekErr
	s.gate.mu.Unlock()

	switch {
	case applied:
		return err
	case closed:
		return ErrClosed
	default:
		return ErrEndOfStream
	}
}

// Close stops the producer, wakes every blocked reader and writer, and releases
// the decoder. It is safe to call more than once and from several goroutines.
func (s *Streamer) Close() error {
	s.startMu.Lock()
	already := s.shutdown
	s.shutdown = true
	started := s.started
	stopCtx := s.stopCtx
	s.startMu.Unlock()

	if already {
		return nil
	}
	if stopCtx != nil {
		stopCtx()
	}

	// Close the gate before signalling the decoder. The producer may be parked
	// on the gate or blocked in a decoder read; the first is released here, the
	// second by the stop channel. Ordering matters: a read that returns because
	// of the stop signal must already see a closed gate, otherwise it would
	// report a planned shutdown as a decode failure.
	s.gate.close()
	s.stopDecOnce.Do(func() { close(s.stopDec) })

	if started {
		<-s.stopped
	} else {
		s.dec.Close()
	}

	return nil
}

// run is the only goroutine that touches the decoder or writes the ring.
func (s *Streamer) run() {
	defer close(s.stopped)
	// Read s.dec at exit rather than binding the receiver now: the seek
	// fallback replaces it, and the original would otherwise be closed twice
	// while the replacement leaked.
	defer func() { s.dec.Close() }()

	for !s.finished.Load() {
		if !s.produce() {
			return
		}
	}
}

// produce performs one step of the pipeline and reports whether the producer
// should keep going.
func (s *Streamer) produce() bool {
	if s.gateClosed() {
		return false
	}
	if s.applyPendingSeek() {
		return true
	}

	if len(s.pending) > 0 {
		n := s.ring.Write(s.pending)
		s.pending = s.pending[n*s.ch:]
		if n > 0 {
			s.gate.wake()
		}
		if len(s.pending) == 0 {
			return true
		}

		// The ring is full; sleep until the consumer drains below the low mark.
		return s.park(s.refillReady)
	}

	if s.ring.Len() >= s.high {
		return s.park(s.refillReady)
	}

	n, err := s.decodeChunk()
	if err != nil {
		if s.gateClosed() {
			return false
		}
		s.finish(err)

		return false
	}
	if n > 0 {
		s.pending = s.work[:n*s.ch]
	}

	return true
}

// park sleeps until ready reports true or the streamer shuts down.
func (s *Streamer) park(ready func() bool) bool {
	s.gate.mu.Lock()
	ok := s.gate.await(ready)
	s.gate.mu.Unlock()

	return ok
}

// refillReady is the producer's wake condition: a seek is waiting, or the
// consumer has drained the ring to the low watermark.
func (s *Streamer) refillReady() bool {
	return s.seekApplied < s.seekSeq || s.ring.Len() <= s.low
}

// applyPendingSeek runs the latest requested seek or swap, if any. A newer
// request that arrives while one is running simply overwrites the target (and
// opener); callers older than the applied request are released once it lands.
func (s *Streamer) applyPendingSeek() bool {
	s.gate.mu.Lock()
	if s.seekApplied >= s.seekSeq {
		s.gate.mu.Unlock()

		return false
	}
	target := s.seekTarget
	open := s.seekOpen
	swap := s.seekSwap
	seq := s.seekSeq
	s.gate.mu.Unlock()

	err := s.doSeek(target, open, swap)

	s.gate.mu.Lock()
	s.seekApplied = seq
	s.seekErr = err
	s.gate.mu.Unlock()
	s.gate.wake()

	return true
}

// doSeek is the gated seek/swap sequence. Flushing first means no consumer can
// be handed a pre-seek frame once the operation is under way; the refill at the
// end means a consumer that wakes up after it finds data, not an empty ring.
func (s *Streamer) doSeek(target int64, open Opener, swap bool) error {
	s.gate.mu.Lock()
	s.ring.Reset()
	s.pending = s.pending[:0]
	s.gate.mu.Unlock()

	if !swap {
		if err := s.reposition(target); err != nil {
			// A decoder that failed to reposition may be left at an unknown
			// frame, so the stream cannot continue honestly. A shutdown that
			// interrupted the seek is not a decode failure, so it does not
			// close Done.
			if !s.gateClosed() {
				s.finish(err)
			}

			return err
		}

		return s.complete(target)
	}

	// Capture the position and the opener before anything is replaced: both are
	// what the fallback restores if the new implementation cannot take over.
	prev, prevOpen := s.Position(), s.open
	if err := s.repositionWith(target, open); err != nil {
		// A failed swap must not leave the stream without a working decoder.
		// Put the previous implementation back at the position the swap started
		// from so the track keeps playing on it, then report the error.
		if rerr := s.repositionWith(prev, prevOpen); rerr != nil {
			joined := errors.Join(err, rerr)
			if !s.gateClosed() {
				s.finish(joined)
			}

			return joined
		}
		if rerr := s.complete(prev); rerr != nil {
			return errors.Join(err, rerr)
		}

		return err
	}

	return s.complete(target)
}

// complete finishes a reposition that landed at target: reset the pre-modules,
// publish the new position and refill the ring. Pre-modules that carry filter
// state must not blend across the jump, so they restart from a clean state.
func (s *Streamer) complete(target int64) error {
	for _, m := range s.mods {
		if err := m.Reset(); err != nil {
			return fmt.Errorf("stream: module %s reset after seek: %w", m.Name(), err)
		}
	}

	s.gate.mu.Lock()
	s.pos.Store(target)
	s.gate.mu.Unlock()

	s.refill()

	return nil
}

func (s *Streamer) reposition(target int64) error {
	if seeker, ok := s.dec.(decode.Seeker); ok {
		return seeker.SeekFrame(target)
	}

	return s.reopenAndDiscard(target, s.open)
}

// repositionWith always reopens, even for a decoder with a native seek: a swap
// is replacing the implementation, so reusing the old one's reposition would
// defeat it. The opener is explicit because the restore path passes the one
// being restored rather than the current s.open.
func (s *Streamer) repositionWith(target int64, open Opener) error {
	if err := s.reopenWith(open); err != nil {
		return err
	}
	if seeker, ok := s.dec.(decode.Seeker); ok {
		return seeker.SeekFrame(target)
	}

	return s.discardTo(target)
}

// reopenWith closes the current decoder and opens a new one, recording the
// opener on success. A decoder whose format does not match the stream's input
// is refused: the ring and the pre-module chain were configured for one layout,
// so a different rate or channel count could not be carried. A failed open
// leaves s.open untouched, so the caller still knows which opener to restore.
func (s *Streamer) reopenWith(open Opener) error {
	s.dec.Close()
	// A terminal error delivered alongside a short read belongs to the decoder
	// being replaced; carrying it over would make the new one report the old
	// one's end immediately.
	s.readErr = nil

	dec, err := open(s.stopDec)
	if err != nil {
		return err
	}
	if got := dec.Info().Format; !got.Equal(s.formats[0]) {
		dec.Close()

		return fmt.Errorf("stream: decoder reports %+v, the stream takes %+v", got, s.formats[0])
	}
	s.dec = dec
	s.open = open

	return nil
}

// reopenAndDiscard is the fallback for decoders without native seek. It reopens
// the source at frame zero and decodes forward until the target is reached,
// keeping any frames past the target so the refill does not decode them twice.
// The result is the same PCM a straight decode would produce from that frame.
func (s *Streamer) reopenAndDiscard(target int64, open Opener) error {
	if err := s.reopenWith(open); err != nil {
		return err
	}

	return s.discardTo(target)
}

// discardTo decodes forward from the current decoder's start until target,
// retaining the frames that cross it.
func (s *Streamer) discardTo(target int64) error {
	var discarded int64
	for discarded < target {
		n, err := s.decodeChunk()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: target %d is past end of stream", ErrSeekRange, target)
			}

			return err
		}
		if discarded+int64(n) <= target {
			discarded += int64(n)

			continue
		}

		need := target - discarded
		s.pending = s.work[int(need)*s.ch : n*s.ch]
		discarded = target
	}

	return nil
}

// refill fills the ring to the high watermark after a flush. Consumers are
// still parked, so the ring always has room and this cannot block on them.
func (s *Streamer) refill() {
	for s.ring.Len() < s.high {
		if len(s.pending) > 0 {
			n := s.ring.Write(s.pending)
			s.pending = s.pending[n*s.ch:]
			if n > 0 {
				s.gate.wake()
			}
			if len(s.pending) > 0 {
				// The ring is full, which is possible when a chunk is larger
				// than half the ring. Waking above is what keeps a reader that
				// went to sleep before the seek from missing the refill.
				return
			}

			continue
		}

		n, err := s.decodeChunk()
		if err != nil {
			if s.gateClosed() {
				return
			}
			s.finish(err)

			return
		}
		s.pending = s.work[:n*s.ch]
	}
	s.gate.wake()
}

// decodeChunk reads one chunk from the decoder and runs the pre-module chain in
// order. The chain is frame-preserving: core.Module.Process cannot report a
// different frame count, so a module that changes the rate would need a richer
// contract than Phase 2 defines. Each module gets one buffer holding its input
// in front and wide enough for its declared output, which is what lets a
// channel remap run in place.
func (s *Streamer) decodeChunk() (int, error) {
	if s.readErr != nil {
		return 0, s.readErr
	}

	n, err := s.dec.ReadFrames(s.work[:s.chunkFrames*s.decCh])
	if err != nil && n == 0 {
		return 0, err
	}
	if err != nil {
		// A short read may legally carry the terminal error; deliver the frames
		// and report the end on the next call.
		s.readErr = err
	}
	if n == 0 {
		return 0, errors.New("stream: decoder returned no frames and no error")
	}

	for i, m := range s.mods {
		in := s.formats[i]
		out := s.formats[i+1]
		// The buffer is both input and output: the frames frames sit at the
		// front in the input layout, and the slice is wide enough for the
		// module's output layout so a channel remap can work in place.
		width := max(in.Ch, out.Ch)
		if perr := m.Process(s.work[:n*width], n); perr != nil {
			return 0, fmt.Errorf("stream: module %s: %w", m.Name(), perr)
		}
	}

	s.decoded.Add(int64(n))

	return n, nil
}

// finish records the terminal state once and closes Done. A later call is inert
// so the seek fallback and the main loop cannot close Done twice.
func (s *Streamer) finish(err error) {
	if !s.finished.CompareAndSwap(false, true) {
		return
	}

	s.gate.mu.Lock()
	s.eos = true
	if err != nil && !errors.Is(err, io.EOF) {
		s.eosErr = err
	}
	s.gate.mu.Unlock()

	s.gate.wake()
	close(s.done)
}

// gateClosed reports a planned shutdown, as opposed to a terminal decode.
func (s *Streamer) gateClosed() bool {
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()

	return s.gate.closed
}

// provider is the shape playback.Provider will require. Pinning it here proves
// the streamer already satisfies it, without this package importing playback.
type provider interface {
	ReadFrames(dst []float32) (int, error)
}

var _ provider = (*Streamer)(nil)
