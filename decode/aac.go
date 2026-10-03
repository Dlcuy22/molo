// AAC-LC decoding through github.com/tphakala/go-aac. This is the lossy path:
// it needs no runtime shared library and no cgo, and it decodes the ADTS frame
// stream (.aac) rather than the raw AAC inside an MP4 container, which is a
// different demuxer's job.
//
// ADTS is a bare frame stream. Each frame carries its own 7-byte header with
// the sample rate and channel configuration, but there is no field for the
// total length and no frame index, so go-aac decodes forward only. There is no
// seek to expose, and the streamer's reopen-and-discard fallback positions this
// decoder. The total is likewise unknown until the stream is drained, which is
// why Probe reports -1 rather than guess.
//
// The decoder normalizes whatever the file holds (44.1 or 48 kHz, mono or
// stereo) to the engine's canonical 48000 Hz stereo float32, the same contract
// the other byte-oriented decoders satisfy. The shared pcmConverter does the
// rate, channel and width conversion; go-aac emits interleaved little-endian
// S16, so the stored width is always two bytes.
//
// Dependencies: github.com/tphakala/go-aac.

package decode

import (
	"fmt"
	"io"
	"os"

	"github.com/dlcuy22/player/core"
	"github.com/tphakala/go-aac/pcm"
)

// init registers the AAC decoder.
func init() {
	Register(NewAacFactory())
}

// AacFactory decodes AAC-LC in ADTS without cgo.
type AacFactory struct{}

// NewAacFactory returns a factory for the AAC-LC in ADTS decoder.
func NewAacFactory() *AacFactory { return &AacFactory{} }

func (f *AacFactory) Name() string { return "aac" }

// FriendlyName is the label a UI shows for this codec. It repeats the family
// name deliberately: the picker renders "{family} {friendly}" but suppresses
// the repetition, so "Aac" reads as one word rather than "Aac Aac". Unlike
// Opus, where the label names a property ("Portable"), there is no second AAC
// variant to distinguish, so no qualifier is added.
func (f *AacFactory) FriendlyName() string { return "Aac" }

// Weight only breaks ties between codecs that claim the same extension. No other
// factory claims .aac, so this value never competes.
func (f *AacFactory) Weight() int { return 90 }

func (f *AacFactory) Exts() []string { return []string{".aac"} }

// Match claims an ADTS syncword. The 12-bit sync is 0xFFF, so the first byte
// must be 0xFF and the second byte's high nibble must be 0xF. The mask 0xF6
// additionally requires the two MPEG layer bits (byte 1, bits 2 and 1) to be
// zero, which ADTS mandates; it deliberately ignores the MPEG version bit and
// the protection bit, which legitimately vary between valid streams.
//
// A match needs two bytes, so a lone 0xFF is never claimed, and MPEG audio
// frames set the layer bits non-zero: 0xFF 0xFB (Layer III) and its siblings
// fall out of the mask and are left to the codec that owns them.
func (f *AacFactory) Match(magic []byte) bool {
	return len(magic) >= 2 && magic[0] == 0xFF && magic[1]&0xF6 == 0xF0
}

func (f *AacFactory) Open(path string) (Decoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	dec, err := pcm.NewDecoder(file)
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("decode: open AAC %s: %w", path, err)
	}

	d, err := newAacDecoder(file, dec)
	if err != nil {
		file.Close()

		return nil, err
	}

	return d, nil
}

// Probe reads the first ADTS header and reports the stream shape. It never
// decodes audio and never scans the file, so DurationScan stays as cheap as
// DurationUnknown: ADTS carries no total, so the count is honestly -1.
func (f *AacFactory) Probe(path string, _ ProbeOptions) (core.StreamInfo, error) {
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
	// NewDecoder syncs to an ADTS header and parses its rate and channel
	// configuration; touching Info confirms the header was usable without
	// decoding a frame.
	if sm := dec.Info(); sm.SampleRate < 1 || sm.Channels < 1 || sm.Channels > 2 {
		return info, fmt.Errorf("decode: AAC reports an unusable stream %d Hz %d ch",
			sm.SampleRate, sm.Channels)
	}

	return info, nil
}

// aacDecoder adapts go-aac's byte-oriented PCM reader to the engine's
// ReadFrames contract, normalizing rate and channels through the shared
// pcmConverter. go-aac emits interleaved little-endian S16, so the stored
// width passed to the converter is a constant 2.
type aacDecoder struct {
	file *os.File
	dec  *pcm.Decoder

	conv *pcmConverter

	// total is the playable frame count in the canonical domain. ADTS cannot
	// state one, so it stays -1 and delivery is never clamped.
	total int64

	// raw receives S16 bytes from go-aac, cut to whole source frames.
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

func newAacDecoder(file *os.File, dec *pcm.Decoder) (*aacDecoder, error) {
	sm := dec.Info()
	if sm.SampleRate < 1 || sm.Channels < 1 || sm.Channels > 2 {
		return nil, fmt.Errorf("decode: AAC reports an unusable stream %d Hz %d ch",
			sm.SampleRate, sm.Channels)
	}

	const bytesPS = 2 // go-aac always hands back S16

	d := &aacDecoder{
		file:  file,
		dec:   dec,
		conv:  newPCMConverter(sm.SampleRate, sm.Channels, bytesPS),
		total: -1,
	}
	d.raw = rawFrames()
	// out is one block of canonical stereo; pending aliases it, so it must not
	// be resized while a caller still holds frames from it.
	d.out = outBlock()

	return d, nil
}

func (d *aacDecoder) Info() core.StreamInfo {
	return core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: d.total}
}

// DecoderName names the codec implementation behind this decoder.
func (d *aacDecoder) DecoderName() string { return "go-aac" }

// ParserName names the container handling. The ADTS framing is go-aac's; the
// parenthetical keeps it distinct from the raw-access-unit path the same
// library offers for MP4.
func (d *aacDecoder) ParserName() string { return "go-aac (adts)" }

func (d *aacDecoder) ReadFrames(dst []float32) (int, error) {
	return readCanonical(dst, d.out, &d.pending, d.fill, &d.pos, d.total, d.closed)
}

// fill decodes and normalizes until one block of stereo output is ready.
func (d *aacDecoder) fill() error {
	return pcmFillLoop(d.conv, d.out, &d.pending, &d.eof, d.dec.Read, d.raw, d.conv.feed)
}

func (d *aacDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.pending = nil
	d.conv.src = nil
	// go-aac's Decoder holds no OS resource of its own, but honoring an
	// io.Closer keeps this adapter correct if the library ever grows one.
	if c, ok := any(d.dec).(io.Closer); ok {
		_ = c.Close()
	}
	d.dec = nil
	if d.file != nil {
		d.file.Close()
		d.file = nil
	}

	return nil
}
