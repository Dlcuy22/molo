// WAV decoding through github.com/tphakala/go-wav. This is the uncompressed
// path: every sample in the file is already the sample the engine needs, so the
// decoder is mostly a width and rate adapter around the shared pcmConverter.
//
// Unlike FLAC, a WAV stream carries no compression and no frame index, so the
// data chunk's declared size is the whole seek story: go-wav turns a frame index
// into a byte offset from the start of the data chunk and clamps to the declared
// end. There is nothing to build.
//
// The decoder normalizes whatever the file holds (any rate, 1..8 channels, 8/16/
// 24/32-bit integer or 32/64-bit float) to the engine's canonical 48000 Hz stereo
// float32, the same contract every other decoder here satisfies. The shared
// pcmConverter does the rate, channel and width conversion.
//
// The one WAV-specific decision is how to reach the shared converter, which
// reads little-endian integer samples and would misread float bits. A float
// source is therefore opened with pcm.WithConvertTo(16), which is exactly the
// library's float-to-integer path (full-scale scaling with clamping), and the
// converter reads the resulting 16-bit integers. An integer source is read at
// its native stored width instead, so the converter scales it directly: asking
// go-wav to convert first would round a 24-bit file down to 16 bits and discard
// the extra precision the converter is about to preserve.
//
// Dependencies: github.com/tphakala/go-wav.

package decode

import (
	"fmt"
	"io"
	"os"

	"github.com/dlcuy22/player/core"
	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/pcm"
)

// init registers the uncompressed decoder.
func init() {
	Register(NewWavFactory())
}

// WavFactory decodes RIFF, RF64 and BW64 WAVE streams without cgo.
type WavFactory struct{}

// NewWavFactory returns a factory for the uncompressed WAV decoder.
func NewWavFactory() *WavFactory { return &WavFactory{} }

func (f *WavFactory) Name() string { return "wav" }

// FriendlyName is the label a UI shows for this codec. WAV is the uncompressed
// choice, which is the one property that distinguishes it from every compressed
// variant.
func (f *WavFactory) FriendlyName() string { return "Wav" }

// Weight only breaks ties between codecs that claim the same extension. No other
// factory claims .wav, so this value never competes.
func (f *WavFactory) Weight() int { return 90 }

func (f *WavFactory) Exts() []string { return []string{".wav"} }

// Match claims the three WAVE container magics. The first four bytes name the
// RIFF flavour but say nothing about the form inside it: "RIFF" also introduces
// an AVI, a WebP or a plain RIFF palette, and handing one of those to go-wav
// would turn a clear "unsupported" into a confusing decode error. The form type
// at offset 8 is what makes it WAVE, so it is required too. The length gate is
// offset 12, the same window wav.Sniff uses; a shorter slice cannot carry the
// form type and is not claimed.
func (f *WavFactory) Match(magic []byte) bool {
	if len(magic) < 12 || string(magic[8:12]) != "WAVE" {
		return false
	}

	switch string(magic[0:4]) {
	case "RIFF", "RF64", "BW64":
		return true
	default:
		return false
	}
}

func (f *WavFactory) Open(path string) (Decoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	dec, err := pcm.NewDecoder(file)
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("decode: open WAV %s: %w", path, err)
	}

	d, err := newWavDecoder(file, dec)
	if err != nil {
		file.Close()

		return nil, err
	}

	return d, nil
}

// Probe reads the RIFF header and reports the stream shape. It never decodes
// audio: go-wav derives the frame count from the data chunk size, so even
// DurationScan stays cheap.
func (f *WavFactory) Probe(path string, _ ProbeOptions) (core.StreamInfo, error) {
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
	// needs converting into the canonical 48 kHz domain. A missing or streamed
	// count (TotalFrames == 0) stays -1 rather than claiming an empty stream.
	if frames, ok := canonicalFrames(sm.TotalFrames, sm.SampleRate); ok {
		info.TotalFrames = frames
	}

	return info, nil
}

// wavDecoder adapts go-wav's byte-oriented PCM reader to the engine's
// ReadFrames contract, normalizing rate, channels and sample width through the
// shared pcmConverter.
type wavDecoder struct {
	file *os.File
	dec  *pcm.Decoder

	conv *pcmConverter

	// total is the playable frame count in the canonical domain, or -1.
	total int64

	// raw receives bytes from go-wav. The converter carries any partial frame,
	// so no manual remainder bookkeeping is needed here.
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

func newWavDecoder(file *os.File, dec *pcm.Decoder) (*wavDecoder, error) {
	sm := dec.Info()

	// The shared converter reads signed little-endian integers, so a source
	// whose stored samples are not that has to be normalized by go-wav first.
	// Reopening in place is cheap and keeps one decoder: the header is
	// re-parsed but no audio is touched.
	if needsConvertToS16(sm) {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("decode: rewind WAV for integer conversion: %w", err)
		}
		if err := dec.Reset(file, pcm.WithConvertTo(16)); err != nil {
			return nil, fmt.Errorf("decode: convert WAV to 16-bit integers: %w", err)
		}
		sm = dec.Info()
	}

	if sm.Channels < 1 || sm.BitDepth < 1 || sm.SampleRate < 1 {
		return nil, fmt.Errorf("decode: WAV reports an unusable stream %d Hz %d ch %d-bit",
			sm.SampleRate, sm.Channels, sm.BitDepth)
	}

	// BitDepth is the width Read yields, so it is also the width the converter
	// must decode: native for a 16/24/32-bit integer source, 16 after the
	// conversion above. bytesPS must track what is read, not what was stored,
	// or a converted stream would be split at the wrong stride.
	bytesPS := (sm.BitDepth + 7) / 8

	total := int64(-1)
	if frames, ok := canonicalFrames(sm.TotalFrames, sm.SampleRate); ok {
		total = frames
	}

	d := &wavDecoder{
		file:  file,
		dec:   dec,
		conv:  newPCMConverter(sm.SampleRate, sm.Channels, bytesPS),
		total: total,
	}
	d.raw = rawFrames()
	// out is one block of canonical stereo; pending aliases it, so it must not
	// be resized while a caller still holds frames from it.
	d.out = outBlock()

	return d, nil
}

// needsConvertToS16 reports whether the stored encoding has to be turned into
// 16-bit signed integers before the shared converter can read it.
//
// This is the one WAV-specific decision in the file, and it has two arms rather
// than the float one a reader might expect. A float source's bits are not
// integers at all, so go-wav's float path (full-scale scaling with clamping) is
// the only way to reach the converter. An 8-bit integer source is the second
// arm because WAV stores it UNSIGNED with a midpoint of 128 while the converter
// reads two's complement: read natively, every 8-bit file would be inverted and
// offset. Widening it to 16 bits is exact (a shift by 8) and is what go-wav's
// converter does with the sign corrected, so nothing is lost. Every wider
// integer depth is stored signed, so it is read natively and the converter
// scales it from its own width, which is what keeps a 24-bit file's low bits.
func needsConvertToS16(sm wav.StreamInfo) bool {
	if sm.SourceFormat == wav.SampleFormatFloat {
		return true
	}

	return sm.SourceFormat == wav.SampleFormatPCM && sm.SourceBitDepth == 8
}

func (d *wavDecoder) Info() core.StreamInfo {
	return core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: d.total}
}

// DecoderName names the codec implementation behind this decoder.
func (d *wavDecoder) DecoderName() string { return "go-wav" }

// ParserName names the container handling: RIFF is the container and the sample
// stream is just its data chunk, so both names point at the same library.
func (d *wavDecoder) ParserName() string { return "go-wav (riff)" }

func (d *wavDecoder) ReadFrames(dst []float32) (int, error) {
	return readCanonical(dst, d.out, &d.pending, d.fill, &d.pos, d.total, d.closed)
}

// fill decodes and normalizes until one block of stereo output is ready.
func (d *wavDecoder) fill() error {
	return pcmFillLoop(d.conv, d.out, &d.pending, &d.eof, d.dec.Read, d.raw, d.conv.feed)
}

// SeekFrame repositions so the next delivered frame is frame. go-wav computes
// the byte offset from the data chunk start and clamps to the declared end, so a
// target past the audio lands on the last frame. It reports the frame it landed
// on, which frame unless the target precedes the first frame.
func (d *wavDecoder) SeekFrame(frame int64) error {
	if d.closed {
		return ErrClosed
	}
	if frame < 0 {
		return fmt.Errorf("decode: negative seek target %d", frame)
	}

	// The source position is in source-rate frames; the canonical frame is not.
	src := canonicalSamples(frame, d.conv.srcRate)

	landed, err := d.dec.SeekToFrame(src)
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

func (d *wavDecoder) Close() error {
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
