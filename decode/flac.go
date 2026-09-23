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
// same contract the Opus decoders satisfy. The shared pcmConverter does the
// rate, channel and width conversion for every byte-oriented decoder here.
//
// Dependencies: github.com/tphakala/go-flac.

package decode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/dlcuy22/player/core"
	"github.com/tphakala/go-flac/pcm"
)

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

// flacDecoder adapts go-flac's byte-oriented PCM reader to the engine's
// ReadFrames contract, normalizing rate, channels and sample width through the
// shared pcmConverter.
type flacDecoder struct {
	file *os.File
	dec  *pcm.Decoder

	conv *pcmConverter

	// total is the playable frame count in the canonical domain, or -1.
	total int64

	// raw receives bytes from go-flac, cut to whole source frames.
	raw []byte
	// out holds one block of interleaved stereo float32, which pending points
	// into until the caller has taken it.
	out     []float32
	pending []float32

	// eof records that the source signalled end, so one final drain can emit
	// the interpolation tail.
	eof bool

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
	// A 44.1 kHz source almost never lands on an exact 48 kHz frame boundary,
	// so canonicalFrames reports ok=false for most real files. Discarding that
	// bool would default total to 0, and readCanonical clamps delivery to it,
	// turning every such file into an immediate end of stream. An inexact
	// length is reported as unknown (-1), the same contract the MP3 and WAV
	// decoders follow, so the resampler delivers the tail instead.
	total := int64(-1)
	if frames, ok := canonicalFrames(sm.TotalSamples, sm.SampleRate); ok {
		total = frames
	}

	d := &flacDecoder{
		file:  file,
		dec:   dec,
		conv:  newPCMConverter(sm.SampleRate, sm.Channels, bytesPS),
		total: total,
	}
	// A read shorter than one whole source frame would split a sample.
	d.raw = make([]byte, max(frameBytes, pcmRawReadBytes/frameBytes*frameBytes))
	// out is one block of canonical stereo; pending aliases it, so it must not
	// be resized while a caller still holds frames from it.
	d.out = make([]float32, pcmOutFrames*2)

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
	return readCanonical(dst, d.out, &d.pending, d.fill, &d.pos, d.total, d.closed)
}

// fill decodes and normalizes until one block of stereo output is ready. It
// returns io.EOF only when the source is exhausted and nothing is buffered.
func (d *flacDecoder) fill() error {
	for {
		if n := d.conv.resampleInto(d.out); n > 0 {
			d.pending = d.out[:n*2]

			return nil
		}
		if d.eof {
			return io.EOF
		}

		n, err := d.dec.Read(d.raw)
		if n > 0 {
			d.conv.ingest(d.raw[:n])
		}
		if err != nil {
			if n == 0 && !errors.Is(err, io.EOF) {
				return err
			}
			// The interpolator needs one frame past the last real one to finish
			// the final output interval.
			d.eof = true
			d.conv.markEOF()
		}
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
	src := canonicalSamples(frame, d.conv.srcRate)

	landed, err := d.dec.SeekToSample(src)
	if err != nil {
		return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
	}

	// Anything the resampler still held belongs to the abandoned position.
	d.conv.reset()
	d.pending = nil
	d.eof = false
	d.pos = canonicalFramesOf(landed, d.conv)

	return nil
}

func (d *flacDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.pending = nil
	d.conv.src = nil
	d.dec = nil
	if d.file != nil {
		d.file.Close()
		d.file = nil
	}

	return nil
}
