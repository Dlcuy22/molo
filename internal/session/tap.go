package session

import (
	"sync"
	"sync/atomic"
)

// tapRingFrames is the tap buffer depth, about 680 ms at 48 kHz. It is deep
// enough that a visualizer polling at 60 Hz never sees a gap and shallow enough
// that a consumer which stops reading costs a bounded 128 KiB.
const tapRingFrames = 1 << 15

// tapScratchFrames bounds one downmix batch inside publish, at 48 kHz about
// 21 ms. It exists so publishing a whole 100 ms device buffer still copies in
// cache-sized pieces and never allocates.
const tapScratchFrames = 1024

// tap is the visualizer feed. It is a second ring, entirely separate from the
// streamer's: the playback ring is forward-only and feeding the device, so
// observing it would consume audio. The provider instead writes a mono downmix
// into this ring after the gain has run, which is exactly what the listener
// hears.
//
// The writer is the audio path and must never wait. It only ever TryLocks, so a
// consumer that is mid-copy makes the writer skip that buffer rather than
// stall. TryLock is not just a politeness: an overwrite-oldest ring lets the
// writer reuse a slot the consumer may be reading, which is a data race unless
// the copy is mutually exclusive. A slow consumer therefore loses frames, never
// the audio thread.
type tap struct {
	// active gates the whole fast path. It is one atomic load for the writer;
	// when no consumer has attached, publish does nothing else.
	active atomic.Bool

	mu   sync.Mutex
	buf  []float32
	mask uint64
	// read and write are plain counters protected by mu.
	read  uint64
	write uint64

	// scratch holds one downmix batch, reused across publishes so the audio
	// path never allocates. Publishing a device-sized buffer is a handful of
	// batches, each a bounded downmix plus ring copy under mu.
	scratch []float32

	// dropped counts frames lost to a slow consumer: overwritten by the writer
	// or skipped by the reader. A UI reads it to tell "paused" from "too slow".
	dropped atomic.Int64
}

func newTap() *tap { return newTapRing(tapRingFrames) }

// newTapRing builds a tap with an explicit ring depth, which lets a test drive
// the overwrite path without writing 32768 frames. The depth must be a power of
// two, matching the mask.
func newTapRing(frames int) *tap {
	return &tap{
		buf:     make([]float32, frames),
		mask:    uint64(frames - 1),
		scratch: make([]float32, tapScratchFrames),
	}
}

// attach marks the tap as having a consumer. It is what makes publish do work;
// calling it more than once is harmless. Read calls it, so merely holding a Tap
// handle costs the audio path nothing until the consumer actually asks for
// samples.
func (t *tap) attach() { t.active.Store(true) }

// publish writes frames of interleaved dst, downmixed to mono, into the ring.
// It is the audio-path entry point: it must not block or allocate. The downmix
// runs into the preallocated scratch and the ring copy is bounded, so the
// critical section is a few microseconds; TryLock guarantees the audio thread
// never waits for a consumer, dropping one buffer rather than stalling.
func (t *tap) publish(dst []float32, frames, ch int) {
	if !t.active.Load() {
		return
	}
	if ch < 1 {
		ch = 1
	}
	if want := len(dst) / ch; frames > want {
		frames = want
	}
	if frames <= 0 {
		return
	}

	if !t.mu.TryLock() {
		// A consumer is mid-copy. Dropping one buffer keeps the audio thread
		// moving; the consumer will get the next one.
		t.dropped.Add(int64(frames))

		return
	}
	defer t.mu.Unlock()

	for done := 0; done < frames; {
		n := min(tapScratchFrames, frames-done)
		base := done * ch
		for f := 0; f < n; f++ {
			var mono float32
			for c := 0; c < ch; c++ {
				mono += dst[base+f*ch+c]
			}
			t.scratch[f] = mono / float32(ch)
		}
		t.writeMonoLocked(t.scratch[:n])
		done += n
	}
}

// writeMono is the single-threaded entry point used by tests. It takes the lock
// the writer normally already holds.
func (t *tap) writeMono(p []float32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writeMonoLocked(p)
}

// writeMonoLocked appends mono frames, overwriting the oldest when full. The
// caller holds mu.
func (t *tap) writeMonoLocked(p []float32) {
	capacity := int64(len(t.buf))
	frames := int64(len(p))
	if frames > capacity {
		// A single write larger than the ring can only keep its newest frames.
		over := frames - capacity
		p = p[over:]
		frames = capacity
		t.dropped.Add(over)
	}

	start := t.write
	if lap := int64(start-t.read) + frames - capacity; lap > 0 {
		// The writer lapped the reader. Only the newest ring-full worth of data
		// can survive, so advance the read cursor over the overwritten frames.
		t.read += uint64(lap)
		t.dropped.Add(lap)
	}

	for done := int64(0); done < frames; {
		slot := (start + uint64(done)) & t.mask
		run := int64(int64(t.mask+1) - int64(slot))
		if run > frames-done {
			run = frames - done
		}
		copy(t.buf[int(slot):int(slot)+int(run)], p[done:done+run])
		done += run
	}

	t.write = start + uint64(frames)
}

// readMono copies up to len(dst) frames of the newest buffered audio. When more
// is buffered than dst holds it advances past the oldest frames rather than
// returning a stale prefix, which is what a live visualizer wants after an idle
// gap. It takes the lock, so a concurrently publishing writer either waits for
// a bounded copy or drops its buffer.
func (t *tap) readMono(dst []float32) int {
	if len(dst) == 0 {
		return 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	avail := int64(t.write - t.read)
	if avail <= 0 {
		return 0
	}
	if avail > int64(len(dst)) {
		skip := uint64(avail - int64(len(dst)))
		t.read += skip
		t.dropped.Add(int64(skip))
		avail = int64(len(dst))
	}

	n := int(avail)
	for done := 0; done < n; {
		slot := (t.read + uint64(done)) & t.mask
		run := int(int64(t.mask+1) - int64(slot))
		if run > n-done {
			run = n - done
		}
		copy(dst[done:done+run], t.buf[int(slot):int(slot)+run])
		done += run
	}
	t.read += uint64(n)

	return n
}

// Read is the consumer side: it copies up to len(dst) mono frames in time
// order and reports zero when nothing is buffered. The first call marks the tap
// live, which is what starts the audio path publishing; a Tap handle that is
// never read therefore costs the audio path nothing. It is safe with no active
// producer, which is what a UI holds between tracks.
func (t *tap) Read(dst []float32) int {
	t.attach()

	return t.readMono(dst)
}

// droppedFrames reports how many frames a slow consumer cost.
func (t *tap) droppedFrames() int64 { return t.dropped.Load() }
