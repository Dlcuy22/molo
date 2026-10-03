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

// The warm-up window is how far before a seek target the reader starts. It is
// passed to SeekGranule so the chosen page precedes the target and the codec
// rebuilds inter-frame state from real packets before the target is reached.
// The landing page plus the warm-up skip bound a seek's decoded packets by this
// window instead of by the distance to the target, so the window is the one
// knob that trades seek cost against how exactly the landing PCM matches a
// straight decode.
//
// The two values below are the two points on that curve, exposed as two
// registry entries:
//
//   - pionWarmupFast is the RFC 7845 section 4.6 nominal pre-roll, 3840 samples
//     (80 ms). libopusfile uses this window too and documents that the first
//     frames may differ from a straight decode. Measured on this repository's
//     fixtures, the difference is a bounded transient (peak around -20 dBFS)
//     whose RMS falls below -60 dBFS within 100 ms; exact float32 equality is
//     reached by roughly 600 ms. It is the default because a seek costs about
//     1 ms on 1 s pages.
//   - pionWarmupExact is 38400 samples (800 ms). pion's CELT coarse-energy
//     predictor carries state across roughly 30 frames, and only a window this
//     large makes the seeked PCM byte-for-byte equal to a straight decode on
//     these fixtures. It costs about 5x the fast seek, so it is an explicit
//     opt-in.
const (
	pionWarmupFast  = 3840
	pionWarmupExact = 38400
)

// init registers both pure-Go variants. Only the fast one carries a weight
// above libopusfile, so it stays the automatic default; the exact variant is
// selected by name. Both need no runtime library, unlike the libopusfile path.
func init() {
	Register(NewPionOpusFactory())
	Register(NewPionOpusExactFactory())
}

// PionOpusFactory decodes Ogg Opus without cgo, using the fast warm-up window.
type PionOpusFactory struct{}

// NewPionOpusFactory returns a factory for the pure-Go Ogg Opus decoder that
// uses the fast warm-up window.
func NewPionOpusFactory() *PionOpusFactory { return &PionOpusFactory{} }

func (f *PionOpusFactory) Name() string { return "opus-pion" }

// FriendlyName is the label a UI shows for this codec.
func (f *PionOpusFactory) FriendlyName() string { return "Portable" }

// Weight makes this the automatic default over libopusfile: it needs no
// runtime shared library. It is also above the exact variant, so adding the
// bit-perfect option cannot move the default.
func (f *PionOpusFactory) Weight() int { return 90 }

func (f *PionOpusFactory) Exts() []string { return []string{".opus", ".ogg"} }

// Match accepts any Ogg stream; Open then rejects non-Opus payloads. Checking
// deeper would mean seeking, which sniffing must not do.
func (f *PionOpusFactory) Match(magic []byte) bool { return matchOgg(magic) }

func (f *PionOpusFactory) Open(path string) (Decoder, error) {
	return newPionOpusDecoder(pionPathSource(path), pionWarmupFast)
}

// OpenReader decodes from a stream rather than a path. A reader that can seek
// keeps native seek; one that cannot plays forward but refuses to reposition.
func (f *PionOpusFactory) OpenReader(r io.Reader) (Decoder, error) {
	return newPionOpusDecoder(pionReaderSource{r}, pionWarmupFast)
}

// Probe reads the tail of the file for the final granule position. It never
// decodes audio, so even a probe on a large file stays cheap.
func (f *PionOpusFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	return probeOggOpus(path, opts.Duration, oggTailWindow)
}

// PionOpusExactFactory is the same pure-Go codec with the large warm-up window
// that makes a seek's PCM bit-exact against a straight decode. Its weight is
// below PionOpusFactory's, so it is never chosen automatically.
type PionOpusExactFactory struct{}

// NewPionOpusExactFactory returns a factory for the pure-Go Ogg Opus decoder
// whose large warm-up window makes a seek bit-exact.
func NewPionOpusExactFactory() *PionOpusExactFactory { return &PionOpusExactFactory{} }

func (f *PionOpusExactFactory) Name() string { return "opus-pion-exact" }

// FriendlyName is the label a UI shows for this codec.
func (f *PionOpusExactFactory) FriendlyName() string { return "Bit-perfect" }

// Weight keeps this below the fast pure-Go factory: it is bit-exact after a
// seek but its warm-up makes a seek several times more expensive, so it must
// never win automatic selection.
func (f *PionOpusExactFactory) Weight() int { return 85 }

func (f *PionOpusExactFactory) Exts() []string { return []string{".opus", ".ogg"} }

func (f *PionOpusExactFactory) Match(magic []byte) bool { return matchOgg(magic) }

func (f *PionOpusExactFactory) Open(path string) (Decoder, error) {
	return newPionOpusDecoder(pionPathSource(path), pionWarmupExact)
}

func (f *PionOpusExactFactory) OpenReader(r io.Reader) (Decoder, error) {
	return newPionOpusDecoder(pionReaderSource{r}, pionWarmupExact)
}

func (f *PionOpusExactFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	return probeOggOpus(path, opts.Duration, oggTailWindow)
}

// matchOgg is the shared content check for both pure-Go factories.
func matchOgg(magic []byte) bool {
	return len(magic) >= 4 && string(magic[:4]) == oggCapture
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
// seekable oggOpusReader and the forward-only fallback satisfy it; the fallback
// reports an unknown total and refuses SeekGranule.
type opusPacketSource interface {
	PreSkip() int
	OutputGainQ78() int16
	TotalGranule() int64
	Position() int64
	PositionExact() bool
	ReadPacket() (packet []byte, granule int64, err error)
	SeekGranule(target, preroll int64) error
}

// openOpusPackets builds the container reader for r. A seekable source gets
// the index-building reader; anything else falls back to a forward-only reader
// that never buffers the stream.
func openOpusPackets(r io.Reader) (opusPacketSource, error) {
	if rs, ok := r.(io.ReadSeeker); ok {
		return newOggOpusReader(rs)
	}

	return newForwardOggOpus(r)
}

// opusContainer opens a packet source from a stream. It is a function rather
// than a hardcoded call so a second container (WebM) can drive the same decoder
// without a second decoder implementation.
type opusContainer func(io.Reader) (opusPacketSource, error)

type pionOpusDecoder struct {
	src pionSource

	srcCloser io.Closer
	reader    opusPacketSource
	dec       opus.Decoder

	// container opens the packet source. It is Ogg for the Opus factories and
	// the WebM reader for the WebM factory, so the decoder below is shared.
	container opusContainer

	// parserName labels the container for Descriptor, so the same decoder
	// reports the container it actually consumed.
	parserName string

	// warmup is the seek pre-roll in granules: the profile's fast/exact window.
	// It is per-decoder rather than a package constant because the two registry
	// entries are the same codec at two points on the cost/accuracy curve.
	warmup int64

	// gain is the linear form of the header's Q7.8 dB output gain.
	gain float32

	// skip counts pre-skip frames still owed to the encoder's warm-up, which
	// must not reach the output. It is zero after a seek, where the discard
	// step accounts for the offset instead.
	skip int

	// pending holds gain-applied, pre-skip-trimmed frames not yet delivered.
	pending []float32
	decBuf  []float32

	// decodedPackets counts codec invocations. A seek uses it to prove that
	// packets which contribute nothing to the output are skipped rather than
	// decoded and discarded; it is read only by tests.
	decodedPackets int

	// total is the playable frame count from the granule index, or -1 when the
	// source is forward-only or the stream is truncated. It is cached so Info
	// stays valid even after Close.
	total int64

	pos    int64
	closed bool
}

func newPionOpusDecoder(src pionSource, warmup int64) (*pionOpusDecoder, error) {
	return newPionOpusDecoderWith(openOpusPackets, src, warmup, "player/decode (oggopus)")
}

// newPionOpusDecoderWith builds the decoder over an explicit container. The
// Ogg path calls it through newPionOpusDecoder, so its behaviour is unchanged;
// the WebM path supplies openWebMOpusPackets and its own parser label.
func newPionOpusDecoderWith(container opusContainer, src pionSource, warmup int64, parserName string) (*pionOpusDecoder, error) {
	d := &pionOpusDecoder{
		src:        src,
		container:  container,
		parserName: parserName,
		warmup:     warmup,
		decBuf:     make([]float32, maxOpusPacketSamples),
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

	reader, err := d.container(r)
	if err != nil {
		d.detachSource()

		// The container is pluggable, so name it from the label the decoder
		// carries rather than hardcoding Ogg: a corrupt WebM saying "Ogg"
		// would send a reader down the wrong path.
		return fmt.Errorf("decode: parse Opus container %s: %w", d.parserName, err)
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
// package's native reader for whichever container was opened: oggopus or
// webmopus.
func (d *pionOpusDecoder) ParserName() string { return d.parserName }

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

		n, err := d.decodePacket(packet)
		if err != nil {
			return err
		}

		frames := d.trim(d.scale(d.decBuf[:n*2]))
		if len(frames) == 0 {
			continue
		}
		d.pending = frames

		return nil
	}
}

// decodePacket runs the codec on one packet into decBuf and returns the frame
// count. Every codec call goes through here so the decoded-packet budget is
// measured in one place.
func (d *pionOpusDecoder) decodePacket(packet []byte) (int, error) {
	d.decodedPackets++

	n, err := d.dec.DecodeToFloat32(packet, d.decBuf)
	if err != nil {
		return 0, fmt.Errorf("decode: opus packet: %w", err)
	}

	return n, nil
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
// reader jumps to a page at or before it, packets that end before the codec's
// warm-up window are advanced over without decoding, and only the short
// remainder is decoded and discarded. No reopen and no decode from zero.
func (d *pionOpusDecoder) SeekFrame(frame int64) error {
	if d.closed {
		return ErrClosed
	}
	if frame < 0 {
		return fmt.Errorf("decode: negative seek target %d", frame)
	}
	if !d.src.rewindable() {
		return ErrNotSeekable
	}

	preSkip := int64(d.reader.PreSkip())
	target := seekGranuleFor(frame, preSkip)
	if err := d.reader.SeekGranule(target, d.warmup); err != nil {
		return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
	}

	// The jump lands before the target, so the codec starts without the state
	// earlier packets built. The existing decoder is reset in place: Init is the
	// OPUS_RESET_STATE equivalent and clears coarse energy, overlap, the
	// postfilter, the range decoder and the SILK resamplers, so reusing it is a
	// fresh decode without a fresh allocation. It is warmed from the pre-roll
	// page on the way to the target. Position is a granule lower bound for the
	// next packet, so subtracting pre-skip yields a playable frame at or before
	// the target; the discard loop closes the rest of the gap.
	if err := d.dec.Init(48000, 2); err != nil {
		return fmt.Errorf("decode: reset Opus decoder: %w", err)
	}
	d.pending = nil
	d.skip = 0
	d.pos = d.reader.Position() - preSkip

	// Everything more than the warm-up window before the target is decoded
	// only to be thrown away. Advance over it packet by packet instead; the
	// packet that straddles the window is decoded, then the discard loop below
	// closes the remaining gap.
	if err := d.skipWarmup(frame, target-d.warmup); err != nil {
		return err
	}

	return d.discardTo(frame)
}

// skipWarmup advances the reader over whole packets whose end lies at or before
// limit (granule domain) without decoding them, leaving the first packet that
// crosses limit for the decode path.
//
// The position of a packet within a page is not in the container, so a running
// sum of packet durations is kept. That sum is only trustworthy when it starts
// at a known packet boundary: a seek can land on a page whose first packet
// continues an unread one, and then the reader drops that packet and a sum from
// the landing granule starts too low, which could skip a packet the codec
// needed. PositionExact reports that case, and the skip is abandoned so the codec
// decodes the landing page as before. From an exact start the sum stays exact
// across page boundaries and spanning packets, so no re-anchoring is needed. A
// packet whose TOC yields no duration is decoded.
func (d *pionOpusDecoder) skipWarmup(frame, limit int64) error {
	if limit <= 0 || !d.reader.PositionExact() {
		return nil
	}

	preSkip := int64(d.reader.PreSkip())
	pos := d.pos + preSkip // granule position of the next packet.
	for pos < limit {
		packet, _, err := d.reader.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The discard loop reports a target past the end.
				return nil
			}

			return err
		}

		samples := int64(opusPacketSamples48(packet))
		if samples <= 0 || pos+samples > limit {
			d.pos = pos - preSkip

			return d.discardFromPacket(frame, packet)
		}
		pos += samples
	}

	d.pos = pos - preSkip

	return nil
}

// discardFromPacket decodes one already-read packet and then hands off to the
// ordinary discard loop, which reads the packets that follow.
func (d *pionOpusDecoder) discardFromPacket(frame int64, packet []byte) error {
	n, err := d.decodePacket(packet)
	if err != nil {
		return err
	}
	d.pending = d.trim(d.scale(d.decBuf[:n*2]))

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
// offers the same operations as oggOpusReader but holds no index: a
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

	// Streaming state, mirroring oggOpusReader's page walk.
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
			return nil, fmt.Errorf("%w: stream has no packets", errOggOpusNotOpus)
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

// PositionExact is false: a forward-only stream never seeks, so its position is
// only ever a lower bound.
func (f *forwardOggOpus) PositionExact() bool { return false }

// SeekGranule always fails with ErrNotSeekable: a forward-only stream cannot be
// repositioned. The decoder checks rewindable before calling this, so it is a
// backstop.
func (f *forwardOggOpus) SeekGranule(int64, int64) error {
	return ErrNotSeekable
}

// loadPage reads and verifies one page straight from the stream.
func (f *forwardOggOpus) loadPage() error {
	page := make([]byte, oggHeaderLen)
	if _, err := io.ReadFull(f.src, page); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}

		return fmt.Errorf("%w: truncated page header: %w", errOggOpusBadPage, err)
	}
	if string(page[:4]) != oggCapture {
		return fmt.Errorf("%w: no Ogg capture pattern", errOggOpusBadPage)
	}

	segments := int(page[oggSegmentOff])
	lacingOff := len(page)
	page = append(page, make([]byte, segments)...)
	if _, err := io.ReadFull(f.src, page[lacingOff:]); err != nil {
		return fmt.Errorf("%w: truncated page lacing: %w", errOggOpusBadPage, err)
	}

	body := 0
	for _, l := range page[lacingOff:] {
		body += int(l)
	}
	bodyOff := len(page)
	page = append(page, make([]byte, body)...)
	if _, err := io.ReadFull(f.src, page[bodyOff:]); err != nil {
		return fmt.Errorf("%w: truncated page body: %w", errOggOpusBadPage, err)
	}

	stored := binary.LittleEndian.Uint32(page[22:26])
	binary.LittleEndian.PutUint32(page[22:26], 0)
	if got := oggOpusChecksum(page); got != stored {
		return fmt.Errorf("%w: page has %08x, want %08x", errOggOpusChecksum, got, stored)
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
// like oggOpusReader but without ever seeking backwards. The stream starts at a
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
