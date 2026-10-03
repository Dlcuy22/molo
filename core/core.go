// Package core defines the data currency shared by every layer of the player:
// interleaved float32 PCM at a known frame format, frame-domain positions, and
// the Module contract that pre- and post-ring processing both satisfy.
//
// Nothing in this package may import another player package; every other
// package depends on core.
package core

import "time"

// SampleFormat identifies how one PCM sample is encoded on the wire or in a
// device buffer. The engine itself works in F32 regardless of the source
// format; the others exist so factories can describe what they decode natively.
type SampleFormat uint8

const (
	// F32 is 32-bit interleaved floating-point PCM, the engine's own currency.
	F32 SampleFormat = iota
	// S16 is 16-bit interleaved signed-integer PCM.
	S16
	// S32 is 32-bit interleaved signed-integer PCM.
	S32
)

// bytes returns the encoded width of a sample, or 0 when the format is unknown
// so callers can detect garbage rather than mis-size buffers.
func (f SampleFormat) bytes() int {
	switch f {
	case F32, S32:
		return 4
	case S16:
		return 2
	default:
		return 0
	}
}

// FrameFormat describes an interleaved PCM stream. Rate is in Hz and Ch is the
// interleaved channel count, so one frame holds Ch samples.
type FrameFormat struct {
	Rate int
	Ch   int
	Fmt  SampleFormat
}

// BytesPerFrame is the size of one interleaved frame in bytes.
func (f FrameFormat) BytesPerFrame() int {
	return f.Ch * f.Fmt.bytes()
}

// Equal reports whether two formats produce identical PCM layouts.
func (f FrameFormat) Equal(o FrameFormat) bool {
	return f == o
}

// StreamInfo describes a decoded stream. TotalFrames is in 48 kHz output frames
// and is -1 while unknown, which is the normal state right after Open.
type StreamInfo struct {
	Format      FrameFormat
	TotalFrames int64
	Bitrate     int

	// SourceSamples and SourceRate hold the exact length in the source domain,
	// so a source whose rate does not divide evenly into 48 kHz (44.1 kHz is
	// the common case) still reports a duration. Zero means unknown.
	SourceSamples int64
	SourceRate    int
}

// Duration converts the stream length to wall-clock time. The source domain is
// preferred when it is known: TotalFrames is the output frame count the
// delivery path clamps to and is -1 whenever the source rate does not divide
// exactly into 48 kHz, while SourceSamples is the exact source-domain length
// that survives that case. Unknown lengths and formats without a usable rate
// yield zero so callers can treat "no duration" as "not yet known" instead of
// an error.
func (s StreamInfo) Duration() time.Duration {
	if s.SourceSamples > 0 && s.SourceRate > 0 {
		return time.Duration(s.SourceSamples) * time.Second / time.Duration(s.SourceRate)
	}
	if s.TotalFrames < 0 || s.Format.Rate <= 0 {
		return 0
	}

	return time.Duration(s.TotalFrames) * time.Second / time.Duration(s.Format.Rate)
}

// CanonicalFormat is the one PCM layout the engine speaks end to end: every
// decoder output is normalized to it before the ring, the device is opened for
// it, and positions are counted in its frames. It lives here, in the only
// package every layer already depends on, so a rate change breaks compilation
// in one place instead of drifting silently between packages.
var CanonicalFormat = FrameFormat{Rate: 48000, Ch: 2, Fmt: F32}

// Module is one insertable stage of the pipeline. Pre-ring modules may
// allocate; post-ring modules are on the real-time path and must not.
type Module interface {
	Name() string
	Configure(in FrameFormat) (out FrameFormat, err error)
	Process(buf []float32, frames int) error
	Reset() error
}

// DurationMode selects how much work a probe may do to learn a stream length.
type DurationMode uint8

const (
	DurationUnknown DurationMode = iota // open as fast as possible, leave the total unknown
	DurationProbe                       // read the tail of the file, cheap
	DurationScan                        // read and decode the whole file
)
