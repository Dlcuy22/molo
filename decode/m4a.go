// M4A/MP4 decoding through github.com/tphakala/go-m4a's aacm4a bridge, which
// couples that container reader to github.com/tphakala/go-aac. This is the AAC
// path only: an MP4 holding Opus or FLAC is rejected by the codec constructor,
// which is the honest outcome for a file this factory cannot decode.
//
// The bridge's decoder is not edit-list aware. It emits every decoded sample,
// including the leading encoder priming and the trailing final-frame padding
// that the container's edit list trims. The adapter skips the priming here and
// lets the shared delivery loop clamp to the edit-list presentation length, so
// what leaves ReadFrames is the playable signal the edit list describes.
//
// Dependencies: github.com/tphakala/go-m4a, github.com/tphakala/go-aac.
package decode

import (
	"fmt"
	"os"
	"time"

	"github.com/dlcuy22/player/core"
	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/aacm4a"
)

// init registers the AAC-in-MP4 decoder.
func init() {
	Register(NewM4aFactory())
}

// M4aFactory decodes AAC-LC audio from an MP4/M4A container.
type M4aFactory struct{}

// NewM4aFactory returns a factory for the AAC-in-MP4 decoder.
func NewM4aFactory() *M4aFactory { return &M4aFactory{} }

func (f *M4aFactory) Name() string { return "m4a" }

// FriendlyName is the label a UI shows for this codec.
func (f *M4aFactory) FriendlyName() string { return "M4a" }

// Weight breaks ties against a future M4A codec registered for the same
// extension, the same role the FLAC factory's 90 plays.
func (f *M4aFactory) Weight() int { return 90 }

// Exts claims both container spellings. An .mp4 audio file has the same
// ISO-BMFF structure and the same ftyp brand box as an .m4a, so the same
// decoder handles it.
func (f *M4aFactory) Exts() []string { return []string{".m4a", ".mp4"} }

// Match claims an ISO base media file. Such a file begins with a 4-byte
// big-endian box size followed by the box type, so the "ftyp" brand box type
// starts at offset 4, not 0: bytes 0..3 are the size field. Checking offset 0
// would reject every real MP4, and a file whose first bytes happened to spell
// "ftyp" would not be one.
func (f *M4aFactory) Match(magic []byte) bool {
	return len(magic) >= 8 && string(magic[4:8]) == "ftyp"
}

func (f *M4aFactory) Open(path string) (Decoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	d, err := newM4aDecoder(file)
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("decode: open M4A %s: %w", path, err)
	}

	return d, nil
}

// Probe reads the container metadata and reports the stream shape. It never
// decodes audio, so even DurationScan stays cheap: the total comes from the
// movie header and the edit list.
func (f *M4aFactory) Probe(path string, _ ProbeOptions) (core.StreamInfo, error) {
	info := core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: -1}

	file, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer file.Close()

	rd, err := m4a.NewReader(file)
	if err != nil {
		return info, err
	}

	md := rd.Info()
	if md.Codec != m4a.CodecAACLC {
		return info, fmt.Errorf("decode: probe M4A %s: track codec is %s, not AAC-LC", path, md.Codec)
	}
	info.TotalFrames = m4aTotalFrames(md)

	return info, nil
}

// m4aDecoder adapts aacm4a's byte-oriented S16 reader to the engine's
// ReadFrames contract, normalizing rate, channels and sample width through the
// shared pcmConverter.
type m4aDecoder struct {
	file *os.File
	dec  *aacpcm.Decoder

	conv *pcmConverter

	// total is the playable frame count in the canonical domain, or -1.
	total int64

	// skipBytes is the encoder priming, in source bytes, that the raw decoder
	// emits but the edit list excludes. It is consumed from the front. Keeping
	// it in the source domain makes the trim exact at any source rate, where a
	// priming count of 1024 need not be a whole number of canonical frames.
	skipBytes int64

	// raw receives bytes from go-aac, cut to whole source frames.
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

func newM4aDecoder(file *os.File) (*m4aDecoder, error) {
	dec, info, err := aacm4a.NewDecoder(file)
	if err != nil {
		// aacm4a returns the codec's own error unwrapped, so an HE-AAC stream
		// stays errors.Is-visible as aacpcm.ErrUnsupportedSBR; the caller's
		// wrapping preserves that match.
		return nil, err
	}
	// The decoder accepted the config, but Info still drives buffer sizing, so
	// a malformed sample entry must not reach the frame-size division.
	if info.SampleRate < 1 || info.Channels < 1 {
		return nil, fmt.Errorf("decode: M4A reports an unusable stream %d Hz %d ch",
			info.SampleRate, info.Channels)
	}

	return newM4aDecoderFrom(file, dec, info), nil
}

// newM4aDecoderFrom builds the adapter from an already-open codec decoder and
// its container info.
func newM4aDecoderFrom(file *os.File, dec *aacpcm.Decoder, info m4a.Info) *m4aDecoder {
	// The bridge always yields interleaved little-endian S16.
	const bytesPS = 2

	d := &m4aDecoder{
		file:  file,
		dec:   dec,
		conv:  newPCMConverter(info.SampleRate, info.Channels, bytesPS),
		total: m4aTotalFrames(info),
	}
	d.skipBytes = m4aPrimingBytes(info)
	d.raw = rawFrames()
	// out is one block of canonical stereo; pending aliases it, so it must not
	// be resized while a caller still holds frames from it.
	d.out = outBlock()

	return d
}

func (d *m4aDecoder) Info() core.StreamInfo {
	return core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: d.total}
}

// DecoderName names the codec implementation behind this decoder.
func (d *m4aDecoder) DecoderName() string { return "go-aac" }

// ParserName names the container handling. The codec is go-aac; the container
// is go-m4a, so the two labels split accordingly.
func (d *m4aDecoder) ParserName() string { return "go-m4a (mp4/aac-lc)" }

func (d *m4aDecoder) ReadFrames(dst []float32) (int, error) {
	return readCanonical(dst, d.out, &d.pending, d.fill, &d.pos, d.total, d.closed)
}

// fill decodes and normalizes until one block of stereo output is ready.
func (d *m4aDecoder) fill() error {
	return pcmFillLoop(d.conv, d.out, &d.pending, &d.eof, d.dec.Read, d.raw, d.feed)
}

// feed trims the leading encoder priming from one chunk and converts the rest.
// The raw go-aac decoder emits the priming samples the edit list trims, so they
// are discarded before conversion rather than delivered as audio. The trim is
// counted in bytes so it stays exact at any source rate.
func (d *m4aDecoder) feed(raw []byte) {
	rest, dropped := d.conv.skipFrameBytes(raw, d.skipBytes)
	d.skipBytes -= dropped
	if dropped > 0 && len(rest) == 0 {
		return
	}
	d.conv.feed(rest)
}

func (d *m4aDecoder) Close() error {
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

// m4aTotalFrames is the truthful playable length. go-m4a reports the edit-list
// presentation duration, which excludes both the encoder priming and the
// trailing final-frame padding; FrameCount*1024 counts both and is longer by
// the priming plus padding. This reports the edit list's duration, which is
// what the streamer will actually deliver. A duration that is not a whole
// number of canonical frames is reported as unknown rather than rounded.
func m4aTotalFrames(info m4a.Info) int64 {
	if info.SampleRate <= 0 || info.Duration <= 0 {
		return -1
	}

	samples, ok := durationSamples(info.Duration, info.SampleRate)
	if !ok {
		return -1
	}

	frames, ok := canonicalFrames(uint64(samples), info.SampleRate)
	if !ok {
		return -1
	}

	return frames
}

// m4aPrimingBytes is the encoder delay in source bytes. The decoder emits it
// before the audio the edit list keeps. A declared delay past the int64 range
// is reported as zero rather than wrapped into a negative skip.
func m4aPrimingBytes(info m4a.Info) int64 {
	if info.EncoderDelay <= 0 || info.Channels <= 0 {
		return 0
	}

	stride := int64(info.Channels) * 2
	if stride <= 0 || info.EncoderDelay > (1<<62)/stride {
		return 0
	}

	return info.EncoderDelay * stride
}

// durationSamples converts a presentation duration to source-rate samples. ok
// is false when the conversion is not exact, so a caller reports an unknown
// total rather than a rounded one. The nanosecond value is split into whole
// seconds and a remainder so neither product can overflow int64.
func durationSamples(d time.Duration, rate int) (int64, bool) {
	if d <= 0 || rate <= 0 {
		return 0, false
	}

	ns := int64(d)
	sec := ns / int64(time.Second)
	rem := ns % int64(time.Second)
	remSamples := rem * int64(rate)
	if remSamples%int64(time.Second) != 0 {
		return 0, false
	}

	return sec*int64(rate) + remSamples/int64(time.Second), true
}
