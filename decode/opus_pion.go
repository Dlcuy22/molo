// Opus decoding through github.com/pion/opus. This is the pure-Go path: it
// needs no runtime shared library and can therefore also decode from an
// arbitrary io.Reader, at the cost of being forward-only (seek is implemented
// by reopening and discarding).
//
// Dependencies: github.com/pion/opus plus its pkg/oggreader container reader.

package decode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/dlcuy22/player/core"
	"github.com/pion/opus"
	"github.com/pion/opus/pkg/oggreader"
)

// opusTagsTag identifies the mandatory comment header of an Ogg Opus stream.
// The ID header is consumed by oggreader.NewWith, so this is the only header
// packet the decoder has to skip.
var opusTagsTag = []byte("OpusTags")

// maxOpusPacketSamples is Opus's 120 ms ceiling: 48000 * 0.12 frames, two
// channels interleaved. One allocation at open covers every packet.
const maxOpusPacketSamples = 48000 * 12 / 100 * 2

// init registers the pure-Go decoder last so it wins magic resolution and
// becomes the default for Ogg Opus. It has no runtime dependency, unlike the
// libopusfile path, so it is the safer default when both are registered.
func init() {
	Register(NewPionOpusFactory())
}

// PionOpusFactory decodes Ogg Opus without cgo.
type PionOpusFactory struct{}

func NewPionOpusFactory() *PionOpusFactory { return &PionOpusFactory{} }

func (f *PionOpusFactory) Name() string { return "opus-pion" }

func (f *PionOpusFactory) Exts() []string { return []string{".opus", ".ogg"} }

// Match accepts any Ogg stream; Open then rejects non-Opus payloads. Checking
// deeper would mean seeking, which sniffing must not do.
func (f *PionOpusFactory) Match(magic []byte) bool {
	return len(magic) >= 4 && string(magic[:4]) == oggCapture
}

func (f *PionOpusFactory) Open(path string) (Decoder, error) {
	return newPionOpusDecoder(pionPathSource(path))
}

// OpenReader decodes from a stream rather than a path. Seeking still works
// when the reader implements io.Seeker; otherwise the decoder is forward-only.
func (f *PionOpusFactory) OpenReader(r io.Reader) (Decoder, error) {
	return newPionOpusDecoder(pionReaderSource{r})
}

// Probe reads the tail of the file for the final granule position. It never
// decodes audio, so even a probe on a large file stays cheap.
func (f *PionOpusFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	return probeOggOpus(path, opts.Duration, oggTailWindow)
}

// pionSource hands out a reader positioned at the start of the stream, which is
// how seek-by-reopen is implemented. A source that cannot rewind reports false
// and seeking is refused rather than silently corrupting position.
type pionSource interface {
	open() (io.Reader, io.Closer, error)
	rewindable() bool
}

type pionPathSource string

func (p pionPathSource) open() (io.Reader, io.Closer, error) {
	f, err := os.Open(string(p))
	if err != nil {
		return nil, nil, err
	}

	return f, f, nil
}

func (p pionPathSource) rewindable() bool { return true }

type pionReaderSource struct{ r io.Reader }

func (s pionReaderSource) open() (io.Reader, io.Closer, error) {
	if seeker, ok := s.r.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return nil, nil, err
		}
	}

	return s.r, nil, nil
}

func (s pionReaderSource) rewindable() bool {
	_, ok := s.r.(io.Seeker)

	return ok
}

type pionOpusDecoder struct {
	src pionSource

	srcCloser io.Closer
	ogg       *oggreader.OggReader
	dec       opus.Decoder

	// gain is the linear form of the header's Q7.8 dB output gain.
	gain float32

	// skip counts pre-skip frames still owed to the encoder's warm-up, which
	// must not reach the output.
	skip int

	// pending holds gain-applied, pre-skip-trimmed frames not yet delivered.
	pending []float32
	decBuf  []float32

	// headersDone becomes true once the comment header packet (or, for a
	// malformed file, the first audio packet) has been consumed.
	headersDone bool

	pos    int64
	closed bool
}

func newPionOpusDecoder(src pionSource) (*pionOpusDecoder, error) {
	d := &pionOpusDecoder{
		src:    src,
		decBuf: make([]float32, maxOpusPacketSamples),
	}
	if err := d.reset(); err != nil {
		return nil, err
	}

	return d, nil
}

// reset reopens the source and rebuilds the parser and decoder, which is also
// how encoding state is cleared for a seek.
func (d *pionOpusDecoder) reset() error {
	r, closer, err := d.src.open()
	if err != nil {
		return err
	}
	if d.srcCloser != nil {
		d.srcCloser.Close()
	}
	d.srcCloser = closer

	ogg, header, err := oggreader.NewWith(r)
	if err != nil {
		d.detachSource()

		return fmt.Errorf("decode: parse Ogg Opus header: %w", err)
	}
	dec, err := opus.NewDecoderWithOutput(48000, 2)
	if err != nil {
		d.detachSource()

		return fmt.Errorf("decode: init Opus decoder: %w", err)
	}

	d.ogg = ogg
	d.dec = dec
	d.gain = float32(math.Pow(10, float64(int16(header.OutputGain))/(20*256)))
	d.skip = int(header.PreSkip)
	d.pending = nil
	d.headersDone = false
	d.pos = 0

	return nil
}

// detachSource releases the reader of a failed reset so a retry opens a fresh
// one instead of leaving a half-built decoder holding the old handle.
func (d *pionOpusDecoder) detachSource() {
	if d.srcCloser != nil {
		d.srcCloser.Close()
		d.srcCloser = nil
	}
	d.ogg = nil
}

func (d *pionOpusDecoder) Info() core.StreamInfo {
	return core.StreamInfo{
		Format: core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		// A forward-only decoder has no frame total until it scans the file,
		// and scanning at open defeats the point of Probe.
		TotalFrames: -1,
	}
}

func (d *pionOpusDecoder) ReadFrames(dst []float32) (int, error) {
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

		want := len(dst)/2 - frames
		n := min(len(d.pending)/2, want)
		copy(dst[frames*2:(frames+n)*2], d.pending[:n*2])
		d.pending = d.pending[n*2:]
		frames += n
		d.pos += int64(n)
	}

	return frames, nil
}

// fill decodes packets until one produces audible frames, applying the header
// gain and consuming any outstanding pre-skip first.
func (d *pionOpusDecoder) fill() error {
	for {
		packet, _, err := d.ogg.ParseNextPacket()
		if err != nil {
			return err
		}

		if !d.headersDone {
			d.headersDone = true
			// NewWith already consumed OpusHead, so the only header packet left
			// is OpusTags. A stream without it still has to be playable, hence
			// the prefix check rather than an unconditional skip.
			if bytes.HasPrefix(packet, opusTagsTag) {
				continue
			}
		}

		n, err := d.dec.DecodeToFloat32(packet, d.decBuf)
		if err != nil {
			return fmt.Errorf("decode: opus packet: %w", err)
		}

		frames := d.trim(d.scale(d.decBuf[:n*2]))
		if len(frames) == 0 {
			continue
		}
		d.pending = frames

		return nil
	}
}

func (d *pionOpusDecoder) scale(samples []float32) []float32 {
	if d.gain == 1 {
		return samples
	}
	for i := range samples {
		samples[i] *= d.gain
	}

	return samples
}

// trim drops decoded frames that belong to the encoder's pre-skip. A short
// first packet is fully consumed and the remainder is carried to the next.
func (d *pionOpusDecoder) trim(samples []float32) []float32 {
	if d.skip <= 0 {
		return samples
	}

	frames := len(samples) / 2
	if frames <= d.skip {
		d.skip -= frames

		return nil
	}

	samples = samples[d.skip*2:]
	d.skip = 0

	return samples
}

func (d *pionOpusDecoder) SeekFrame(frame int64) error {
	if d.closed {
		return ErrClosed
	}
	if frame < 0 {
		return fmt.Errorf("decode: negative seek target %d", frame)
	}
	if !d.src.rewindable() {
		return errors.New("decode: source is not seekable")
	}
	if err := d.reset(); err != nil {
		return err
	}

	for d.pos < frame {
		if len(d.pending) == 0 {
			if err := d.fill(); err != nil {
				if errors.Is(err, io.EOF) {
					return fmt.Errorf("decode: seek target %d is past end of stream", frame)
				}

				return err
			}
		}

		need := frame - d.pos
		avail := int64(len(d.pending) / 2)
		if avail <= need {
			d.pending = nil
			d.pos += avail

			continue
		}
		d.pending = d.pending[need*2:]
		d.pos += need
	}

	return nil
}

func (d *pionOpusDecoder) Close() error {
	d.closed = true
	d.pending = nil
	d.ogg = nil

	if d.srcCloser != nil {
		d.srcCloser.Close()
		d.srcCloser = nil
	}

	return nil
}
