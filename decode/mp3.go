// MP3 decoding through github.com/tphakala/go-mp3/pcm. This is the pure-Go
// path: it needs no runtime shared library and no cgo, and it decodes the
// native MPEG audio bitstream rather than an Ogg mapping.
//
// MP3 has no container index: a seek walks frame headers from the first audio
// frame and then primes the bit reservoir and MDCT overlap before landing, which
// is what the library's SeekToSample does. The output is normalized to the
// engine's canonical 48000 Hz stereo float32 by the shared pcmConverter, the
// same contract the FLAC and Opus decoders satisfy.
//
// The decoder asks the library for WithF32, its native sample type. pcmConverter
// ingests integer PCM only, so the float bytes are unpacked to []float32 here
// and handed straight to the converter's resampler; only the byte decode is
// local, rate and channel conversion still go through the shared layer. Taking
// the default S16 would round every sample through int16 and back for no gain,
// and the extra quantization is not worth it even on lossy output.
//
// Dependencies: github.com/tphakala/go-mp3.

package decode

import (
	"fmt"
	"os"

	"github.com/dlcuy22/molo/core"
	mp3pcm "github.com/tphakala/go-mp3/pcm"
)

// init registers the MP3 decoder.
func init() {
	Register(NewMp3Factory())
}

// Mp3Factory decodes MPEG audio without cgo.
type Mp3Factory struct{}

// NewMp3Factory returns a factory for the MPEG audio decoder.
func NewMp3Factory() *Mp3Factory { return &Mp3Factory{} }

func (f *Mp3Factory) Name() string { return "mp3" }

// FriendlyName is the label a UI shows for this codec.
func (f *Mp3Factory) FriendlyName() string { return "Mp3" }

// Weight only breaks ties between codecs that claim the same extension. No other
// factory claims .mp3, so this value never competes.
func (f *Mp3Factory) Weight() int { return 90 }

func (f *Mp3Factory) Exts() []string { return []string{".mp3"} }

// Match claims an MPEG-1/2/2.5 Layer III frame sync, and also a leading ID3v2
// tag. A tagged file starts with "ID3" and reaches its first frame sync only
// after the tag body, so a magic-only check that demanded a sync would reject
// every tagged file: the extension would carry it, but a file with no .mp3
// suffix would fall through sniffing and be unrecognized. Claiming "ID3" lets
// the decoder itself skip the tag and find the audio.
func (f *Mp3Factory) Match(magic []byte) bool {
	if hasID3v2(magic) {
		return true
	}
	if len(magic) < 2 || magic[0] != 0xFF || magic[1]&0xE0 != 0xE0 {
		return false
	}
	version := (magic[1] >> 3) & 0x03 // 0=MPEG2.5, 1=reserved, 2=MPEG2, 3=MPEG1
	layer := (magic[1] >> 1) & 0x03   // 1=Layer III

	return layer == 0x01 && version != 0x01
}

// hasID3v2 reports a leading ID3v2 tag header. Only the three magic bytes are
// needed: this is a dispatch hint, not a tag parser, and the decoder rejects a
// malformed tag on its own.
func hasID3v2(magic []byte) bool {
	return len(magic) >= 3 && magic[0] == 'I' && magic[1] == 'D' && magic[2] == '3'
}

func (f *Mp3Factory) Open(path string) (Decoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	dec, err := mp3pcm.NewDecoder(file, mp3pcm.WithF32())
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("decode: open MP3 %s: %w", path, err)
	}

	d, err := newMp3Decoder(file, dec)
	if err != nil {
		file.Close()

		return nil, err
	}

	return d, nil
}

// Probe reads the first frame and its Xing/Info or VBRI tag without decoding
// audio, so DurationScan stays cheap. The total is exact when a tag carries a
// frame count (the fixture's Info tag does) or when a constant-bitrate stream's
// byte length divides by its first frame size; go-mp3 fills TotalSamples with
// whichever applies and leaves it zero when neither can be determined, in which
// case this reports -1 rather than a guess.
func (f *Mp3Factory) Probe(path string, _ ProbeOptions) (core.StreamInfo, error) {
	info := core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: -1}

	file, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer file.Close()

	dec, err := mp3pcm.NewDecoder(file)
	if err != nil {
		return info, err
	}

	sm := dec.Info()
	// MP3's TotalSamples is per channel in both the 1- and 2-channel case, so
	// only the rate needs converting into the canonical 48 kHz domain.
	if frames, ok := canonicalFrames(sm.TotalSamples, sm.SampleRate); ok {
		info.TotalFrames = frames
	}
	// The tag states the exact source-domain total, which survives the 48 kHz
	// conversion even when it is not exact and TotalFrames stays -1.
	info.SourceSamples = int64(sm.TotalSamples)
	info.SourceRate = sm.SampleRate

	return info, nil
}

// mp3Decoder adapts go-mp3's byte-oriented PCM reader to the engine's
// ReadFrames contract, normalizing rate and channels through the shared
// pcmConverter.
type mp3Decoder struct {
	file *os.File
	dec  *mp3pcm.Decoder

	conv *pcmConverter

	// total is the playable frame count in the canonical domain, or -1.
	total int64

	// srcSamples and srcRate are the exact source-domain length, carried so
	// Info can still report a duration when total is -1.
	srcSamples int64
	srcRate    int

	// raw receives bytes from go-mp3, cut to whole source frames.
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

func newMp3Decoder(file *os.File, dec *mp3pcm.Decoder) (*mp3Decoder, error) {
	sm := dec.Info()
	if sm.Channels < 1 || sm.SampleRate < 1 {
		return nil, fmt.Errorf("decode: MP3 reports an unusable stream %d Hz %d ch",
			sm.SampleRate, sm.Channels)
	}

	// go-mp3 emits its native interleaved float32 with WithF32.
	const bytesPS = 4
	total := int64(-1)
	if frames, ok := canonicalFrames(sm.TotalSamples, sm.SampleRate); ok {
		total = frames
	}

	d := &mp3Decoder{
		file:       file,
		dec:        dec,
		conv:       newPCMConverter(sm.SampleRate, sm.Channels, bytesPS),
		total:      total,
		srcSamples: int64(sm.TotalSamples),
		srcRate:    sm.SampleRate,
	}
	d.raw = rawFrames()
	// out is one block of canonical stereo; pending aliases it, so it must not
	// be resized while a caller still holds frames from it.
	d.out = outBlock()

	return d, nil
}

func (d *mp3Decoder) Info() core.StreamInfo {
	return core.StreamInfo{
		Format:        core.CanonicalFormat,
		TotalFrames:   d.total,
		SourceSamples: d.srcSamples,
		SourceRate:    d.srcRate,
	}
}

// DecoderName names the codec implementation behind this decoder.
func (d *mp3Decoder) DecoderName() string { return "go-mp3" }

// ParserName names the container handling. MP3's frames and its container are
// the same bitstream, so both names point at the same library.
func (d *mp3Decoder) ParserName() string { return "go-mp3 (mpeg audio)" }

func (d *mp3Decoder) ReadFrames(dst []float32) (int, error) {
	return readCanonical(dst, d.out, &d.pending, d.fill, &d.pos, d.total, d.closed)
}

// fill decodes and normalizes until one block of stereo output is ready.
func (d *mp3Decoder) fill() error {
	return pcmFillLoop(d.conv, d.out, &d.pending, &d.eof, d.dec.Read, d.raw, d.conv.feedFloat)
}

// SeekFrame repositions so the next delivered frame is frame. go-mp3's
// SeekToSample walks frame headers to find the landing frame, primes the bit
// reservoir and MDCT overlap, and drops the intra-frame samples, so at 48 kHz
// it lands on the exact source sample. When the source rate is not the
// canonical one the canonical frame maps to a fractional source sample and
// canonicalSamples floors, so the seek lands at or just before the target,
// never past it, the same rounding the FLAC path uses.
func (d *mp3Decoder) SeekFrame(frame int64) error {
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

func (d *mp3Decoder) Close() error {
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
