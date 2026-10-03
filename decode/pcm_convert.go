// Canonical PCM conversion shared by the byte-oriented decoders.
//
// The engine's currency is 48 kHz stereo interleaved float32 (core.CanonicalFormat):
// playback rejects any other device format and the streamer's pre-module chain
// must end there. A codec library, though, hands back whatever the file held:
// its own sample rate, 1..8 channels, and int16 or int32 samples. package
// decode therefore converts at the decoder boundary rather than pushing the
// problem downstream, which is also why the direct decoder (see flac.go)
// converts on its own.
//
// One implementation lives here because go-aac, go-mp3, go-wav and the direct
// readers all need the same three conversions. A per-codec copy would be four
// chances to disagree about rounding or channel order.

package decode

import (
	"math"

	"github.com/dlcuy22/molo/core"
)

// pcmConverter turns interleaved integer PCM from a codec library into
// interleaved float32 at core.CanonicalFormat.
//
// The source rate is fixed at construction, so the only per-call state is the
// fractional read position. It is not safe for concurrent use, matching the
// decoders that own it.
type pcmConverter struct {
	srcRate     int
	srcChannels int
	// bytesPS is the stored width of one sample, 1..4.
	bytesPS int

	// ratio is srcRate/48000. It is 1 for the common case, which lets the
	// sample loop skip interpolation entirely.
	ratio float64

	// src holds decoded interleaved stereo float32 not yet emitted. Only the
	// fraction the resampler has not consumed is retained.
	src []float32
	// frac is the fractional frame position within src.
	frac float64

	// padded records that the one-frame interpolation tail was appended, so an
	// end-of-stream signal does not append it twice.
	padded bool

	// carry holds a partial source frame from the end of the last chunk. A codec
	// library's Read may stop mid-frame (go-wav does), and dropping that
	// fragment would shift every later sample by a fraction of a frame, so it is
	// kept until the next chunk completes it.
	carry []byte
}

// newPCMConverter builds a converter for a source stream shape. A rate or
// channel count of zero is rejected by the caller before this; the caller
// derives bytesPS from its own bit depth.
func newPCMConverter(srcRate, srcChannels, bytesPS int) *pcmConverter {
	return &pcmConverter{
		srcRate:     srcRate,
		srcChannels: srcChannels,
		bytesPS:     bytesPS,
		ratio:       float64(srcRate) / float64(core.CanonicalFormat.Rate),
	}
}

// stereo reports whether the rate already is the canonical one, so the caller
// can skip interpolation. It is a method rather than a field so the invariant
// cannot drift from ratio.
func (c *pcmConverter) stereo() bool { return c.ratio == 1 }

// feed hands one chunk of integer PCM to the converter. A chunk that ends
// mid-frame is not an error: the fragment is carried and prepended to the next
// call, so a codec library that returns short reads still produces contiguous
// audio. A chunk smaller than one frame and no larger than the carry capacity
// is simply stored.
//
// It is the entry point every byte-oriented decoder uses; none of them should
// slice a chunk to a frame boundary themselves.
func (c *pcmConverter) feed(raw []byte) {
	frameBytes := c.srcChannels * c.bytesPS
	if frameBytes <= 0 {
		return
	}

	whole := len(raw) / frameBytes * frameBytes
	if whole == len(raw) && len(c.carry) == 0 {
		c.ingest(raw)

		return
	}

	// Complete any carried fragment first, then take whole frames, then carry
	// the remainder.
	if len(c.carry) > 0 {
		need := frameBytes - len(c.carry)
		if need > len(raw) {
			c.carry = append(c.carry, raw...)

			return
		}
		c.carry = append(c.carry, raw[:need]...)
		c.ingest(c.carry)
		c.carry = c.carry[:0]
		raw = raw[need:]
		whole = len(raw) / frameBytes * frameBytes
	}
	if whole > 0 {
		c.ingest(raw[:whole])
		raw = raw[whole:]
	}
	if len(raw) > 0 {
		c.carry = append(c.carry[:0], raw...)
	}
}

// ingest decodes whole interleaved source frames from raw into c.src. raw must
// be a whole number of frames; a caller with an arbitrary chunk uses feed.
func (c *pcmConverter) ingest(raw []byte) int {
	frameBytes := c.srcChannels * c.bytesPS
	frames := len(raw) / frameBytes
	if frames == 0 {
		return 0
	}

	scale := c.scale()

	// int16 is by far the most common stored width, so it gets a loop the
	// compiler can keep in registers; wider and 8-bit samples take the general
	// path.
	if c.bytesPS == 2 {
		for i := range frames {
			base := i * c.srcChannels * 2
			left := float32(int16(le16(raw[base:]))) * scale
			right := left
			if c.srcChannels > 1 {
				right = float32(int16(le16(raw[base+2:]))) * scale
			}
			c.src = append(c.src, left, right)
		}

		return frames
	}

	for i := range frames {
		base := i * frameBytes
		left := float32(signExtend(raw[base:base+c.bytesPS])) * scale
		right := left
		if c.srcChannels > 1 {
			off := base + c.bytesPS
			right = float32(signExtend(raw[off:off+c.bytesPS])) * scale
		}
		c.src = append(c.src, left, right)
	}

	return frames
}

// scale is the factor from a stored integer to a float in [-1, 1].
// A 16-bit full-scale sample (32768) maps to 1.0, so the divisor is 2^(bits-1)
// rather than 2^bits: the extra step would make every file 6 dB quiet.
func (c *pcmConverter) scale() float32 {
	return 1 / float32(uint64(1)<<(c.bytesPS*8-1))
}

// resampleInto writes up to len(out)/2 canonical stereo frames into out and
// returns the frame count. It emits only what it can finish: when the source is
// exhausted a caller must call markEOF and then drain once more, because the
// final output interval needs one sample beyond the last real one.
func (c *pcmConverter) resampleInto(out []float32) int {
	frames := len(out) / 2
	n := 0

	if c.stereo() {
		for n < frames {
			i := int(c.frac)
			if i*2+1 >= len(c.src) {
				break
			}
			out[n*2] = c.src[i*2]
			out[n*2+1] = c.src[i*2+1]
			n++
			c.frac++
		}
	} else {
		for n < frames {
			i := int(c.frac)
			if (i+1)*2+1 >= len(c.src) {
				break
			}
			f := float32(c.frac - float64(i))
			out[n*2] = c.src[i*2] + f*(c.src[i*2+2]-c.src[i*2])
			out[n*2+1] = c.src[i*2+1] + f*(c.src[i*2+3]-c.src[i*2+1])
			n++
			c.frac += c.ratio
		}
	}

	c.dropConsumed()

	return n
}

// markEOF appends one duplicated final frame so the last output interval can be
// interpolated. Calling it twice is harmless. A sub-frame fragment still in the
// carry can never become audio, so it is dropped here.
func (c *pcmConverter) markEOF() {
	c.carry = c.carry[:0]
	if c.padded || c.stereo() || len(c.src) < 2 {
		return
	}
	c.src = append(c.src, c.src[len(c.src)-2], c.src[len(c.src)-1])
	c.padded = true
}

// dropConsumed releases the frames the resampler has passed.
func (c *pcmConverter) dropConsumed() {
	consumed := int(c.frac)
	if consumed > len(c.src)/2 {
		consumed = len(c.src) / 2
	}
	if consumed > 0 {
		c.src = c.src[consumed*2:]
		c.frac -= float64(consumed)
	}
}

// reset clears the conversion state after a seek, where whatever the resampler
// still held belongs to the abandoned position.
func (c *pcmConverter) reset() {
	c.src = c.src[:0]
	c.carry = c.carry[:0]
	c.frac = 0
	c.padded = false
}

// feedFloat hands one chunk of interleaved little-endian float32 PCM to the
// converter. It exists because a codec library can produce float natively
// (go-mp3 with WithF32), and a decoder should not have to round those samples
// through int16 just to reuse the shared resampler. Mono is duplicated to
// stereo, exactly as the integer path does.
//
// Truncation, not rejection, is deliberate: a chunk that ends mid-sample is a
// short read, and the caller will not have more of that sample.
func (c *pcmConverter) feedFloat(raw []byte) {
	if c.srcChannels < 1 {
		return
	}
	for i := 0; i+4 <= len(raw); i += 4 {
		s := float32frombits(le32(raw[i:]))
		c.src = append(c.src, s)
		if c.srcChannels == 1 {
			c.src = append(c.src, s)
		}
	}
}

// skipFrameBytes discards up to want bytes from the front of a chunk, reporting
// how many it actually dropped. A caller uses it to trim leading encoder
// priming without slicing a chunk to a frame boundary itself.
func (c *pcmConverter) skipFrameBytes(raw []byte, want int64) (rest []byte, dropped int64) {
	if want <= 0 {
		return raw, 0
	}
	if want >= int64(len(raw)) {
		return nil, int64(len(raw))
	}

	return raw[want:], want
}

// canonicalFrames converts a source sample count at rate into 48000 Hz frames.
// ok is false when the result is not exact or the rate is unusable, in which
// case the caller reports an unknown total rather than a rounded one.
func canonicalFrames(total uint64, rate int) (int64, bool) {
	if rate <= 0 || total == 0 {
		return 0, false
	}
	if rate == core.CanonicalFormat.Rate {
		// Guard the int64 conversion: a declared total past MaxInt64 would wrap.
		if total > uint64(1)<<62 {
			return 0, false
		}

		return int64(total), true
	}
	scaled := total * uint64(core.CanonicalFormat.Rate)
	if scaled%uint64(rate) != 0 {
		return 0, false
	}

	return int64(scaled / uint64(rate)), true
}

// canonicalSamples converts a canonical frame index back to a source sample
// index, rounding toward the start of the canonical frame so a seek never lands
// past its target.
func canonicalSamples(frame int64, rate int) int64 {
	if rate == core.CanonicalFormat.Rate {
		return frame
	}

	return int64(float64(frame) * float64(rate) / float64(core.CanonicalFormat.Rate))
}

// signExtend reads a little-endian two's complement integer of len(b) bytes.
func signExtend(b []byte) int32 {
	var v uint32
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint32(b[i])
	}

	shift := uint(32 - len(b)*8)

	return int32(v<<shift) >> shift
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// float32frombits is math.Float32frombits without the math import here, so the
// converter file stays free of a dependency it uses once.
func float32frombits(b uint32) float32 {
	return math.Float32frombits(b)
}
