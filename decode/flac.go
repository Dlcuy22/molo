// FLAC decoding through github.com/tphakala/go-flac. This is the lossless path:
// it needs no runtime shared library and no cgo, and it decodes the native FLAC
// bitstream (fLaC) rather than the Ogg FLAC mapping.
//
// The container is go-flac's own: FLAC has no page index and no granule, so the
// decoder binary-searches frames with STREAMINFO's max frame size and lands with
// a short decode. That is the format's only seek strategy; there is no cheaper
// index to build, because a FLAC frame does not carry its own length.
//
// The decoder normalizes whatever the file holds (any FLAC sample rate, 1..8
// channels, 4..32-bit) to the engine's canonical 48000 Hz stereo float32, the
// same contract the Opus decoders satisfy. The device and the ring accept only
// that format, so a decoder that forwarded the file's own rate would simply fail
// to open on a 44.1 kHz stream.
//
// Dependencies: github.com/tphakala/go-flac.

package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/dlcuy22/player/core"
	"github.com/tphakala/go-flac/pcm"
)

// flacRawReadBytes bounds one read from go-flac. It is rounded down to a whole
// number of interleaved source frames so a sample is never split across reads.
const flacRawReadBytes = 64 * 1024

// flacOutFrames is one block of canonical stereo output, about 100 ms. It is the
// unit the resampler finishes in, so a seek discards at most this much work.
const flacOutFrames = 4800

// init registers the lossless decoder.
func init() {
	Register(NewFlacFactory())
}

// FlacFactory decodes native FLAC without cgo.
type FlacFactory struct{}

func NewFlacFactory() *FlacFactory { return &FlacFactory{} }

func (f *FlacFactory) Name() string { return "flac" }

// FriendlyName is the label a UI shows for this codec. FLAC is the lossless
// choice, which is the one property that distinguishes it from every Opus
// variant.
func (f *FlacFactory) FriendlyName() string { return "Lossless" }

// Weight only breaks ties between codecs that claim the same extension. No other
// factory claims .flac, so this value never competes.
func (f *FlacFactory) Weight() int { return 90 }

func (f *FlacFactory) Exts() []string { return []string{".flac"} }

// Match claims the native FLAC marker. The Ogg FLAC mapping starts with "OggS"
// and is deliberately not claimed: go-flac rejects it, so pretending to support
// it would turn a clear "unsupported" into a confusing decode error.
func (f *FlacFactory) Match(magic []byte) bool {
	return bytes.HasPrefix(magic, []byte("fLaC"))
}

func (f *FlacFactory) Open(path string) (Decoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	dec, err := pcm.NewDecoder(file)
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("decode: open FLAC %s: %w", path, err)
	}

	d, err := newFlacDecoder(file, dec)
	if err != nil {
		file.Close()

		return nil, err
	}

	return d, nil
}

// Probe reads the STREAMINFO and reports the stream shape. It never decodes
// audio, so even DurationScan stays cheap: FLAC's total is a header field.
func (f *FlacFactory) Probe(path string, _ ProbeOptions) (core.StreamInfo, error) {
	info := core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: -1}

	file, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer file.Close()

	dec, err := pcm.NewDecoder(file)
	if err != nil {
		return info, err
	}

	sm := dec.Info()
	// Channels and bit depth do not change the frame count, so only the rate
	// needs converting into the canonical 48 kHz domain.
	if frames, ok := canonicalFrames(sm.TotalSamples, sm.SampleRate); ok {
		info.TotalFrames = frames
	}

	return info, nil
}

// canonicalFrames converts a source sample count at rate into 48000 Hz frames.
// ok is false when the result is not exact or the rate is unusable, in which
// case the caller reports an unknown total rather than a rounded one.
func canonicalFrames(total uint64, rate int) (int64, bool) {
	if rate <= 0 || total == 0 {
		return 0, false
	}
	if rate == core.CanonicalFormat.Rate {
		return int64(total), true
	}
	scaled := total * uint64(core.CanonicalFormat.Rate)
	if scaled%uint64(rate) != 0 {
		return 0, false
	}

	return int64(scaled / uint64(rate)), true
}

// flacDecoder adapts go-flac's byte-oriented PCM reader to the engine's
// ReadFrames contract, normalizing rate, channels and sample width on the way.
type flacDecoder struct {
	file *os.File
	dec  *pcm.Decoder

	srcRate     int
	srcChannels int
	bitDepth    int
	bytesPS     int

	// ratio is the normalization factor from the source rate to 48000. It is 1
	// for the common case, which lets the sample loop skip resampling entirely.
	ratio float64

	// total is the playable frame count in the canonical domain, or -1.
	total int64

	// raw receives bytes from go-flac, cut to whole source frames.
	raw []byte
	// src queues decoded interleaved stereo frames for the resampler. Only the
	// fraction not yet consumed is kept, so it stays a few frames deep.
	src []float32
	// out holds one block of interleaved stereo float32, which pending points
	// into until the caller has taken it.
	out     []float32
	pending []float32

	// resample state. frac is the fractional frame position into src.
	frac float64
	eof  bool

	// sourceFrame is one interleaved source frame, used to keep reads whole.
	sourceFrame int

	pos    int64
	closed bool
}

func newFlacDecoder(file *os.File, dec *pcm.Decoder) (*flacDecoder, error) {
	sm := dec.Info()
	if sm.Channels < 1 || sm.BitDepth < 1 || sm.SampleRate < 1 {
		return nil, fmt.Errorf("decode: FLAC reports an unusable stream %d Hz %d ch %d-bit",
			sm.SampleRate, sm.Channels, sm.BitDepth)
	}

	bytesPS := (sm.BitDepth + 7) / 8
	frameBytes := sm.Channels * bytesPS
	if frameBytes < 1 {
		return nil, fmt.Errorf("decode: FLAC reports %d channels at %d bits", sm.Channels, sm.BitDepth)
	}

	total, _ := canonicalFrames(sm.TotalSamples, sm.SampleRate)

	d := &flacDecoder{
		file:        file,
		dec:         dec,
		srcRate:     sm.SampleRate,
		srcChannels: sm.Channels,
		bitDepth:    sm.BitDepth,
		bytesPS:     bytesPS,
		ratio:       float64(sm.SampleRate) / float64(core.CanonicalFormat.Rate),
		total:       total,
	}
	if d.ratio == 1 {
		d.ratio = 1 // keep the exact form; the branch below compares to 1
	}
	d.sourceFrame = frameBytes
	// A read shorter than one whole source frame would split a sample.
	d.raw = make([]byte, max(frameBytes, flacRawReadBytes/frameBytes*frameBytes))
	// out is one block of canonical stereo; pending aliases it, so it must not
	// be resized while a caller still holds frames from it.
	d.out = make([]float32, flacOutFrames*2)

	return d, nil
}

func (d *flacDecoder) Info() core.StreamInfo {
	return core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: d.total}
}

// DecoderName names the codec implementation behind this decoder.
func (d *flacDecoder) DecoderName() string { return "go-flac" }

// ParserName names the container handling. FLAC's frames and its container are
// the same bitstream, so both names point at the same library.
func (d *flacDecoder) ParserName() string { return "go-flac (native flac)" }

func (d *flacDecoder) ReadFrames(dst []float32) (int, error) {
	if d.closed {
		return 0, ErrClosed
	}
	if len(dst) < 2 {
		return 0, nil
	}

	frames := 0
	for frames*2 < len(dst) {
		if len(d.pending) == 0 {
			if err := d.fill(); err != nil {
				if errors.Is(err, io.EOF) {
					if frames == 0 {
						return 0, io.EOF
					}

					return frames, nil
				}

				return frames, err
			}
		}

		// Never deliver past the declared end. The resampler's tail can finish
		// one frame long when the interpolation interval lands on the padded
		// sample, and Info already promised the exact total.
		if d.total >= 0 {
			if left := d.total - d.pos; left <= 0 {
				if frames == 0 {
					return 0, io.EOF
				}

				return frames, nil
			} else if want := int(left) * 2; want < len(d.pending) {
				d.pending = d.pending[:want]
			}
		}

		want := len(dst)/2 - frames
		n := min(len(d.pending)/2, want)
		copy(dst[frames*2:(frames+n)*2], d.pending[:n*2])
		d.pending = d.pending[n*2:]
		frames += n
		d.pos += int64(n)
	}

	return frames, nil
}

// fill decodes and normalizes until one block of stereo output is ready. It
// returns io.EOF only when the source is exhausted and nothing is buffered.
func (d *flacDecoder) fill() error {
	for {
		if n := d.resampleInto(d.out); n > 0 {
			d.pending = d.out[:n*2]

			return nil
		}
		if d.eof {
			return io.EOF
		}

		n, err := d.dec.Read(d.raw)
		if n > 0 {
			d.ingest(d.raw[:n])
		}
		if err != nil {
			if n == 0 && !errors.Is(err, io.EOF) {
				return err
			}
			// At end of input the interpolator still needs one frame past the
			// last real one to finish the final output interval. Repeating the
			// last frame holds the endpoint instead of dropping it.
			d.eof = true
			if d.ratio != 1 && len(d.src) >= 2 {
				d.src = append(d.src, d.src[len(d.src)-2], d.src[len(d.src)-1])
			}
		}
	}
}

// ingest decodes interleaved source samples into interleaved stereo float32 in
// [-1, 1]. Mono is duplicated to both channels and anything above stereo keeps
// the first two, which is the width the rest of the engine carries.
func (d *flacDecoder) ingest(raw []byte) {
	frameBytes := d.srcChannels * d.bytesPS
	frames := len(raw) / frameBytes
	if frames == 0 {
		return
	}

	scale := 1 / float32(uint64(1)<<(d.bitDepth-1))
	ch := d.srcChannels

	// The 16-bit case is what nearly every FLAC file uses, so it gets a loop the
	// compiler can keep in registers; every other width takes the general path.
	if d.bytesPS == 2 {
		for i := range frames {
			base := i * ch * 2
			left := float32(int16(binary.LittleEndian.Uint16(raw[base:]))) * scale
			right := left
			if ch > 1 {
				right = float32(int16(binary.LittleEndian.Uint16(raw[base+2:]))) * scale
			}
			d.src = append(d.src, left, right)
		}

		return
	}

	for i := range frames {
		base := i * frameBytes
		left := float32(signExtend(raw[base:base+d.bytesPS])) * scale
		right := left
		if ch > 1 {
			off := base + d.bytesPS
			right = float32(signExtend(raw[off:off+d.bytesPS])) * scale
		}
		d.src = append(d.src, left, right)
	}
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

// resampleInto writes up to len(out)/2 stereo frames into out and returns the
// frame count. The source rate is fixed at construction, so the only per-call
// work is the fractional advance.
func (d *flacDecoder) resampleInto(out []float32) int {
	frames := len(out) / 2
	n := 0

	if d.ratio == 1 {
		for n < frames {
			i := int(d.frac)
			if i*2+1 >= len(d.src) {
				break
			}
			out[n*2] = d.src[i*2]
			out[n*2+1] = d.src[i*2+1]
			n++
			d.frac++
		}
	} else {
		for n < frames {
			i := int(d.frac)
			// Linear interpolation needs the following frame. Without it the
			// interval is left for the next read, which is why a drained block
			// can stop one output short of len(out)/2.
			if (i+1)*2+1 >= len(d.src) {
				break
			}
			f := float32(d.frac - float64(i))
			left := d.src[i*2] + f*(d.src[i*2+2]-d.src[i*2])
			right := d.src[i*2+1] + f*(d.src[i*2+3]-d.src[i*2+1])
			out[n*2] = left
			out[n*2+1] = right
			n++
			d.frac += d.ratio
		}
	}

	d.dropConsumed()

	return n
}

// dropConsumed releases the frames the resampler has passed.
func (d *flacDecoder) dropConsumed() {
	consumed := int(d.frac)
	if consumed > len(d.src)/2 {
		consumed = len(d.src) / 2
	}
	if consumed > 0 {
		d.src = d.src[consumed*2:]
		d.frac -= float64(consumed)
	}
}

// SeekFrame repositions so the next delivered frame is frame. go-flac's
// SeekToSample binary-searches frames and lands with a short decode; it reports
// the sample it actually landed on, which is frame unless the target precedes
// the first frame.
func (d *flacDecoder) SeekFrame(frame int64) error {
	if d.closed {
		return ErrClosed
	}
	if frame < 0 {
		return fmt.Errorf("decode: negative seek target %d", frame)
	}

	// The source position is in source-rate samples; the canonical frame is not.
	src := frame
	if d.ratio != 1 {
		src = int64(float64(frame) * d.ratio)
	}

	landed, err := d.dec.SeekToSample(src)
	if err != nil {
		return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
	}

	// Anything the resampler still held belongs to the abandoned position.
	d.src = d.src[:0]
	d.pending = nil
	d.frac = 0
	d.eof = false
	d.pos = landed
	if d.ratio != 1 {
		d.pos = int64(float64(landed) / d.ratio)
	}

	return nil
}

func (d *flacDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.pending = nil
	d.src = nil
	d.dec = nil
	if d.file != nil {
		d.file.Close()
		d.file = nil
	}

	return nil
}
