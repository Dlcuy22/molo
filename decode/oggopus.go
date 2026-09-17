// Native Ogg Opus container reader with granule-index seeks.
//
// Purpose:
//   Read Opus audio packets out of an Ogg bitstream (RFC 3533) without
//   decoding, validate the Opus-specific framing (RFC 7845), and reposition to
//   a granule position with a binary search over a page index. The pion
//   container reader is forward-only, so its seek decodes and discards from the
//   start of the file; this reader walks page headers once and then jumps
//   straight to one page per seek.
//
// Key Components:
//   - NewOggOpusReader(): parses OpusHead, skips OpusTags, builds the page index
//   - OggOpusReader.ReadPacket(): reassembles one packet over pages
//   - OggOpusReader.SeekGranule(): binary search, then repositions for reading
//   - OggOpusReader.Position(): granule lower bound for the next packet
//
// Granule domain (RFC 7845 section 4):
//   The granule counts decoded samples at 48 kHz and includes the pre-skip, so
//   a stream's playable length is lastGranule - preSkip and a seek to playable
//   frame f targets granule f + preSkip. A page spanned by a packet that
//   completes on a later page has no granule; the field is the two's complement
//   -1 and such pages are never indexed. Only pages where at least one packet
//   completes carry a usable position.
//
// Dependencies:
//   - Reuses the Ogg framing constants and helpers from ogg_probe.go; it does
//     not change or duplicate that probe path.
//
// Error Types:
//   - ErrOggOpusNotOpus: the first packet is not an OpusHead
//   - ErrOggOpusChecksum: a page consumed for reading failed its CRC
//   - ErrOggOpusBadPage: Ogg framing is malformed
//   - ErrOggOpusUnsupportedMapping: a channel mapping family other than 0
//
// Ownership:
//   The reader never owns, closes, or spawns goroutines around the io.ReadSeeker
//   it is given. The caller keeps that responsibility.

package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	// oggOpusSampleRate is fixed by RFC 7845 section 5.1: granule positions and
	// Opus decoding always count samples at 48 kHz.
	oggOpusSampleRate = 48000

	// oggFlagContinued marks a page whose first packet continues from the
	// previous page (RFC 3533).
	oggFlagContinued = 0x01

	// oggOpusHeadMinLen is the fixed part of the ID header through the mapping
	// family octet (RFC 7845 section 5.1, Figure 2).
	oggOpusHeadMinLen = 19
)

// Errors returned by the reader. Callers can test them with errors.Is.
var (
	ErrOggOpusNotOpus            = errors.New("decode: first packet is not OpusHead")
	ErrOggOpusChecksum           = errors.New("decode: Ogg page checksum mismatch")
	ErrOggOpusBadPage            = errors.New("decode: malformed Ogg page")
	ErrOggOpusUnsupportedMapping = errors.New("decode: unsupported Opus channel mapping family")
)

// oggOpusPageEntry is one seekable page. start is the granule at which the
// first packet on the page begins, which is the previous page's granule by the
// contiguity rule in RFC 7845 section 4. It is a lower bound: pages before the
// first audio page may have been cropped, and individual packets on the page
// begin later than start.
type oggOpusPageEntry struct {
	offset  int64
	granule int64
	start   int64
}

// oggOpusPage is a parsed page held by the packet reader.
type oggOpusPage struct {
	offset  int64
	end     int64
	flags   byte
	granule int64
	lacing  []byte
	body    []byte
}

func (p oggOpusPage) continued() bool { return p.flags&oggFlagContinued != 0 }

// OggOpusReader reads Opus audio packets from an Ogg bitstream and can seek to
// a granule position. It is not safe for concurrent use.
type OggOpusReader struct {
	src      io.ReadSeeker
	fileSize int64

	preSkip  int
	gainQ78  int16
	channels int

	// total is the last audio page granule, or -1 when the file is truncated
	// and the true end is unknown.
	total int64
	// index holds every audio page with a completed packet and a real granule,
	// ordered by granule.
	index []oggOpusPageEntry

	// Streaming state.
	pageOffset int64
	page       oggOpusPage
	pageLoaded bool
	segIndex   int
	bodyIndex  int

	partial    []byte
	discarding bool

	// pending is a first audio packet read while probing for OpusTags, handed
	// back by the next ReadPacket so nothing is consumed twice.
	pending        []byte
	pendingGranule int64

	// packetStart is the offset of the page where the in-progress packet began.
	packetStart int64

	// pos is the granule lower bound for the next packet, updated at page
	// boundaries where the exact value is known.
	pos int64
}

// NewOggOpusReader parses the stream headers and indexes its audio pages.
//
// The ID header is validated (magic, version, channel count, mapping family)
// and the comment header is skipped. The returned reader is positioned just
// before the first audio packet. The input is read but not retained as owned:
// the caller closes it.
//
// Errors: ErrOggOpusNotOpus for a non-Opus first packet,
// ErrOggOpusUnsupportedMapping for a mapping family other than 0,
// ErrOggOpusChecksum when a header page fails its CRC, and ErrOggOpusBadPage
// for malformed framing.
func NewOggOpusReader(r io.ReadSeeker) (*OggOpusReader, error) {
	if r == nil {
		return nil, errors.New("decode: nil Ogg Opus source")
	}
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("decode: measure Ogg Opus source: %w", err)
	}

	o := &OggOpusReader{src: r, fileSize: size, total: -1}
	if err := o.resetTo(0); err != nil {
		return nil, err
	}

	head, _, err := o.nextPacket()
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
	o.preSkip = info.preSkip
	o.gainQ78 = info.gain
	o.channels = info.channels

	// RFC 7845 section 3: the comment header is the second packet, may span
	// pages, and always finishes the page where it completes. It can be very
	// large (the reference file spends 280 pages on it), so it is skipped by
	// lacing rather than read; the body is never needed to play audio.
	audioStart, pending, pendingGranule, reset, err := o.skipCommentPacket()
	if err != nil {
		return nil, err
	}
	if err := o.buildIndex(audioStart); err != nil {
		return nil, err
	}
	if pending != nil {
		o.pending = pending
		o.pendingGranule = pendingGranule
	} else if reset {
		if err := o.resetTo(audioStart); err != nil {
			return nil, err
		}
	}

	return o, nil
}

// skipCommentPacket consumes or skips the second header packet and reports
// where audio begins.
//
// It returns the offset of the first audio page, the first audio packet when
// the second packet was not OpusTags (non-conformant but playable), and
// whether the reader's streaming state must be reset to that offset. When the
// comment header is skipped by lacing the body is never read, which keeps open
// proportional to page count rather than tag size.
func (o *OggOpusReader) skipCommentPacket() (audioStart int64, pending []byte, pendingGranule int64, reset bool, err error) {
	// A stream that ends after the ID header has no comment header and no
	// audio. Report EOF so the index stays empty and reads return io.EOF.
	if o.pageOffset >= o.fileSize {
		return o.fileSize, nil, 0, true, nil
	}

	// The comment header must start on the page after the ID header. When it
	// does not, fall back to the ordinary packet reader; those files are small.
	if o.pageLoaded && o.segIndex < len(o.page.lacing) {
		pkt, granule, perr := o.nextPacket()
		if perr != nil {
			if errors.Is(perr, io.EOF) {
				return o.fileSize, nil, 0, true, nil
			}

			return 0, nil, 0, false, perr
		}
		if bytes.HasPrefix(pkt, opusTagsTag) {
			return o.audioStartAfterComment(), nil, 0, false, nil
		}

		return o.packetStart, pkt, granule, false, nil
	}

	prefix, atPageEnd, continued, next, perr := o.peekPacketPrefix(o.pageOffset)
	if perr != nil {
		return 0, nil, 0, false, perr
	}
	if bytes.HasPrefix(prefix, opusTagsTag) && atPageEnd && !continued {
		return next, nil, 0, true, nil
	}

	// Either the second packet is audio, or the comment header is laid out in a
	// way this fast path does not cover. Read it the ordinary way.
	pkt, granule, rerr := o.nextPacket()
	if rerr != nil {
		if errors.Is(rerr, io.EOF) {
			return o.fileSize, nil, 0, true, nil
		}

		return 0, nil, 0, false, rerr
	}
	if bytes.HasPrefix(pkt, opusTagsTag) {
		return o.audioStartAfterComment(), nil, 0, false, nil
	}

	return o.packetStart, pkt, granule, false, nil
}

// audioStartAfterComment is the read point just past a consumed comment header.
func (o *OggOpusReader) audioStartAfterComment() int64 {
	if o.pageLoaded && o.segIndex < len(o.page.lacing) {
		return o.page.offset
	}

	return o.pageOffset
}

// peekPacketPrefix walks the packet starting at the beginning of the page at
// offset, reading only enough body bytes to identify it and using lacing alone
// to find where it completes. It returns the packet's leading bytes, whether
// the packet ended on the page's final lacing value, whether the first page
// carried the continued flag, and the offset of the page after it completes.
func (o *OggOpusReader) peekPacketPrefix(offset int64) (prefix []byte, atPageEnd, continuedFirst bool, next int64, err error) {
	const identifyLen = 8

	var header [oggHeaderLen]byte
	first := true
	for offset < o.fileSize {
		if _, err := o.src.Seek(offset, io.SeekStart); err != nil {
			return nil, false, false, 0, fmt.Errorf("decode: peek Ogg page at %d: %w", offset, err)
		}
		if _, err := io.ReadFull(o.src, header[:]); err != nil {
			return nil, false, false, 0, fmt.Errorf("%w: truncated page header at %d: %w", ErrOggOpusBadPage, offset, err)
		}
		if string(header[:4]) != oggCapture {
			return nil, false, false, 0, fmt.Errorf("%w: no Ogg capture pattern at %d", ErrOggOpusBadPage, offset)
		}

		segments := int(header[oggSegmentOff])
		lacing := make([]byte, segments)
		if _, err := io.ReadFull(o.src, lacing); err != nil {
			return nil, false, false, 0, fmt.Errorf("%w: truncated page lacing at %d: %w", ErrOggOpusBadPage, offset, err)
		}
		bodyLen := 0
		for _, l := range lacing {
			bodyLen += int(l)
		}

		if first {
			continuedFirst = header[5]&oggFlagContinued != 0
			n := min(identifyLen, bodyLen)
			prefix = make([]byte, n)
			if _, err := io.ReadFull(o.src, prefix); err != nil {
				return nil, false, false, 0, fmt.Errorf("%w: truncated page body at %d: %w", ErrOggOpusBadPage, offset, err)
			}
			first = false
		}

		for i, l := range lacing {
			if l < 255 {
				return prefix, i == segments-1, continuedFirst, offset + int64(oggHeaderLen+segments+bodyLen), nil
			}
		}
		offset += int64(oggHeaderLen + segments + bodyLen)
	}

	return nil, false, false, 0, io.ErrUnexpectedEOF
}

// oggOpusHeadInfo is the parsed ID header.
type oggOpusHeadInfo struct {
	preSkip  int
	gain     int16
	channels int
}

// oggOpusCRCTable is the Ogg page checksum (CRC-32, polynomial 0x04c11db7,
// no reflection, zero initial value) in table form.
var oggOpusCRCTable = func() [256]uint32 {
	const poly = 0x04c11db7

	var table [256]uint32
	for i := range table {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ poly
			} else {
				r <<= 1
			}
		}
		table[i] = r
	}

	return table
}()

// oggOpusChecksum computes the checksum over a page whose checksum field has
// been zeroed, matching the 32-bit field at offset 22.
func oggOpusChecksum(page []byte) uint32 {
	var crc uint32
	for _, b := range page {
		crc = crc<<8 ^ oggOpusCRCTable[byte(crc>>24)^b]
	}

	return crc
}

// parseOggOpusHead validates and decodes an OpusHead packet.
//
// Only mapping family 0 (RFC 7845 section 5.1.1.1) is accepted: mono or
// stereo, the case every normal encoder emits. Other families name multistream
// layouts this engine does not decode, so they are rejected with a clear error
// rather than reported with a channel count that would not hold.
func parseOggOpusHead(packet []byte) (oggOpusHeadInfo, error) {
	if len(packet) < oggOpusHeadMinLen {
		return oggOpusHeadInfo{}, fmt.Errorf("%w: OpusHead is %d bytes", ErrOggOpusNotOpus, len(packet))
	}
	if string(packet[:8]) != "OpusHead" {
		return oggOpusHeadInfo{}, ErrOggOpusNotOpus
	}
	// RFC 7845 section 5.1: the upper four version bits are the major version;
	// anything beyond 0 is not a compatible encapsulation.
	if version := packet[8]; version>>4 != 0 {
		return oggOpusHeadInfo{}, fmt.Errorf("decode: unsupported OpusHead version %d", version)
	}
	channels := int(packet[9])
	if channels == 0 {
		return oggOpusHeadInfo{}, errors.New("decode: OpusHead declares zero channels")
	}
	if family := packet[18]; family != 0 {
		return oggOpusHeadInfo{}, fmt.Errorf("%w: family %d", ErrOggOpusUnsupportedMapping, family)
	}
	if channels != 1 && channels != 2 {
		return oggOpusHeadInfo{}, fmt.Errorf("%w: family 0 with %d channels", ErrOggOpusUnsupportedMapping, channels)
	}

	return oggOpusHeadInfo{
		preSkip:  int(binary.LittleEndian.Uint16(packet[10:12])),
		gain:     int16(binary.LittleEndian.Uint16(packet[16:18])),
		channels: channels,
	}, nil
}

// resetTo clears the streaming state and seeks to offset.
func (o *OggOpusReader) resetTo(offset int64) error {
	if _, err := o.src.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("decode: seek Ogg Opus source to %d: %w", offset, err)
	}
	o.pageOffset = offset
	o.page = oggOpusPage{}
	o.pageLoaded = false
	o.segIndex = 0
	o.bodyIndex = 0
	o.partial = nil
	o.discarding = false
	o.pending = nil
	o.pendingGranule = 0
	o.packetStart = offset
	o.pos = 0

	return nil
}

// buildIndex walks page headers from start to EOF and records every page that
// completes at least one packet and carries a real granule.
//
// Bodies are skipped, so building the index stays proportional to page count
// rather than file size; CRC is verified only for pages actually read into the
// packet stream. A truncated tail stops the walk and leaves total at -1 rather
// than reporting the last complete page as the end.
func (o *OggOpusReader) buildIndex(start int64) error {
	var (
		offset      = start
		lastGranule int64
		lastPage    int64 = -1
		truncated   bool
		header      [oggHeaderLen]byte
	)
	for offset < o.fileSize {
		if _, err := o.src.Seek(offset, io.SeekStart); err != nil {
			return fmt.Errorf("decode: index Ogg Opus pages: %w", err)
		}
		if _, err := io.ReadFull(o.src, header[:]); err != nil {
			truncated = true

			break
		}
		if string(header[:4]) != oggCapture {
			truncated = true

			break
		}

		segments := int(header[oggSegmentOff])
		lacing := make([]byte, segments)
		if _, err := io.ReadFull(o.src, lacing); err != nil {
			truncated = true

			break
		}

		body := 0
		completed := false
		for _, l := range lacing {
			body += int(l)
			if l < 255 {
				completed = true
			}
		}
		end := offset + int64(oggHeaderLen+segments+body)
		if end > o.fileSize {
			truncated = true

			break
		}

		granule := int64(binary.LittleEndian.Uint64(header[oggGranuleOff : oggGranuleOff+oggGranuleSize]))
		lastPage = granule
		// A -1 granule means no packet completed here (RFC 7845 section 4), and
		// a granule without a completed packet is not a usable position.
		if completed && granule >= 0 {
			if granule < lastGranule {
				return fmt.Errorf("decode: Ogg Opus granule %d precedes %d", granule, lastGranule)
			}
			o.index = append(o.index, oggOpusPageEntry{offset: offset, granule: granule, start: lastGranule})
			lastGranule = granule
		}
		offset = end
	}
	if truncated || lastPage < 0 {
		o.total = -1
	} else {
		o.total = lastGranule
	}

	return nil
}

// loadNextPage reads and verifies the page at the current offset. It rejects a
// page that silently drops or misrepresents a continued packet.
func (o *OggOpusReader) loadNextPage() error {
	if o.pageOffset >= o.fileSize {
		return io.EOF
	}

	page, err := o.readPage(o.pageOffset)
	if err != nil {
		return err
	}

	if len(o.partial) > 0 && !page.continued() {
		return fmt.Errorf("%w: page at %d drops a continued packet", ErrOggOpusBadPage, page.offset)
	}

	o.page = page
	o.pageOffset = page.end
	o.pageLoaded = true
	o.segIndex = 0
	o.bodyIndex = 0
	// A continued first packet with no buffered prefix is data we joined
	// mid-packet: RFC 7845 section 3 forbids decoding it, so skip to the first
	// packet boundary. This is the post-seek case. The skip lasts until the
	// skipped packet completes; a page that starts fresh ends it.
	o.discarding = page.continued() && len(o.partial) == 0

	return nil
}

// readPage reads the complete page at offset and verifies its CRC.
func (o *OggOpusReader) readPage(offset int64) (oggOpusPage, error) {
	if _, err := o.src.Seek(offset, io.SeekStart); err != nil {
		return oggOpusPage{}, fmt.Errorf("decode: seek to Ogg page %d: %w", offset, err)
	}

	raw := make([]byte, oggHeaderLen)
	if _, err := io.ReadFull(o.src, raw); err != nil {
		return oggOpusPage{}, fmt.Errorf("%w: truncated page header at %d: %w", ErrOggOpusBadPage, offset, err)
	}
	if string(raw[:4]) != oggCapture {
		return oggOpusPage{}, fmt.Errorf("%w: no Ogg capture pattern at %d", ErrOggOpusBadPage, offset)
	}

	segments := int(raw[oggSegmentOff])
	lacingOff := len(raw)
	raw = append(raw, make([]byte, segments)...)
	if _, err := io.ReadFull(o.src, raw[lacingOff:]); err != nil {
		return oggOpusPage{}, fmt.Errorf("%w: truncated page lacing at %d: %w", ErrOggOpusBadPage, offset, err)
	}

	body := 0
	for _, l := range raw[lacingOff:] {
		body += int(l)
	}
	bodyOff := len(raw)
	raw = append(raw, make([]byte, body)...)
	if _, err := io.ReadFull(o.src, raw[bodyOff:]); err != nil {
		return oggOpusPage{}, fmt.Errorf("%w: truncated page body at %d: %w", ErrOggOpusBadPage, offset, err)
	}

	stored := binary.LittleEndian.Uint32(raw[22:26])
	binary.LittleEndian.PutUint32(raw[22:26], 0)
	if got := oggOpusChecksum(raw); got != stored {
		return oggOpusPage{}, fmt.Errorf("%w: page at %d has %08x, want %08x", ErrOggOpusChecksum, offset, got, stored)
	}

	return oggOpusPage{
		offset:  offset,
		end:     offset + int64(len(raw)),
		flags:   raw[5],
		granule: int64(binary.LittleEndian.Uint64(raw[oggGranuleOff : oggGranuleOff+oggGranuleSize])),
		lacing:  raw[lacingOff : lacingOff+segments],
		body:    raw[bodyOff:],
	}, nil
}

// nextPacket returns the next packet, reassembling it across lacing values and
// pages. The granule is the one on the page the packet completed on, or -1
// when that page carries none.
func (o *OggOpusReader) nextPacket() ([]byte, int64, error) {
	if o.pending != nil {
		packet, granule := o.pending, o.pendingGranule
		o.pending = nil
		o.pendingGranule = 0

		return packet, granule, nil
	}

	for {
		if !o.pageLoaded {
			if err := o.loadNextPage(); err != nil {
				if errors.Is(err, io.EOF) && len(o.partial) > 0 {
					return nil, -1, io.ErrUnexpectedEOF
				}

				return nil, -1, err
			}
		}

		for o.segIndex < len(o.page.lacing) {
			size := int(o.page.lacing[o.segIndex])
			o.segIndex++
			segment := o.page.body[o.bodyIndex : o.bodyIndex+size]
			o.bodyIndex += size

			if o.discarding {
				if size < 255 {
					o.discarding = false
				}

				continue
			}
			if len(o.partial) == 0 {
				o.packetStart = o.page.offset
			}
			o.partial = append(o.partial, segment...)
			if size == 255 {
				continue
			}

			packet := o.partial
			o.partial = nil

			return packet, o.page.granule, nil
		}
		// The page is exhausted. Packet boundaries are contiguous, so the next
		// packet starts at this page's granule; a -1 page has none and leaves
		// the last known bound in place.
		if o.page.granule >= 0 {
			o.pos = o.page.granule
		}
		o.pageLoaded = false
	}
}

// PreSkip is the number of decoded samples to discard from the start, and the
// amount subtracted from a granule to get a PCM sample position (RFC 7845
// section 4.2).
func (o *OggOpusReader) PreSkip() int { return o.preSkip }

// OutputGainQ78 is the signed Q7.8 dB output gain from the ID header (RFC 7845
// section 5.1). Apply it as 10^(gain/(20*256)).
func (o *OggOpusReader) OutputGainQ78() int16 { return o.gainQ78 }

// Channels is the output channel count declared by the ID header,
// 1 or 2.
func (o *OggOpusReader) Channels() int { return o.channels }

// SampleRate is always 48000: Opus granule positions and decoding are defined
// at 48 kHz regardless of the input rate recorded in the header.
func (o *OggOpusReader) SampleRate() int { return oggOpusSampleRate }

// TotalGranule is the granule of the final audio page, or -1 when the stream
// is truncated and the true end could not be established.
func (o *OggOpusReader) TotalGranule() int64 { return o.total }

// ReadPacket returns the next audio packet and the granule position of the page
// it completed on. The granule is -1 when that page has no usable position. The
// returned slice is owned by the caller.
//
// A continued packet whose start is missing (a seek that landed mid-packet) is
// skipped rather than returned, per RFC 7845 section 3.
func (o *OggOpusReader) ReadPacket() (packet []byte, granule int64, err error) {
	return o.nextPacket()
}

// SeekGranule repositions the reader so the next packet read begins at or
// before target, backing off by up to preroll samples so a decoder can warm up
// its inter-frame state (RFC 7845 section 4.6 recommends 3840, 80 ms).
//
// The page chosen is the last indexed page whose granule is at or before
// target-preroll, or the first audio page when the target is earlier. The
// chosen page is never past target, so a caller can always discard forward to
// the exact frame.
//
// Errors: a negative target, a target past the last known granule, or a stream
// with no indexed audio pages.
func (o *OggOpusReader) SeekGranule(target int64, preroll int64) error {
	if target < 0 {
		return fmt.Errorf("decode: negative granule seek target %d", target)
	}
	if len(o.index) == 0 {
		return errors.New("decode: Ogg Opus stream has no seekable audio pages")
	}
	last := o.index[len(o.index)-1].granule
	if target > last {
		return fmt.Errorf("decode: granule seek target %d is past end %d", target, last)
	}

	want := target - preroll
	if want < 0 {
		want = 0
	}
	// Last page with granule <= want; the index is sorted by granule.
	i := sort.Search(len(o.index), func(i int) bool { return o.index[i].granule > want }) - 1
	if i < 0 {
		i = 0
	}
	entry := o.index[i]

	if err := o.resetTo(entry.offset); err != nil {
		return err
	}
	o.pos = entry.start

	return nil
}

// Position is the granule position at which the next packet read starts, as a
// lower bound. It is exact at page boundaries and never exceeds the true value,
// so a caller can use it as the point from which to discard forward.
func (o *OggOpusReader) Position() int64 { return o.pos }
