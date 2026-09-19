// Shared plumbing for decoders that adapt a byte-stream codec library.
//
// Several codecs (go-flac, go-aac, go-mp3, go-wav) expose the same shape: an
// io.Reader of interleaved integer PCM. Their decoders in this package differ
// only in how they produce that PCM and how they seek. This file holds the two
// pieces they would otherwise each reimplement: the ReadFrames delivery loop
// and the constants that bound it.

package decode

import (
	"errors"
	"io"
)

const (
	// pcmRawReadBytes bounds one read from a codec library. The converter
	// carries a partial frame forward, so this need not be a multiple of the
	// source frame size.
	pcmRawReadBytes = 64 * 1024

	// pcmOutFrames is one block of canonical stereo output, about 100 ms. It is
	// the unit the converter finishes in, so a seek discards at most this much
	// resampling work.
	pcmOutFrames = 4800
)

// rawFrames is the read buffer one codec library chunk goes into. It is
// allocated once per decoder; the converter carries any partial frame, so the
// size only has to be a reasonable I/O batch.
func rawFrames() []byte { return make([]byte, pcmRawReadBytes) }

// outBlock is one canonical stereo output block, aliased by a decoder's pending
// slice between fills.
func outBlock() []float32 { return make([]float32, pcmOutFrames*2) }

// byteSource is the minimal decode surface a byte-stream codec library exposes:
// read interleaved PCM, and (only for the seekable ones) reposition.
type byteSource interface {
	io.Reader
}

// pcmFillLoop is the shared fill body of every byte-stream decoder.
//
// It converts until one output block is ready and returns io.EOF only when the
// source is exhausted and nothing is buffered. read pulls the next raw chunk;
// an err from it that is not io.EOF is fatal once the chunk it returned has been
// converted. skipLeading, when non-nil, trims that many leading bytes (encoder
// priming); it is called once per chunk until satisfied and its counter is kept
// by the caller.
//
// Keeping this in one place is what stops four decoders from each getting the
// end-of-stream and carry handling subtly different.
func pcmFillLoop(
	conv *pcmConverter,
	out []float32,
	pending *[]float32,
	eof *bool,
	read func([]byte) (int, error),
	raw []byte,
	feed func([]byte),
) error {
	for {
		if n := conv.resampleInto(out); n > 0 {
			*pending = out[:n*2]

			return nil
		}
		if *eof {
			return io.EOF
		}

		n, err := read(raw)
		if n > 0 {
			feed(raw[:n])
		}
		if err != nil {
			if n == 0 && !errors.Is(err, io.EOF) {
				return err
			}
			// The interpolator needs one frame past the last real one to finish
			// the final output interval; a sub-frame fragment is dropped there.
			*eof = true
			conv.markEOF()
		}
	}
}

// readCanonical is the ReadFrames body shared by the byte-stream decoders.
//
// dst receives whole canonical stereo frames. out is the decoder's reusable
// block and pending aliases it until consumed; fill produces the next block and
// returns io.EOF at end of stream. pos is advanced by the delivered count and,
// when total is non-negative, delivery is clamped to it so a converter's
// interpolation tail can never emit past the length Info promised.
func readCanonical(
	dst []float32,
	out []float32,
	pending *[]float32,
	fill func() error,
	pos *int64,
	total int64,
	closed bool,
) (int, error) {
	if closed {
		return 0, ErrClosed
	}
	if len(dst) < 2 {
		return 0, nil
	}

	frames := 0
	for frames*2 < len(dst) {
		if len(*pending) == 0 {
			if err := fill(); err != nil {
				if errors.Is(err, io.EOF) {
					if frames == 0 {
						return 0, io.EOF
					}

					return frames, nil
				}

				return frames, err
			}
		}

		// Never deliver past the declared end. Info already promised the exact
		// total, and the resampler's tail can finish one frame long.
		if total >= 0 {
			left := total - *pos
			if left <= 0 {
				if frames == 0 {
					return 0, io.EOF
				}

				return frames, nil
			}
			if want := int(left) * 2; want < len(*pending) {
				*pending = (*pending)[:want]
			}
		}

		want := len(dst)/2 - frames
		n := min(len(*pending)/2, want)
		copy(dst[frames*2:(frames+n)*2], (*pending)[:n*2])
		*pending = (*pending)[n*2:]
		frames += n
		*pos += int64(n)
	}

	_ = out

	return frames, nil
}

// canonicalFramesOf converts a source sample index back to canonical frames,
// rounding toward the start.
func canonicalFramesOf(sample int64, conv *pcmConverter) int64 {
	if conv.ratio == 1 {
		return sample
	}

	return int64(float64(sample) / conv.ratio)
}
