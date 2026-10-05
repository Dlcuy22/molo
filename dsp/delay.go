// Clean-room note: this file implements a fixed-size ring buffer using standard
// circular-buffer lookahead-delay techniques. It is not a translation of any
// GPL or LGPL C code.

package dsp

import "math"

// delayLine stores a circular ring of interleaved audio frames for integer and fractional lookahead delays.
// An instance is not safe for concurrent use across goroutines.
type delayLine struct {
	buf       []float64
	maxFrames int
	ch        int
	writePos  int
}

// newDelayLine allocates a circular buffer clamped to at least one frame and one channel.
func newDelayLine(maxFrames, ch int) *delayLine {
	if maxFrames < 1 {
		maxFrames = 1
	}
	if ch < 1 {
		ch = 1
	}
	return &delayLine{
		buf:       make([]float64, maxFrames*ch),
		maxFrames: maxFrames,
		ch:        ch,
		writePos:  maxFrames - 1,
	}
}

// writeFrame pushes one multichannel frame into the ring without heap allocation.
func (d *delayLine) writeFrame(frame []float64) {
	d.writePos++
	if d.writePos == d.maxFrames {
		d.writePos = 0
	}
	start := d.writePos * d.ch
	n := copy(d.buf[start:start+d.ch], frame)
	for i := n; i < d.ch; i++ {
		d.buf[start+i] = 0
	}
}

// readFrame writes the delayed multichannel frame into dst, with delay 0 reading the latest write.
func (d *delayLine) readFrame(delayFrames int, dst []float64) {
	if delayFrames < 0 {
		delayFrames = 0
	} else if delayFrames >= d.maxFrames {
		delayFrames = d.maxFrames - 1
	}

	idx := d.writePos - delayFrames
	if idx < 0 {
		idx += d.maxFrames
	}

	start := idx * d.ch
	copy(dst, d.buf[start:start+d.ch])
}

// readFrameFrac interpolates linearly between neighbouring integer frames for fractional delays.
func (d *delayLine) readFrameFrac(delayFrames float64, dst []float64) {
	if math.IsNaN(delayFrames) || delayFrames <= 0 {
		d.readFrame(0, dst)
		return
	}
	maxDelay := float64(d.maxFrames - 1)
	if delayFrames >= maxDelay {
		d.readFrame(d.maxFrames-1, dst)
		return
	}

	d0 := int(delayFrames)
	frac := delayFrames - float64(d0)
	if frac == 0 {
		d.readFrame(d0, dst)
		return
	}

	d1 := d0 + 1

	idx0 := d.writePos - d0
	if idx0 < 0 {
		idx0 += d.maxFrames
	}
	idx1 := d.writePos - d1
	if idx1 < 0 {
		idx1 += d.maxFrames
	}

	start0 := idx0 * d.ch
	start1 := idx1 * d.ch

	n := d.ch
	if len(dst) < n {
		n = len(dst)
	}
	for c := 0; c < n; c++ {
		s0 := d.buf[start0+c]
		s1 := d.buf[start1+c]
		dst[c] = s0 + frac*(s1-s0)
	}
}

// process writes in and reads the frame delayed by delayFrames in one step.
func (d *delayLine) process(in, out []float64, delayFrames int) {
	d.writeFrame(in)
	d.readFrame(delayFrames, out)
}

// reset zeroes all stored history and realigns the ring write cursor.
func (d *delayLine) reset() {
	clear(d.buf)
	d.writePos = d.maxFrames - 1
}
