package playback

import (
	"encoding/binary"
	"io"
	"math"
	"sync/atomic"
)

// pcmReader adapts a Provider to the io.Reader that push APIs like oto want.
//
// The inversion is the whole point: the engine produces PCM when asked, but
// oto pulls it on its own thread. This type is that bridge, and it must be
// careful about one thing above all: it must never return (0, nil). oto's
// multiplexer treats a zero-length successful read as "nothing to do" and
// sleeps a millisecond, so a Provider that answers (0, nil) would turn the
// audio path into a poll loop. Every path here either returns bytes, an error,
// or waits for the Provider.
//
// It is also the boundary where frames become little-endian float32 bytes,
// which is why the byte order is spelled out here and nowhere else.
type pcmReader struct {
	provider Provider

	// ch is the interleaved channel count, needed to round a byte count down
	// to whole frames and to size the scratch buffer.
	ch int

	// scratch is reused across reads. oto calls Read on its mux goroutine,
	// once at a time, so no lock is needed; the single consumer is part of the
	// contract this type relies on.
	scratch []float32

	// closed lets Close unblock a Read that is parked inside a Provider that
	// has nothing to give. It is checked before every pull, and a closed
	// reader reports io.EOF so oto stops asking.
	closed atomic.Bool

	// pending holds a terminal error whose frames were already delivered. A
	// short read may legally carry io.EOF; returning the frames and the error
	// together would make oto drop the tail, so the error waits for the next
	// call. Only the single mux goroutine touches it.
	pending error
}

// newPCMReader wraps p. A non-positive channel count is impossible on the
// device path (Open validates the format first) but is clamped so a misuse
// divides by zero rather than panicking in the audio thread.
func newPCMReader(p Provider, ch int) *pcmReader {
	if ch < 1 {
		ch = 1
	}

	return &pcmReader{provider: p, ch: ch}
}

// Read fills p with whole frames and returns the byte count. A partial trailing
// frame in p is left untouched, matching oto's own alignment requirement.
func (r *pcmReader) Read(p []byte) (int, error) {
	bytesPerFrame := r.ch * 4
	frames := len(p) / bytesPerFrame
	if frames == 0 {
		return 0, nil
	}

	samples := frames * r.ch
	if cap(r.scratch) < samples {
		// Allocation happens on oto's mux goroutine, not on the audio callback,
		// and only when the caller hands a larger buffer than before. oto's
		// player buffer size is fixed after the first read, so this settles.
		r.scratch = make([]float32, samples)
	}
	buf := r.scratch[:samples]

	for {
		if r.closed.Load() {
			return 0, io.EOF
		}
		if r.pending != nil {
			err := r.pending
			r.pending = nil

			return 0, err
		}

		n, err := r.provider.ReadFrames(buf)
		if n > 0 {
			encodeFloat32LE(p, buf[:n*r.ch])

			// A short read that also carries the terminal error delivers the
			// frames now and reports the error on the next call, which is what
			// io.Reader permits and what keeps this path (0, nil)-free.
			r.pending = err

			return n * bytesPerFrame, nil
		}
		if err != nil {
			return 0, err
		}
		// n == 0 and err == nil: the provider had nothing and did not block.
		// Loop rather than hand oto a zero-length success.
	}
}

// close stops further reads. It does not close the Provider: ownership of the
// streamer stays with the caller, who may hand the same stream to another
// device after a seek.
func (r *pcmReader) close() { r.closed.Store(true) }

// encodeFloat32LE writes samples into dst as little-endian float32. It is the
// one place byte order is decided, so oto's FormatFloat32LE always agrees with
// how the engine stores samples.
func encodeFloat32LE(dst []byte, samples []float32) {
	for i, s := range samples {
		binary.LittleEndian.PutUint32(dst[4*i:], math.Float32bits(s))
	}
}
