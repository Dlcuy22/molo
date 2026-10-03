// M4A/MP4 decoding through github.com/tphakala/go-m4a's container reader and
// github.com/tphakala/go-aac's frame decoder. This is the AAC path only: an MP4
// holding Opus or FLAC is rejected by the codec constructor, which is the honest
// outcome for a file this factory cannot decode.
//
// The container reader indexes every access unit (it knows each one's file
// offset from the sample tables), so this adapter demuxes MP4 itself and hands
// go-aac whole access units through a raw AudioSpecificConfig decoder, rather
// than letting a bridge pull a forward-only stream. That gives a native seek:
// go-m4a does not expose its cursor, so a seek builds a fresh reader and reads
// forward to the landing access unit, which costs one seek per frame and no
// audio decode.
//
// The decoded stream still carries the encoder priming the container's edit list
// trims, so the adapter drops EncoderDelay leading samples from the first access
// unit and lets the shared delivery loop clamp to the edit-list presentation
// length. What leaves ReadFrames is the playable signal the edit list describes.
//
// Dependencies: github.com/tphakala/go-m4a, github.com/tphakala/go-aac.
package decode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dlcuy22/molo/core"
	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
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

// Weight breaks ties against another codec registered for the same extension,
// the same role the FLAC factory's 90 plays.
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
	// The movie header states an exact presentation duration; converting it to
	// source samples gives a length that survives even when TotalFrames is -1
	// because the source does not divide evenly into canonical frames.
	if samples, ok := durationSamples(md.Duration, md.SampleRate); ok {
		info.SourceSamples = samples
		info.SourceRate = md.SampleRate
	}

	return info, nil
}

// m4aDecoder adapts go-m4a's container reader and go-aac's frame decoder to the
// engine's ReadFrames contract, normalizing rate, channels and sample width
// through the shared pcmConverter.
type m4aDecoder struct {
	file *os.File
	// rd demuxes the container; it is replaced on every seek.
	rd  *m4a.Reader
	dec *aacpcm.FrameDecoder

	conv *pcmConverter

	// frameCount is the number of access units the container declares.
	frameCount int
	// auIndex is the next access unit to read.
	auIndex int
	// total is the playable frame count in the canonical domain, or -1.
	total int64

	// srcSamples and srcRate are the exact source-domain presentation length,
	// carried so Info can still report a duration when total is -1.
	srcSamples int64
	srcRate    int

	// encoderDelay is the leading encoder priming, in source samples, that the
	// raw decoder emits but the edit list excludes. It also locates the access
	// unit a canonical seek target falls in.
	encoderDelay int64
	// skipBytes is the remaining priming, in source bytes, to drop from the
	// front. It is spent after the first delivery and is not re-applied after a
	// seek, because priming is a start-of-track trim, not a per-seek one.
	skipBytes int64

	// au receives one access unit read from the container.
	au []byte
	// prevAU retains the access unit before the seek landing one, for the
	// overlap-add priming.
	prevAU []byte
	// s16 receives S16 bytes from go-aac, grown in place across frames.
	s16 []byte
	// raw is the buffer pcmFillLoop hands to readPCM.
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
	rd, err := m4a.NewReader(file)
	if err != nil {
		return nil, err
	}

	info := rd.Info()
	if info.Codec != m4a.CodecAACLC {
		return nil, fmt.Errorf("decode: M4A track codec is %s, not AAC-LC", info.Codec)
	}
	// The frame decoder parses the AudioSpecificConfig up front, so an HE-AAC
	// stream is rejected here. Its typed error is returned unwrapped so it stays
	// errors.Is-visible as aacpcm.ErrUnsupportedSBR/PS to the routing caller.
	dec, err := aacpcm.NewRawDecoder(info.ASC)
	if err != nil {
		return nil, err
	}
	// The decoder accepted the config, but Info still drives buffer sizing, so
	// a malformed sample entry must not reach the frame-size division.
	if info.SampleRate < 1 || info.Channels < 1 {
		return nil, fmt.Errorf("decode: M4A reports an unusable stream %d Hz %d ch",
			info.SampleRate, info.Channels)
	}

	return newM4aDecoderFrom(file, rd, dec, info), nil
}

// newM4aDecoderFrom builds the adapter from an already-open container reader,
// its frame decoder and its container info.
func newM4aDecoderFrom(file *os.File, rd *m4a.Reader, dec *aacpcm.FrameDecoder, info m4a.Info) *m4aDecoder {
	// go-aac always yields interleaved little-endian S16.
	const bytesPS = 2

	d := &m4aDecoder{
		file:         file,
		rd:           rd,
		dec:          dec,
		conv:         newPCMConverter(info.SampleRate, info.Channels, bytesPS),
		frameCount:   info.FrameCount,
		total:        m4aTotalFrames(info),
		encoderDelay: info.EncoderDelay,
	}
	if d.encoderDelay < 0 {
		d.encoderDelay = 0
	}
	if samples, ok := durationSamples(info.Duration, info.SampleRate); ok {
		d.srcSamples = samples
		d.srcRate = info.SampleRate
	}
	d.skipBytes = m4aPrimingBytes(info)
	d.raw = rawFrames()
	// au starts small; m4aReadInto grows it to the first frame's declared size.
	d.au = make([]byte, 2048)
	// out is one block of canonical stereo; pending aliases it, so it must not
	// be resized while a caller still holds frames from it.
	d.out = outBlock()

	return d
}

func (d *m4aDecoder) Info() core.StreamInfo {
	return core.StreamInfo{
		Format:        core.CanonicalFormat,
		TotalFrames:   d.total,
		SourceSamples: d.srcSamples,
		SourceRate:    d.srcRate,
	}
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
	return pcmFillLoop(d.conv, d.out, &d.pending, &d.eof, d.readPCM, d.raw, d.conv.feed)
}

// readPCM reads the next access unit from the container, decodes it, and drops
// any remaining encoder priming before the converter sees it.
func (d *m4aDecoder) readPCM(dst []byte) (int, error) {
	if d.closed {
		return 0, ErrClosed
	}
	if d.auIndex >= d.frameCount {
		return 0, io.EOF
	}

	au, err := m4aReadInto(d.rd, &d.au)
	if err != nil {
		return 0, fmt.Errorf("decode: read M4A frame %d: %w", d.auIndex, err)
	}
	out, samples, err := d.dec.DecodeFrame(d.s16[:0], au)
	if err != nil {
		return 0, fmt.Errorf("decode: decode M4A frame %d: %w", d.auIndex, err)
	}
	d.s16 = out
	d.auIndex++

	if samples == 0 {
		return 0, nil
	}

	// The raw decoder emits the priming the edit list trims. Drop it from the
	// front, counted in bytes so the trim stays exact at any source rate. It is
	// spent once and never re-applied after a seek.
	data := d.s16
	if d.skipBytes > 0 {
		rest, dropped := d.conv.skipFrameBytes(data, d.skipBytes)
		d.skipBytes -= dropped
		data = rest
	}

	// One AAC-LC access unit is at most 1024 samples of interleaved S16, far
	// under the raw buffer, so this copy is always whole.
	return copy(dst, data), nil
}

// SeekFrame repositions so the next delivered frame is frame. go-m4a exposes no
// cursor, so this builds a fresh reader and reads forward to the landing access
// unit, discarding those access units without decoding them. AAC-LC frames
// cannot start mid-frame, so a canonical target is converted to a source sample,
// raised by the encoder delay to a source position, and floored to the access
// unit at or before it. pos is set from the landing unit, so it is authoritative
// even when the target sat inside a 1024-sample frame.
func (d *m4aDecoder) SeekFrame(frame int64) error {
	if d.closed {
		return ErrClosed
	}
	if frame < 0 {
		return fmt.Errorf("decode: negative seek target %d", frame)
	}

	srcSample := canonicalSamples(frame, d.conv.srcRate)

	// Every AAC-LC access unit is 1024 samples per channel, so the source sample
	// raised by the encoder delay lands in this unit. Clamp to the track.
	auIndex := int64(0)
	if d.frameCount > 0 {
		auIndex = (srcSample + d.encoderDelay) / 1024
		if last := int64(d.frameCount) - 1; auIndex > last {
			auIndex = last
		}
	}

	// Reopen by building a fresh reader, which resets its unexported cursor, and
	// read forward to the landing unit. The unit before it is kept so the
	// decoder's overlap-add history can be primed after the reset.
	rd, err := m4a.NewReader(d.file)
	if err != nil {
		return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
	}
	for i := int64(0); i < auIndex; i++ {
		au, err := m4aReadInto(rd, &d.au)
		if err != nil {
			return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
		}
		if i == auIndex-1 {
			d.prevAU = append(d.prevAU[:0], au...)
		}
	}

	if err := d.dec.Reset(); err != nil {
		return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
	}
	// AAC-LC reconstructs each frame by overlap-add with the previous frame, so
	// the decoder needs the unit before the landing one for its history. Decode
	// it into scratch and discard the output; without this the first landing
	// frame is windowed against silence and the seek would not match a straight
	// decode. This mirrors the priming decode/aac.go does, and go-mp3 inside
	// SeekToSample.
	if auIndex > 0 && len(d.prevAU) > 0 {
		if _, _, err := d.dec.DecodeFrame(d.s16[:0], d.prevAU); err != nil {
			return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
		}
	}

	d.rd = rd
	d.auIndex = int(auIndex)

	// Anything the resampler still held belongs to the abandoned position, and
	// the priming trim is a start-of-track concern that is already spent.
	d.conv.reset()
	d.pending = nil
	d.eof = false
	d.skipBytes = 0
	// The first delivered sample is the landing unit's start, which is the
	// source sample after the encoder delay, so the next streamer position is
	// that unit's position in the playable stream.
	landingStartSample := auIndex*1024 - d.encoderDelay
	if landingStartSample < 0 {
		landingStartSample = 0
	}
	d.pos = canonicalFramesOf(landingStartSample, d.conv)

	return nil
}

func (d *m4aDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.pending = nil
	d.prevAU = nil
	d.conv.src = nil
	d.rd = nil
	// go-aac's FrameDecoder holds no OS resource of its own, but honoring an
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

// m4aReadInto reads the next access unit from rd into buf, growing buf when the
// reader reports the needed size with io.ErrShortBuffer. The grown buffer is
// assigned back through buf so the caller keeps reusing it.
func m4aReadInto(rd *m4a.Reader, buf *[]byte) ([]byte, error) {
	for {
		n, err := rd.ReadFrameInto(*buf)
		if errors.Is(err, io.ErrShortBuffer) {
			// The reader reports the required size and reads nothing; a value
			// not larger than the buffer we already gave it cannot be grown.
			if n <= len(*buf) {
				return nil, fmt.Errorf("decode: M4A access unit needs %d bytes", n)
			}
			*buf = make([]byte, n)

			continue
		}
		if err != nil {
			return nil, err
		}

		return (*buf)[:n], nil
	}
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
