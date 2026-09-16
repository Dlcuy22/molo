// Package stream bridges a decode.Decoder to a real-time consumer through a
// single-producer, single-consumer ring buffer.
//
// The ring is deliberately dumb: it copies interleaved float32 frames between
// two parties and nothing else. Waiting, backpressure, seeking and position
// accounting live in the streamer, which owns the one goroutine that decodes.
//
// Concurrency contract: exactly one goroutine writes and one goroutine reads.
// The cursors are atomic and the hot paths never allocate, so a second
// concurrent writer is undefined behaviour rather than something the ring
// detects and defends against.
package stream

import (
	"math/bits"
	"sync/atomic"
)

// Ring is a power-of-two SPSC circular buffer of interleaved float32 frames.
//
// Capacity is measured in frames (one frame is Ch samples) and is always a
// power of two so every index maps to a slot with a bitmask instead of a
// modulo. Write and Read report frames, not samples, and both are non-blocking
// and may transfer less than requested.
type Ring struct {
	buf  []float32
	ch   int
	mask uint64

	// read and write are frame counters, not slot indices; they increase
	// forever and the bitmask folds them onto the buffer. Monotonic counters
	// make fullness a subtraction with no ambiguity between empty and full.
	read  atomic.Uint64
	write atomic.Uint64
}

// NewRing returns a ring with at least frames frames of capacity. The request
// is rounded up to the next power of two, which is why Cap can exceed it. A
// non-positive frame or channel count panics: rounding a zero up would hide a
// caller bug behind a ring that can never hold anything.
func NewRing(frames, ch int) *Ring {
	if frames < 1 {
		panic("stream: NewRing requires a positive frame capacity")
	}
	if ch < 1 {
		panic("stream: NewRing requires a positive channel count")
	}

	capacity := uint64(1) << bits.Len64(uint64(frames-1))

	return &Ring{
		buf:  make([]float32, capacity*uint64(ch)),
		ch:   ch,
		mask: capacity - 1,
	}
}

// Cap is the real capacity in frames after rounding, which is what Len and
// Space are relative to.
func (r *Ring) Cap() int64 { return int64(r.mask + 1) }

// Ch is the interleaved channel count the ring was created with.
func (r *Ring) Ch() int { return r.ch }

// Len is the number of frames currently buffered.
func (r *Ring) Len() int64 {
	return int64(r.write.Load() - r.read.Load())
}

// Space is the number of frames the next Write can accept.
func (r *Ring) Space() int64 { return r.Cap() - r.Len() }

// Write copies whole frames from p and returns how many were stored. A partial
// trailing frame is ignored, and a full ring accepts nothing.
func (r *Ring) Write(p []float32) int {
	frames := len(p) / r.ch

	start := r.write.Load()
	room := r.Cap() - int64(start-r.read.Load())
	if int64(frames) > room {
		frames = int(room)
	}

	for done := 0; done < frames; {
		slot := (start + uint64(done)) & r.mask
		run := int(int64(r.mask+1) - int64(slot))
		if run > frames-done {
			run = frames - done
		}
		copy(r.buf[int(slot)*r.ch:(int(slot)+run)*r.ch], p[done*r.ch:(done+run)*r.ch])
		done += run
	}

	// Release the new frames to the consumer only after the data is in place.
	r.write.Store(start + uint64(frames))

	return frames
}

// Read copies whole frames into p and returns how many were available. It never
// waits for data and never writes past len(p).
func (r *Ring) Read(p []float32) int {
	frames := len(p) / r.ch

	start := r.read.Load()
	avail := int64(r.write.Load() - start)
	if int64(frames) > avail {
		frames = int(avail)
	}

	for done := 0; done < frames; {
		slot := (start + uint64(done)) & r.mask
		run := int(int64(r.mask+1) - int64(slot))
		if run > frames-done {
			run = frames - done
		}
		copy(p[done*r.ch:(done+run)*r.ch], r.buf[int(slot)*r.ch:(int(slot)+run)*r.ch])
		done += run
	}

	r.read.Store(start + uint64(frames))

	return frames
}

// Reset drops every unread frame by moving the write cursor back to the read
// cursor. It must not run concurrently with Read: a reader that snapshotted the
// old write cursor would advance the read cursor past the new one and make the
// ring look almost full. The streamer calls it only while holding the lock that
// also excludes consumers.
func (r *Ring) Reset() {
	r.write.Store(r.read.Load())
}
