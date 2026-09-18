// Opus decoding through github.com/pion/opus. This is the pure-Go path: it
// needs no runtime shared library and can therefore also decode from an
// arbitrary io.Reader.
//
// The Ogg container is parsed by the native reader in oggopus.go, which walks
// the page headers once at open and indexes them, so a seek is a binary search
// plus a short decode forward instead of a reopen and decode-from-zero.
// pion/opus is used for the codec only.
//
// Dependencies: github.com/pion/opus.

package decode

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/dlcuy22/player/core"
	"github.com/pion/opus"
)

// opusTagsTag identifies the mandatory comment header of an Ogg Opus stream.
var opusTagsTag = []byte("OpusTags")

// maxOpusPacketSamples is Opus's 120 ms ceiling: 48000 * 0.12 frames, two
// channels interleaved. One allocation at open covers every packet.
const maxOpusPacketSamples = 48000 * 12 / 100 * 2

// seekPrerollGranules is how far before a seek target the reader starts. It is
// passed to SeekGranule so the chosen page precedes the target and the codec
// rebuilds inter-frame state from real packets before the target is reached.
// RFC 7845 section 4.6 recommends 3840 samples (80 ms at 48 kHz).
const seekPrerollGranules = 3840

// init registers the pure-Go decoder. Its weight makes it the automatic
// default for Ogg Opus: it has no runtime library dependency, unlike the
// libopusfile path, so it is the safer choice when both are registered.
func init() {
	Register(NewPionOpusFactory())
}

// PionOpusFactory decodes Ogg Opus without cgo.
type PionOpusFactory struct{}

func NewPionOpusFactory() *PionOpusFactory { return &PionOpusFactory{} }

func (f *PionOpusFactory) Name() string { return "opus-pion" }

// FriendlyName is the label a UI shows for this codec.
func (f *PionOpusFactory) FriendlyName() string { return "Portable" }

// Weight makes this the automatic default over libopusfile: it needs no
// runtime shared library.
func (f *PionOpusFactory) Weight() int { return 90 }

func (f *PionOpusFactory) Exts() []string { return []string{".opus", ".ogg"} }

// Match accepts any Ogg stream; Open then rejects non-Opus payloads. Checking
// deeper would mean seeking, which sniffing must not do.
func (f *PionOpusFactory) Match(magic []byte) bool {
	return len(magic) >= 4 && string(magic[:4]) == oggCapture
}

func (f *PionOpusFactory) Open(path string) (Decoder, error) {
	return newPionOpusDecoder(pionPathSource(path))
}

// OpenReader decodes from a stream rather than a path. A reader that can seek
// keeps native seek; one that cannot plays forward but refuses to reposition.
func (f *PionOpusFactory) OpenReader(r io.Reader) (Decoder, error) {
	return newPionOpusDecoder(pionReaderSource{r})
}

// Probe reads the tail of the file for the final granule position. It never
// decodes audio, so even a probe on a large file stays cheap.
func (f *PionOpusFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	return probeOggOpus(path, opts.Duration, oggTailWindow)
}

// pionSource supplies the container stream. open is called once, when the
// decoder is built; a seek repositions the stream in place rather than
// reopening, because the native reader owns its io.ReadSeeker.
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

// A path is measured at open by the reader itself, so a seek can always reuse
// the same file handle.
func (p pionPathSource) rewindable() bool { return true }

type pionReaderSource struct{ r io.Reader }

func (s pionReaderSource) open() (io.Reader, io.Closer, error) {
	return s.r, nil, nil
}

// A reader is seekable only when it declares io.Seeker; the decoder uses the
// same io.ReadSeeker for every seek, so nothing needs to rewind.
func (s pionReaderSource) rewindable() bool {
	_, ok := s.r.(io.Seeker)

	return ok
}

// opusPacketSource is the container surface the decoder drives. Both the
// seekable OggOpusReader and the forward-only fallback satisfy it; the fallback
// reports an unknown total and refuses SeekGranule.
type opusPacketSource interface {
	PreSkip() int
	OutputGainQ78() int16
	TotalGranule() int64
	Position() int64
	ReadPacket() (packet []byte, granule int64, err error)
	SeekGranule(target, preroll int64) error
}

// openOpusPackets builds the container reader for r. A seekable source gets
// the index-building reader; anything else falls back to a forward-only reader
// that never buffers the stream.
func openOpusPackets(r io.Reader) (opusPacketSource, error) {
	if rs, ok := r.(io.ReadSeeker); ok {
		return NewOggOpusReader(rs)
	}

	return newForwardOggOpus(r)
}

type pionOpusDecoder struct {
	src pionSource

	srcCloser io.Closer
	reader    opusPacketSource
	dec       opus.Decoder

	// gain is the linear form of the header's Q7.8 dB output gain.
	gain float32

	// skip counts pre-skip frames still owed to the encoder's warm-up, which
	// must not reach the output. It is zero after a seek, where the discard
	// step accounts for the offset instead.
	skip int

	// pending holds gain-applied, pre-skip-trimmed frames not yet delivered.
	pending []float32
	decBuf  []float32

	// total is the playable frame count from the granule index, or -1 when the
	// source is forward-only or the stream is truncated. It is cached so Info
	// stays valid even after Close.
	total int64

	pos    int64
	closed bool
}

func newPionOpusDecoder(src pionSource) (*pionOpusDecoder, error) {
	d := &pionOpusDecoder{
		src:    src,
		decBuf: make([]float32, maxOpusPacketSamples),
	}
	if err := d.openSource(); err != nil {
		return nil, err
	}

	return d, nil
}

// openSource builds the container reader and a fresh codec at the start of the
// stream. It runs once; a seek reuses the reader instead of reopening it.
func (d *pionOpusDecoder) openSource() error {
	r, closer, err := d.src.open()
	if err != nil {
		return err
	}
	d.srcCloser = closer

	reader, err := openOpusPackets(r)
	if err != nil {
		d.detachSource()

		return fmt.Errorf("decode: parse Ogg Opus header: %w", err)
	}
	dec, err := opus.NewDecoderWithOutput(48000, 2)
	if err != nil {
		d.detachSource()

		return fmt.Errorf("decode: init Opus decoder: %w", err)
	}

	d.reader = reader
	d.dec = dec
	d.gain = float32(math.Pow(10, float64(reader.OutputGainQ78())/(20*256)))
	d.skip = reader.PreSkip()
	d.total = totalFrames(reader)
	d.pending = nil
	d.pos = 0

	return nil
}

// totalFrames converts the last granule to playable frames. The granule domain
// includes the pre-skip (RFC 7845 section 4), so the playable length is
// granule - preSkip. It is -1 when the source cannot know its end.
func totalFrames(reader opusPacketSource) int64 {
	total := reader.TotalGranule()
	if total < 0 || total < int64(reader.PreSkip()) {
		return -1
	}

	return total - int64(reader.PreSkip())
}

// detachSource releases the reader of a failed open so a retry starts clean
// instead of leaving a half-built decoder holding the old handle.
func (d *pionOpusDecoder) detachSource() {
	if d.srcCloser != nil {
		d.srcCloser.Close()
		d.srcCloser = nil
	}
	d.reader = nil
}

func (d *pionOpusDecoder) Info() core.StreamInfo {
	return core.StreamInfo{
		Format: core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		// The page index built at open already carries the final granule, so
		// the total is free here. It stays -1 for a forward-only source or a
		// truncated stream, where the true end is genuinely unknown.
		TotalFrames: d.total,
	}
}

// DecoderName names the Opus implementation behind this decoder.
func (d *pionOpusDecoder) DecoderName() string { return "pion/opus" }

// ParserName names the container reader this decoder consumes. It is this
// package's native reader, not pion's forward-only oggreader.
func (d *pionOpusDecoder) ParserName() string { return "player/decode (oggopus)" }

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
		packet, _, err := d.reader.ReadPacket()
		if err != nil {
			return err
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

// SeekFrame repositions the stream so the next frame delivered is frame. The
// frame target is mapped to the granule domain (which counts pre-skip), the
// reader jumps to a page at or before it, and the codec decodes and discards
// the short remainder. No reopen and no decode from zero.
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

	preSkip := int64(d.reader.PreSkip())
	if err := d.reader.SeekGranule(seekGranuleFor(frame, preSkip), seekPrerollGranules); err != nil {
		return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
	}

	// The jump lands before the target, so the codec starts without the state
	// earlier packets built. A fresh decoder is warmed from the pre-roll page
	// on the way to the target. Position is a granule lower bound for the next
	// packet, so subtracting pre-skip yields a playable frame at or before the
	// target; the discard loop closes the rest of the gap.
	dec, err := opus.NewDecoderWithOutput(48000, 2)
	if err != nil {
		return fmt.Errorf("decode: init Opus decoder: %w", err)
	}
	d.dec = dec
	d.pending = nil
	d.skip = 0
	d.pos = d.reader.Position() - preSkip

	return d.discardTo(frame)
}

// seekGranuleFor maps a playable frame to the granule domain. Granules count
// decoded samples including the pre-skip (RFC 7845 section 4), so the seek
// target is the frame plus pre-skip. Getting this wrong still produces correct
// PCM because discardTo closes the gap, but it makes the seek walk extra pages
// and can leave the codec without warm-up state when the target is early.
func seekGranuleFor(frame, preSkip int64) int64 { return frame + preSkip }

// discardTo decodes forward until exactly frame playable frames are accounted
// for. The frames before the target are dropped; frames past it are kept in
// pending so the refill does not decode them twice.
func (d *pionOpusDecoder) discardTo(frame int64) error {
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
	d.reader = nil

	if d.srcCloser != nil {
		d.srcCloser.Close()
		d.srcCloser = nil
	}

	return nil
}

// forwardOggOpus reads Opus audio packets from a stream that cannot seek. It
// offers the same operations as OggOpusReader but holds no index: a
// forward-only source cannot know its length, and the decoder refuses to seek
// before this type is ever asked to reposition.
type forwardOggOpus struct {
	src *bufio.Reader

	preSkip int
	gainQ78 int16

	// pendingHandoff is the first audio packet, read while checking for the
	// comment header, handed back by the next ReadPacket.
	pendingHandoff []byte
	pendingGranule int64

	// Streaming state, mirroring OggOpusReader's page walk.
	lacing     []byte
	body       []byte
	segIndex   int
	bodyIndex  int
	pageLoaded bool
	granule    int64
	partial    []byte
	pos        int64
}

func newForwardOggOpus(r io.Reader) (*forwardOggOpus, error) {
	f := &forwardOggOpus{src: bufio.NewReaderSize(r, 64*1024), granule: -1}

	head, _, err := f.nextPacket()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: stream has no packets", ErrOggOpusNotOpus)
		}

		return nil, err
	}
	info, err := parseOggOpusHead(head)
	if err != nil {
		return nil, err
	}
	f.preSkip = info.preSkip
	f.gainQ78 = info.gain

	// The second packet is the comment header, or audio in a non-conformant
	// stream. Skipping it by prefix keeps a stream without OpusTags playable.
	pkt, granule, err := f.nextPacket()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return f, nil
		}

		return nil, err
	}
	if !bytes.HasPrefix(pkt, opusTagsTag) {
		f.pendingHandoff = pkt
		f.pendingGranule = granule
	}

	return f, nil
}

func (f *forwardOggOpus) PreSkip() int { return f.preSkip }

func (f *forwardOggOpus) OutputGainQ78() int16 { return f.gainQ78 }

// TotalGranule is always unknown: the tail has not been read yet, and reading
// it would buffer the whole stream.
func (f *forwardOggOpus) TotalGranule() int64 { return -1 }

// Position is the granule lower bound for the next packet, mirroring the
// seekable reader so the decoder's discard logic works unchanged.
func (f *forwardOggOpus) Position() int64 { return f.pos }

// SeekGranule always fails: a forward-only stream cannot be repositioned. The
// decoder checks rewindable before calling this, so it is a backstop.
func (f *forwardOggOpus) SeekGranule(int64, int64) error {
	return errors.New("decode: source is not seekable")
}

// loadPage reads and verifies one page straight from the stream.
func (f *forwardOggOpus) loadPage() error {
	page := make([]byte, oggHeaderLen)
	if _, err := io.ReadFull(f.src, page); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}

		return fmt.Errorf("%w: truncated page header: %w", ErrOggOpusBadPage, err)
	}
	if string(page[:4]) != oggCapture {
		return fmt.Errorf("%w: no Ogg capture pattern", ErrOggOpusBadPage)
	}

	segments := int(page[oggSegmentOff])
	lacingOff := len(page)
	page = append(page, make([]byte, segments)...)
	if _, err := io.ReadFull(f.src, page[lacingOff:]); err != nil {
		return fmt.Errorf("%w: truncated page lacing: %w", ErrOggOpusBadPage, err)
	}

	body := 0
	for _, l := range page[lacingOff:] {
		body += int(l)
	}
	bodyOff := len(page)
	page = append(page, make([]byte, body)...)
	if _, err := io.ReadFull(f.src, page[bodyOff:]); err != nil {
		return fmt.Errorf("%w: truncated page body: %w", ErrOggOpusBadPage, err)
	}

	stored := binary.LittleEndian.Uint32(page[22:26])
	binary.LittleEndian.PutUint32(page[22:26], 0)
	if got := oggOpusChecksum(page); got != stored {
		return fmt.Errorf("%w: page has %08x, want %08x", ErrOggOpusChecksum, got, stored)
	}

	f.lacing = page[lacingOff : lacingOff+segments]
	f.body = page[bodyOff:]
	f.granule = int64(binary.LittleEndian.Uint64(page[oggGranuleOff : oggGranuleOff+oggGranuleSize]))
	f.segIndex = 0
	f.bodyIndex = 0
	f.pageLoaded = true

	return nil
}

// nextPacket reassembles one packet across lacing values and pages, exactly
// like OggOpusReader but without ever seeking backwards. The stream starts at a
// packet boundary, so the mid-packet skip the seekable reader needs for a
// post-seek start never applies here.
func (f *forwardOggOpus) nextPacket() ([]byte, int64, error) {
	if f.pendingHandoff != nil {
		packet, granule := f.pendingHandoff, f.pendingGranule
		f.pendingHandoff = nil
		f.pendingGranule = 0

		return packet, granule, nil
	}

	for {
		if !f.pageLoaded {
			if err := f.loadPage(); err != nil {
				if errors.Is(err, io.EOF) && len(f.partial) > 0 {
					return nil, -1, io.ErrUnexpectedEOF
				}

				return nil, -1, err
			}
		}

		for f.segIndex < len(f.lacing) {
			size := int(f.lacing[f.segIndex])
			f.segIndex++
			segment := f.body[f.bodyIndex : f.bodyIndex+size]
			f.bodyIndex += size

			f.partial = append(f.partial, segment...)
			if size == 255 {
				continue
			}

			packet := f.partial
			f.partial = nil

			return packet, f.granule, nil
		}
		// Packet boundaries are contiguous, so the next packet starts at this
		// page's granule; a -1 page leaves the last known bound in place.
		if f.granule >= 0 {
			f.pos = f.granule
		}
		f.pageLoaded = false
	}
}

// ReadPacket returns the next audio packet and the granule of the page it
// completed on.
func (f *forwardOggOpus) ReadPacket() ([]byte, int64, error) {
	return f.nextPacket()
}
