// Native WebM/Matroska Opus container reader with Cue-index seeks.
//
// Purpose:
//   Read Opus audio packets out of a WebM/Matroska bitstream (RFC 8794 framing,
//   Matroska codec mapping for A_OPUS) without decoding, and reposition to a
//   granule position through the file's Cues index. It is the WebM counterpart
//   of oggopus.go: the same opusPacketSource surface, so pionOpusDecoder is
//   reused unchanged and only the container differs.
//
// Key Components:
//   - newWebMOpusReader(): parses EBML, Info, Tracks and Cues, indexes clusters
//   - webMOpusReader.ReadPacket(): one Opus packet from a SimpleBlock or Block
//   - webMOpusReader.SeekGranule(): Cues lookup, falling back to a cluster scan
//   - forwardWebMOpus: the forward-only variant for a non-seekable stream
//   - WebMOpusFactory: registry entry, probe, and decoding from a path or reader
//
// Position domain:
//   opusPacketSource counts at 48 kHz and includes the pre-skip, so a stream's
//   playable length is TotalGranule - PreSkip. WebM has no granule; it has block
//   timestamps in segment ticks. This reader adopts the Ogg convention so the
//   decoder's arithmetic keeps working:
//
//     PreSkip()      = CodecDelay * 48000 / 1e9
//     TotalGranule() = Duration * 48000 / 1e9      (Info, already spans the pre-skip)
//
//   A position is the running sum of decoded packet durations, not a block
//   timestamp. Matroska timestamps sit on a millisecond grid and can disagree
//   with the true packet lengths by a sample or two per block; the decoder's
//   seek skip sums packet durations, so mixing the two domains would shift every
//   seek by the accumulated difference. The reader therefore builds its cluster
//   index in the decoded domain: each cluster records the running sample sum at
//   its first packet, and a seek targets that. Matroska's CodecDelay is the
//   reason the first block is stamped zero rather than the pre-skip: that
//   leading audio is the encoder's warm-up and the decoder discards it.
//
// Offset domains:
//   CueClusterPosition and SeekPosition are relative to the start of the
//   Segment's data, not the file (Matroska Cues). Every offset read from an
//   index element is shifted by segmentDataOff before use; mixing the two
//   domains is the classic Matroska seek bug.
//
// Ownership:
//   The reader never owns, closes, or spawns goroutines around the source it is
//   given. The caller keeps that responsibility.

package decode

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/dlcuy22/molo/core"
)

// Matroska/WebM element IDs, raw (marker bit kept, RFC 8794 section 5).
const (
	idEBML           = 0x1A45DFA3
	idDocType        = 0x4282
	idEBMLMaxIDLen   = 0x42F2
	idSegment        = 0x18538067
	idSeekHead       = 0x114D9B74
	idInfo           = 0x1549A966
	idTimestampScale = 0x2AD7B1
	idDuration       = 0x4489
	idTracks         = 0x1654AE6B
	idTrackEntry     = 0xAE
	idTrackNumber    = 0xD7
	idTrackType      = 0x83 // 2 = audio
	idCodecID        = 0x86 // "A_OPUS"
	idCodecPrivate   = 0x63A2
	idCodecDelay     = 0x56AA
	idSeekPreRoll    = 0x56BB
	idContentEnc     = 0x6D80
	idCluster        = 0x1F43B675
	idTimestamp      = 0xE7
	idSimpleBlock    = 0xA3
	idBlockGroup     = 0xA0
	idBlock          = 0xA1
	idDiscardPadding = 0x75A2
	idCues           = 0x1C53BB6B
	idCuePoint       = 0xBB
	idCueTime        = 0xB3
	idCueTrackPos    = 0xB7
	idCueClusterPos  = 0xF1
	idCueBlockNum    = 0xF0
)

// webmOpusSampleRate is fixed like the Ogg path: Opus decoding and pre-skip are
// defined at 48 kHz regardless of the rate recorded in the header.
const webmOpusSampleRate = 48000

// webmDefaultTimestampScale is 1 ms per tick, the Matroska default and the only
// value WebM permits; a file that omits TimestampScale is read with it.
const webmDefaultTimestampScale = 1_000_000

// webmTrackTypeAudio is TrackType 2 (Matroska Codec Mapping).
const webmTrackTypeAudio = 2

// Errors returned by the reader. Callers can test them with errors.Is.
var (
	errWebMOpusNotWebM          = errors.New("decode: not a WebM/Matroska stream")
	errWebMOpusNotOpus          = errors.New("decode: no A_OPUS audio track")
	errWebMOpusMissingCodecPriv = errors.New("decode: A_OPUS track has no CodecPrivate")
	errWebMOpusEncrypted        = errors.New("decode: WebM ContentEncodings are unsupported")
	errWebMOpusNoClusters       = errors.New("decode: WebM stream has no clusters")
)

// level1IDs are the element IDs that may appear directly under Segment. An
// unknown-size master ends where the next of these begins, which is how a
// streaming Segment or Cluster is terminated.
var level1IDs = map[uint32]bool{
	idSeekHead: true,
	idInfo:     true,
	idTracks:   true,
	idCues:     true,
	idCluster:  true,
	0x1043A770: true, // Chapters
	0x1941A469: true, // Attachments
	0x1254C367: true, // Tags
}

// webmCue is one Cues entry. clusterOff is absolute: CueClusterPosition plus
// segmentDataOff. timeNs is CueTime converted from segment ticks.
type webmCue struct {
	timeNs      int64
	clusterOff  int64
	blockNumber int
}

// webmCluster is one Cluster's position and time. end is the exclusive offset
// just past the element. startGranule is the decoded-domain granule at which the
// cluster's first packet begins: the running sum of packet durations from the
// stream start, which is the domain opusPacketSource counts in. It is built at
// open because Matroska block timestamps are on a millisecond grid and can
// differ from the true packet durations, so a seek must target the decoded
// domain the decoder actually advances in.
type webmCluster struct {
	offset       int64 // absolute element offset
	dataOff      int64 // absolute offset of the first child
	end          int64
	timeNs       int64
	startGranule int64
}

// webMOpusReader reads Opus packets from a WebM/Matroska stream and can seek
// with the file's Cues index. It satisfies opusPacketSource. It is unexported
// because it is engine plumbing: only the Decoder contract is public, and the
// reader exists to feed it. It is not safe for concurrent use.
type webMOpusReader struct {
	src      io.ReaderAt
	fileSize int64

	// segmentDataOff is the start of the Segment's data, the base of every
	// Cues and SeekHead offset. segmentEnd is the end of the Segment.
	segmentDataOff int64
	segmentEnd     int64

	timestampScale uint64

	preSkip     int
	gainQ78     int16
	channels    int
	seekPreRoll int // 48 kHz samples, from SeekPreRoll

	trackNumber uint64

	// total is the granule at the end of the last playable sample, or -1 when
	// the duration is unknown. It already includes the pre-skip so the
	// decoder's TotalGranule - PreSkip stays the playable frame count.
	total         int64
	hasDuration   bool
	durationTicks float64
	// runningGranule accumulates decoded-domain samples while buildIndex walks
	// the clusters, so each cluster records where it starts.
	runningGranule int64

	cues     []webmCue
	clusters []webmCluster

	// Read state. The current block is buffered whole; blocks are small and
	// this avoids re-reading a header to find where its packets begin.
	clusterIdx    int
	childOff      int64 // next child to examine in the current cluster
	clusterEnd    int64
	block         webmBlock
	blockLoaded   bool
	blockPacket   int
	blockGran     int64 // decoded-domain granule of the current block's first packet
	nextBlockGran int64 // decoded-domain granule where the next block starts
	clusterBaseNs int64 // timestamp of the current cluster

	pos      int64
	posExact bool
}

// webmBlock is one parsed Block or SimpleBlock payload.
type webmBlock struct {
	track    uint64
	timeNs   int64 // absolute presentation timestamp in nanoseconds
	keyframe bool
	packets  [][]byte
	// discardPad is the signed DiscardPadding of the enclosing BlockGroup, in
	// nanoseconds. It is trimmed from the last packet's duration.
	discardPad int64
}

// newWebMOpusReader parses the stream headers and indexes its clusters.
//
// The EBML header is validated (DocType "webm"), Info supplies the timestamp
// scale and duration, Tracks must hold an A_OPUS audio track with a valid
// OpusHead CodecPrivate, and Cues is used when present. The returned reader is
// positioned at the first audio packet. The input is read but not owned: the
// caller closes it.
func newWebMOpusReader(r io.ReadSeeker) (*webMOpusReader, error) {
	if r == nil {
		return nil, errors.New("decode: nil WebM Opus source")
	}
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("decode: measure WebM Opus source: %w", err)
	}

	w := &webMOpusReader{src: readerAtOf(r), fileSize: size, total: -1, posExact: true}
	if err := w.parseHeaders(); err != nil {
		return nil, err
	}
	if err := w.buildIndex(); err != nil {
		return nil, err
	}

	return w, nil
}

// readerAtOf adapts a seekable reader to io.ReaderAt. A reader that is already
// an io.ReaderAt (an *os.File, a bytes.Reader) is used directly so a seek does
// not thrash its buffer.
func readerAtOf(r io.ReadSeeker) io.ReaderAt {
	if ra, ok := r.(io.ReaderAt); ok {
		return ra
	}

	return seekReaderAt{r}
}

type seekReaderAt struct{ rs io.ReadSeeker }

func (s seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}

	return io.ReadFull(s.rs, p)
}

// headerAt reads one element header at off and resolves its absolute extent.
func (w *webMOpusReader) headerAt(off int64) (ebmlElement, error) {
	var buf [ebmlMaxIDLen + ebmlMaxSizeLen]byte
	n, err := w.src.ReadAt(buf[:], off)
	if err != nil && !errors.Is(err, io.EOF) {
		return ebmlElement{}, fmt.Errorf("decode: read element header at %d: %w", off, err)
	}
	if n == 0 {
		return ebmlElement{}, fmt.Errorf("%w: no element header at %d", errEBMLTruncated, off)
	}

	e, err := readEBMLHeader(buf[:n], 0)
	if err != nil {
		return ebmlElement{}, err
	}
	e.DataOff += off
	if !e.Unknown {
		e.Next += off
	}

	return e, nil
}

// readBytes reads size bytes at off, bounded by the file's extent. Every
// allocation in this reader goes through here so an element size from the file
// can never exceed the bytes that actually exist.
func (w *webMOpusReader) readBytes(off, size int64) ([]byte, error) {
	return readFullAt(w.src, off, size, w.fileSize)
}

// parseHeaders reads EBML, Info and Tracks, and validates the audio track. It
// stops before the clusters; buildIndex walks those.
func (w *webMOpusReader) parseHeaders() error {
	ebml, err := w.headerAt(0)
	if err != nil {
		return err
	}
	if ebml.ID != idEBML {
		return fmt.Errorf("%w: leading element is %#x, not EBML", errWebMOpusNotWebM, ebml.ID)
	}
	if err := w.parseEBMLHeader(ebml); err != nil {
		return err
	}

	seg, err := w.headerAt(ebml.Next)
	if err != nil {
		return err
	}
	if seg.ID != idSegment {
		return fmt.Errorf("%w: element after EBML is %#x, not Segment", errWebMOpusNotWebM, seg.ID)
	}
	w.segmentDataOff = seg.DataOff
	if seg.Unknown {
		w.segmentEnd = w.fileSize
	} else {
		w.segmentEnd = seg.Next
	}

	// Walk Segment's level-1 children up to the clusters. Info and Tracks
	// normally appear before the first Cluster; Cues may be before or after it,
	// so buildIndex reads it wherever it is. SeekHead is skipped: it is a
	// guessed index and this reader walks the tree itself.
	for off := seg.DataOff; off < w.segmentEnd; {
		e, err := w.headerAt(off)
		if err != nil {
			return err
		}
		if e.Unknown {
			e.Next = w.segmentEnd
		}

		switch e.ID {
		case idInfo:
			if err := w.parseInfo(e); err != nil {
				return err
			}
		case idTracks:
			if err := w.parseTracks(e); err != nil {
				return err
			}
		}

		if e.Next <= off {
			return fmt.Errorf("%w: level-1 element %#x at %d did not advance", errEBMLBadVint, e.ID, off)
		}
		off = e.Next
		if e.ID == idCluster {
			// Clusters start here; the rest is buildIndex's job.
			break
		}
	}

	if w.timestampScale == 0 {
		w.timestampScale = webmDefaultTimestampScale
	}
	if w.trackNumber == 0 {
		return fmt.Errorf("%w: no audio track entry in Tracks", errWebMOpusNotOpus)
	}

	return nil
}

// parseEBMLHeader checks the document is WebM and the ID length bound is one
// this reader can honour.
func (w *webMOpusReader) parseEBMLHeader(e ebmlElement) error {
	if e.Unknown || e.DataOff+int64(e.Size) > int64(w.fileSize) {
		return fmt.Errorf("%w: EBML header does not fit the file", errEBMLTruncated)
	}
	body, err := w.readBytes(e.DataOff, int64(e.Size))
	if err != nil {
		return err
	}

	docType := ""
	maxID := uint64(0)
	err = ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		switch c.ID {
		case idDocType:
			docType = string(body[c.DataOff : c.DataOff+int64(c.Size)])
		case idEBMLMaxIDLen:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			maxID = v
		}

		return nil
	})
	if err != nil {
		return err
	}
	if docType != "webm" {
		return fmt.Errorf("%w: DocType is %q", errWebMOpusNotWebM, docType)
	}
	if maxID > ebmlMaxIDLen {
		return fmt.Errorf("%w: EBMLMaxIDLength %d exceeds %d", errWebMOpusNotWebM, maxID, ebmlMaxIDLen)
	}

	return nil
}

// parseInfo reads TimestampScale and Duration. Duration is a float in segment
// ticks (RFC 8794 section 7.2), which is why it is not read as an integer.
func (w *webMOpusReader) parseInfo(e ebmlElement) error {
	if e.Unknown || e.DataOff+int64(e.Size) > int64(w.fileSize) {
		return fmt.Errorf("%w: Info does not fit the file", errEBMLTruncated)
	}
	body, err := w.readBytes(e.DataOff, int64(e.Size))
	if err != nil {
		return err
	}

	var durationTicks float64
	found := false
	err = ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		switch c.ID {
		case idTimestampScale:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			w.timestampScale = v
		case idDuration:
			v, err := ebmlFloat(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			durationTicks = v
			found = true
		}

		return nil
	})
	if err != nil {
		return err
	}

	if found && durationTicks > 0 {
		w.hasDuration = true
		w.durationTicks = durationTicks
	}

	return nil
}

// durationToGranule converts a Duration in segment ticks to the 48 kHz granule
// at the end of the playable audio. Duration is in seconds after the scale is
// applied and already spans the pre-skip, so it is the granule domain directly;
// the playable frame count is this minus PreSkip.
func (w *webMOpusReader) durationToGranule(ticks float64) int64 {
	seconds := ticks * float64(w.timestampScale) / 1e9

	return int64(math.Round(seconds * webmOpusSampleRate))
}

// parseTracks finds the A_OPUS audio TrackEntry and reads its codec fields.
func (w *webMOpusReader) parseTracks(e ebmlElement) error {
	if e.Unknown || e.DataOff+int64(e.Size) > int64(w.fileSize) {
		return fmt.Errorf("%w: Tracks does not fit the file", errEBMLTruncated)
	}
	body, err := w.readBytes(e.DataOff, int64(e.Size))
	if err != nil {
		return err
	}

	return ebmlChildren(body, 0, int64(len(body)), func(track ebmlElement) error {
		if track.ID != idTrackEntry || track.Unknown {
			return nil
		}
		return w.parseTrackEntry(body[track.DataOff:track.Next])
	})
}

// parseTrackEntry reads one TrackEntry and, when it is the Opus audio track,
// adopts its fields. A non-audio or non-Opus entry is ignored so a muxed file
// with a video track still plays its audio. CodecDelay is authoritative for the
// pre-skip; the OpusHead pre_skip is the fallback when CodecDelay is absent.
func (w *webMOpusReader) parseTrackEntry(body []byte) error {
	return webmTrackParser{}.parse(body, func(fields webmTrackFields) {
		if w.trackNumber != 0 {
			// First audio track wins; a second one is not this reader's
			// problem.
			return
		}
		w.trackNumber = fields.trackNumber
		w.channels = fields.channels
		w.gainQ78 = fields.gain
		w.preSkip = fields.preSkip
		w.seekPreRoll = fields.seekPreRoll
	})
}

// buildIndex walks every level-1 child from the start of the Segment: it
// records the cluster positions and timestamps, reads Cues if present, and
// derives the total when Duration was absent. Only element headers and the
// small first child of each cluster are read, so the walk stays proportional to
// the cluster count, not the file size.
func (w *webMOpusReader) buildIndex() error {
	for off := w.segmentDataOff; off < w.segmentEnd; {
		e, err := w.headerAt(off)
		if err != nil {
			return err
		}
		if e.ID == ebmlIDVoid || e.ID == ebmlIDCRC32 {
			// Void and CRC-32 are skippable, but both require a known size:
			// their extent cannot be derived from the next level-1 element.
			// Treating an unknown-size one as zero-length would silently drop
			// everything that follows.
			if e.Unknown {
				return fmt.Errorf("%w: unknown-size %#x at %d", errEBMLTruncated, e.ID, off)
			}
			if e.Next <= off {
				return fmt.Errorf("%w: %#x at %d did not advance", errEBMLBadVint, e.ID, off)
			}
			off = e.Next

			continue
		}
		if e.Unknown {
			// An unknown-size level-1 element ends where the next level-1
			// element begins; find it by scanning children.
			end, scanErr := w.scanUnknownMasterEnd(e.DataOff, w.segmentEnd)
			if scanErr != nil {
				return scanErr
			}
			e.Next = end
		}

		switch e.ID {
		case idCues:
			if err := w.parseCues(e); err != nil {
				return err
			}
		case idCluster:
			cl := webmCluster{offset: off, dataOff: e.DataOff, end: e.Next}
			if err := w.readClusterTime(&cl); err != nil {
				return err
			}
			// The decoded-domain start of this cluster is the running sum of
			// every packet duration before it. Reading each block's TOC here is
			// what makes a seek land in the same domain the decoder advances in;
			// block timestamps alone are on a millisecond grid and drift.
			cl.startGranule = w.runningGranule
			durations, err := w.clusterBlockSamples(cl)
			if err != nil {
				return err
			}
			w.runningGranule += durations
			w.clusters = append(w.clusters, cl)
		}

		if e.Next <= off {
			return fmt.Errorf("%w: element %#x at %d did not advance", errEBMLBadVint, e.ID, off)
		}
		off = e.Next
	}

	if len(w.clusters) == 0 {
		return errWebMOpusNoClusters
	}
	sort.SliceStable(w.cues, func(i, j int) bool { return w.cues[i].timeNs < w.cues[j].timeNs })

	// The total is resolved here rather than in parseInfo because Info precedes
	// Tracks, so the pre-skip is only known once the whole header is parsed.
	// Matroska's Duration already covers the pre-skip: the block timeline starts
	// at the first packet (timestamp 0), and the first preSkip samples are the
	// encoder's warm-up inside the container, so the playable frame count is
	// Duration*samples - preSkip, matching TotalGranule - PreSkip.
	if w.hasDuration {
		w.total = w.durationToGranule(w.durationTicks)
	} else {
		total, err := w.scanTotal()
		if err != nil {
			return err
		}
		w.total = total
	}
	if w.seekPreRoll == 0 {
		w.seekPreRoll = pionWarmupFast
	}
	if err := w.resetToBlock(w.clusters[0]); err != nil {
		return err
	}

	return nil
}

// readClusterTime reads a cluster's leading Timestamp so an index can be built
// without touching its blocks.
func (w *webMOpusReader) readClusterTime(cl *webmCluster) error {
	off := cl.dataOff
	for off < cl.end {
		e, err := w.headerAt(off)
		if err != nil {
			return err
		}
		if e.Unknown {
			e.Next = cl.end
		}
		if e.ID == idTimestamp {
			v, err := w.readBytes(e.DataOff, int64(e.Size))
			if err != nil {
				return err
			}
			ticks, err := ebmlUint(v, 0, int64(len(v)))
			if err != nil {
				return err
			}
			cl.timeNs = int64(ticks) * int64(w.timestampScale)
		}
		if e.ID == idSimpleBlock || e.ID == idBlockGroup {
			// The Timestamp always precedes the blocks; once one is seen the
			// cluster time is settled.
			break
		}
		if e.Next <= off {
			break
		}
		off = e.Next
	}

	return nil
}

// scanUnknownMasterEnd walks the children of an unknown-size master and returns
// the offset of the first level-1 element after its data, or limit.
func (w *webMOpusReader) scanUnknownMasterEnd(start, limit int64) (int64, error) {
	off := start
	for off < limit {
		e, err := w.headerAt(off)
		if err != nil {
			return 0, err
		}
		if level1IDs[e.ID] {
			return off, nil
		}
		if e.Unknown {
			// A nested unknown master makes the boundary ambiguous; treat the
			// rest of the parent as this element and stop.
			return limit, nil
		}
		if e.Next <= off {
			return 0, fmt.Errorf("%w: element %#x at %d did not advance", errEBMLBadVint, e.ID, off)
		}
		off = e.Next
	}

	return limit, nil
}

// parseCues reads every CuePoint and records the cluster offsets, shifted into
// the file's offset domain. CueBlockNumber is optional and only a hint.
func (w *webMOpusReader) parseCues(e ebmlElement) error {
	if e.Unknown || e.DataOff+int64(e.Size) > int64(w.fileSize) {
		return fmt.Errorf("%w: Cues does not fit the file", errEBMLTruncated)
	}
	body, err := w.readBytes(e.DataOff, int64(e.Size))
	if err != nil {
		return err
	}

	return ebmlChildren(body, 0, int64(len(body)), func(point ebmlElement) error {
		if point.ID != idCuePoint || point.Unknown {
			return nil
		}
		return w.parseCuePoint(body[point.DataOff:point.Next])
	})
}

// parseCuePoint reads CueTime and the first CueTrackPositions' cluster offset.
func (w *webMOpusReader) parseCuePoint(body []byte) error {
	cue := webmCue{blockNumber: -1}
	err := ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		switch c.ID {
		case idCueTime:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			cue.timeNs = int64(v) * int64(w.timestampScale)
		case idCueTrackPos:
			if c.Unknown {
				return nil
			}
			return w.parseCueTrackPos(body[c.DataOff:c.Next], &cue)
		}

		return nil
	})
	if err != nil {
		return err
	}

	w.cues = append(w.cues, cue)

	return nil
}

func (w *webMOpusReader) parseCueTrackPos(body []byte, cue *webmCue) error {
	return ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		switch c.ID {
		case idCueClusterPos:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			// CueClusterPosition is relative to the Segment's data, not the
			// file. This add is the whole point of tracking segmentDataOff.
			cue.clusterOff = w.segmentDataOff + int64(v)
		case idCueBlockNum:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			cue.blockNumber = int(v)
		}

		return nil
	})
}

// scanTotal derives the total granule from the cluster index, used when Info
// carries no Duration (a live or streaming file). The last cluster's decoded
// end is the best available end-of-stream position. A failure leaves the total
// at -1, exactly like Ogg's unreadable tail.
func (w *webMOpusReader) scanTotal() (int64, error) {
	last := w.clusters[len(w.clusters)-1]

	end, err := w.lastBlockEndGranule(last)
	if err != nil {
		return -1, nil
	}

	return end, nil
}

// clusterBlockSamples returns the total decoded samples of the audio blocks in
// one cluster, by reading each block's payload and summing its packets' TOC
// durations. It is the running-sum step that turns the block-timestamp index
// into the decoded-domain index a seek needs.
func (w *webMOpusReader) clusterBlockSamples(cl webmCluster) (int64, error) {
	var (
		off   = cl.dataOff
		total int64
	)
	w.clusterBaseNs = cl.timeNs
	for off < cl.end {
		e, err := w.headerAt(off)
		if err != nil {
			return 0, err
		}
		if e.Unknown {
			e.Next = cl.end
		}
		if e.ID == idSimpleBlock || e.ID == idBlockGroup {
			blk, err := w.readBlock(e)
			if err != nil {
				return 0, err
			}
			if blk.track == w.trackNumber {
				total += int64(packetTotalSamples(blk))
			}
		}
		if e.Next <= off {
			break
		}
		off = e.Next
	}

	return total, nil
}

// lastBlockEndGranule returns the decoded-domain granule at the end of the
// cluster's last audio block, from the running sum the index already holds plus
// that cluster's own block durations. It is used for the total when Info has no
// Duration.
func (w *webMOpusReader) lastBlockEndGranule(cl webmCluster) (int64, error) {
	durations, err := w.clusterBlockSamples(cl)
	if err != nil {
		return -1, err
	}

	return cl.startGranule + durations, nil
}

// packetTotalSamples sums the Opus duration of every packet in a block, adjusted
// by the block's DiscardPadding. Matroska allows a negative padding, which means
// the block adds silence at the beginning rather than trimming the end, so a
// negative value lengthens the block instead of being ignored.
func packetTotalSamples(blk webmBlock) int {
	total := 0
	for _, p := range blk.packets {
		if n := opusPacketSamples48(p); n > 0 {
			total += n
		}
	}
	if blk.discardPad == 0 {
		return total
	}
	padNs := blk.discardPad
	if padNs < 0 {
		padNs = -padNs
	}
	samples := int((padNs*webmOpusSampleRate + 500_000_000) / 1_000_000_000)
	if blk.discardPad > 0 {
		if samples > total {
			samples = total
		}

		return total - samples
	}

	return total + samples
}

// readBlock reads and parses the element at e: a SimpleBlock directly, or a
// BlockGroup whose Block and DiscardPadding are read together.
func (w *webMOpusReader) readBlock(e ebmlElement) (webmBlock, error) {
	if e.ID == idSimpleBlock {
		if e.Unknown {
			return webmBlock{}, fmt.Errorf("%w: unknown-size SimpleBlock", errEBMLTruncated)
		}
		body, err := w.readBytes(e.DataOff, int64(e.Size))
		if err != nil {
			return webmBlock{}, err
		}

		return parseWebMBlock(body, w.timestampScale, w.clusterBaseNs)
	}

	// BlockGroup. Its size is required; unknown-size here is malformed.
	if e.Unknown || e.DataOff+int64(e.Size) > int64(w.fileSize) {
		return webmBlock{}, fmt.Errorf("%w: BlockGroup does not fit the file", errEBMLTruncated)
	}
	body, err := w.readBytes(e.DataOff, int64(e.Size))
	if err != nil {
		return webmBlock{}, err
	}

	var (
		blk webmBlock
		pad int64
	)
	err = ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		switch c.ID {
		case idBlock:
			b, err := parseWebMBlock(body[c.DataOff:c.Next], w.timestampScale, w.clusterBaseNs)
			if err != nil {
				return err
			}
			blk = b
		case idDiscardPadding:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			pad = signedEBMLInt(v, int(c.Size))
		}

		return nil
	})
	if err != nil {
		return webmBlock{}, err
	}
	blk.discardPad = pad

	return blk, nil
}

// resetToBlock positions the reader at a cluster and loads its first block so
// Position is exact from the first packet. The granule comes from the cluster's
// decoded-domain index, not its timestamp, so it matches the domain the decoder
// advances in.
func (w *webMOpusReader) resetToBlock(cl webmCluster) error {
	// Find the cluster's index so forward reads can advance to the next one.
	idx := sort.Search(len(w.clusters), func(i int) bool { return w.clusters[i].offset >= cl.offset })
	if idx >= len(w.clusters) || w.clusters[idx].offset != cl.offset {
		idx = 0
	}
	w.clusterIdx = idx
	w.childOff = cl.dataOff
	w.clusterEnd = cl.end
	w.clusterBaseNs = cl.timeNs
	w.blockLoaded = false
	w.block = webmBlock{}
	w.blockPacket = 0
	w.blockGran = cl.startGranule
	w.nextBlockGran = cl.startGranule
	w.pos = cl.startGranule
	w.posExact = true

	// Peek the first block so Position names the first packet's real granule.
	return w.loadBlock()
}

// loadBlock advances the read cursor to the next SimpleBlock or BlockGroup of
// the audio track and buffers it. It is called directly by resetToBlock so a
// seek leaves the reader ready with an exact position.
func (w *webMOpusReader) loadBlock() error {
	for {
		if w.childOff >= w.clusterEnd {
			if !w.nextCluster() {
				return io.EOF
			}

			continue
		}

		e, err := w.headerAt(w.childOff)
		if err != nil {
			return err
		}
		if e.Unknown {
			e.Next = w.clusterEnd
		}
		if e.Next <= w.childOff {
			return fmt.Errorf("%w: cluster child %#x did not advance", errEBMLBadVint, e.ID)
		}
		w.childOff = e.Next

		if e.ID != idSimpleBlock && e.ID != idBlockGroup {
			continue
		}
		blk, err := w.readBlock(e)
		if err != nil {
			return err
		}
		if blk.track != w.trackNumber || len(blk.packets) == 0 {
			continue
		}

		w.block = blk
		w.blockLoaded = true
		w.blockPacket = 0
		// The block starts where the previous block ended, in the decoded
		// domain; the block timestamp is only a hint and is not used here.
		w.blockGran = w.nextBlockGran
		w.pos = w.blockGran

		return nil
	}
}

// nextCluster advances to the following cluster in the index, reporting false
// at the end of the stream.
func (w *webMOpusReader) nextCluster() bool {
	if w.clusterIdx+1 >= len(w.clusters) {
		return false
	}
	w.clusterIdx++
	cl := w.clusters[w.clusterIdx]
	w.childOff = cl.dataOff
	w.clusterEnd = cl.end
	w.clusterBaseNs = cl.timeNs
	w.nextBlockGran = cl.startGranule
	w.blockLoaded = false
	w.blockPacket = 0

	return true
}

// ReadPacket returns the next audio packet and the granule at which it starts.
// Every block is a whole packet boundary, so the returned granule is exact. The
// granule advances by decoded packet durations, the same domain the decoder
// counts in.
func (w *webMOpusReader) ReadPacket() ([]byte, int64, error) {
	for {
		if w.blockLoaded && w.blockPacket < len(w.block.packets) {
			pkt := w.block.packets[w.blockPacket]
			gran := w.pos
			w.blockPacket++
			// The next packet starts after this one's decoded duration; the
			// block's samples cover its whole payload including any padding.
			w.pos = gran + int64(max(opusPacketSamples48(pkt), 0))
			if w.blockPacket >= len(w.block.packets) {
				w.nextBlockGran = w.blockGran + int64(packetTotalSamples(w.block))
				w.pos = w.nextBlockGran
			}
			w.posExact = true

			return pkt, gran, nil
		}

		if err := w.loadBlock(); err != nil {
			return nil, -1, err
		}
	}
}

// PreSkip is the CodecDelay in 48 kHz samples, the amount discarded from the
// start and the offset between a granule and a playable frame.
func (w *webMOpusReader) PreSkip() int { return w.preSkip }

// OutputGainQ78 is the signed Q7.8 dB output gain from the OpusHead CodecPrivate.
func (w *webMOpusReader) OutputGainQ78() int16 { return w.gainQ78 }

// Channels is the output channel count declared by the OpusHead CodecPrivate.
func (w *webMOpusReader) Channels() int { return w.channels }

// SampleRate is always 48000: Opus decoding is defined at 48 kHz regardless of
// the SamplingFrequency recorded in the track.
func (w *webMOpusReader) SampleRate() int { return webmOpusSampleRate }

// TotalGranule is the granule at the end of the playable audio, or -1 when it
// could not be derived from Duration or a cluster scan.
func (w *webMOpusReader) TotalGranule() int64 { return w.total }

// Position is the granule at which the next packet starts. It is exact at every
// block boundary.
func (w *webMOpusReader) Position() int64 { return w.pos }

// PositionExact is always true: WebM blocks are packet-aligned, so a position
// never falls mid-packet the way an Ogg page after a seek can.
func (w *webMOpusReader) PositionExact() bool { return w.posExact }

// SeekGranule repositions so the next packet starts at or before target, backing
// off by up to preroll samples for the decoder's warm-up.
//
// The cluster chosen is the last one whose decoded-domain start is at or before
// target-preroll, found through the Cues index when present and the cluster
// index otherwise. The reader then guarantees Position() <= target: if the
// chosen cluster's first packet already starts past the target, it steps back to
// the previous cluster. The decoder's warm-up skip advances from there without
// decoding, because Position is exact.
func (w *webMOpusReader) SeekGranule(target int64, preroll int64) error {
	if target < 0 {
		return fmt.Errorf("decode: negative granule seek target %d", target)
	}
	if len(w.clusters) == 0 {
		return errors.New("decode: WebM Opus stream has no clusters")
	}
	if w.total >= 0 && target > w.total {
		return fmt.Errorf("decode: granule seek target %d is past end %d", target, w.total)
	}

	want := target - preroll
	if want < 0 {
		want = 0
	}

	cl := w.clusterFor(want)
	if err := w.resetToBlock(cl); err != nil {
		return err
	}

	// A cluster's first block can start after the target (its decoded start is
	// an indexed value, but the target may fall inside the pre-roll gap before
	// it). Position must not exceed target or the decoder would deliver audio
	// from past it without an error, so back off cluster by cluster.
	for w.pos > target && w.clusterIdx > 0 {
		if err := w.resetToBlock(w.clusters[w.clusterIdx-1]); err != nil {
			return err
		}
	}

	return nil
}

// clusterFor returns the last cluster whose decoded-domain start is at or before
// want, falling back to the first cluster. Cues are used when present because
// they carry the muxer's own cluster offsets; otherwise the cluster index is
// binary-searched on its running granule.
func (w *webMOpusReader) clusterFor(want int64) webmCluster {
	if len(w.cues) > 0 {
		i := sort.Search(len(w.cues), func(i int) bool { return w.cues[i].timeNs > w.decodeFromGranule(want) }) - 1
		if i < 0 {
			i = 0
		}
		if cl, ok := w.clusterAt(w.cues[i].clusterOff); ok {
			return cl
		}
	}

	i := sort.Search(len(w.clusters), func(i int) bool { return w.clusters[i].startGranule > want }) - 1
	if i < 0 {
		i = 0
	}

	return w.clusters[i]
}

// decodeFromGranule converts a decoded-domain granule to the segment-tick
// timestamp domain, so a CueTime can be compared against it. The two domains
// agree except for the muxer's millisecond-grid rounding, which is small enough
// for choosing a cue.
func (w *webMOpusReader) decodeFromGranule(granule int64) int64 {
	return granule * 1_000_000_000 / webmOpusSampleRate
}

// clusterAt finds the cluster whose element offset matches off.
func (w *webMOpusReader) clusterAt(off int64) (webmCluster, bool) {
	i := sort.Search(len(w.clusters), func(i int) bool { return w.clusters[i].offset >= off })
	if i < len(w.clusters) && w.clusters[i].offset == off {
		return w.clusters[i], true
	}

	return webmCluster{}, false
}

// parseWebMBlock parses a Block or SimpleBlock payload (Matroska "Block
// Structure"): a track-number VINT, a signed 16-bit timestamp relative to the
// cluster, one flag byte, then optional lacing and the packets. baseNs is the
// enclosing cluster's timestamp, so the returned timeNs is absolute.
func parseWebMBlock(data []byte, timestampScale uint64, baseNs int64) (webmBlock, error) {
	track, trackW, _, err := ebmlVint(data, 0)
	if err != nil {
		return webmBlock{}, fmt.Errorf("decode: block track number: %w", err)
	}
	if trackW+3 > len(data) {
		return webmBlock{}, fmt.Errorf("%w: block header truncated", errEBMLTruncated)
	}
	rel := int16(binary.BigEndian.Uint16(data[trackW : trackW+2]))
	flags := data[trackW+2]
	rest := data[trackW+3:]

	blk := webmBlock{
		track:    track,
		timeNs:   baseNs + int64(rel)*int64(timestampScale),
		keyframe: flags&0x80 != 0,
	}
	lacing := (flags >> 1) & 0x03

	packets, err := splitWebMLacing(lacing, rest)
	if err != nil {
		return webmBlock{}, err
	}
	blk.packets = packets

	return blk, nil
}

// splitWebMLacing divides a block body into packets. Lacing is optional and the
// three non-zero modes must all be accepted: rejecting a laced file would be a
// surprise failure on a valid stream.
func splitWebMLacing(lacing byte, body []byte) ([][]byte, error) {
	if lacing == 0 {
		if len(body) == 0 {
			return nil, nil
		}

		return [][]byte{body}, nil
	}
	if len(body) < 1 {
		return nil, fmt.Errorf("%w: laced block has no frame count", errEBMLTruncated)
	}

	frames := int(body[0]) + 1
	rest := body[1:]

	switch lacing {
	case 1: // Xiph: sizes as runs of 255 ending in a byte < 255.
		sizes := make([]int, 0, frames-1)
		for range frames - 1 {
			size := 0
			for {
				if len(rest) == 0 {
					return nil, fmt.Errorf("%w: Xiph lacing ran out", errEBMLTruncated)
				}
				b := rest[0]
				rest = rest[1:]
				size += int(b)
				if b < 255 {
					break
				}
			}
			sizes = append(sizes, size)
		}

		return sliceWebMPackets(rest, sizes)
	case 2: // Fixed: every frame is the same size.
		if frames == 0 || len(rest)%frames != 0 {
			return nil, fmt.Errorf("decode: fixed lacing does not divide %d bytes into %d frames", len(rest), frames)
		}
		size := len(rest) / frames
		sizes := make([]int, frames-1)
		for i := range sizes {
			sizes[i] = size
		}

		return sliceWebMPackets(rest, sizes)
	default: // 3, EBML: first size unsigned, the rest deltas from it.
		first, w, _, err := ebmlVint(rest, 0)
		if err != nil {
			return nil, fmt.Errorf("decode: EBML lacing size: %w", err)
		}
		rest = rest[w:]
		sizes := make([]int, 0, frames-1)
		cur := int(first)
		sizes = append(sizes, cur)
		for range frames - 2 {
			delta, w, _, err := ebmlVint(rest, 0)
			if err != nil {
				return nil, fmt.Errorf("decode: EBML lacing delta: %w", err)
			}
			rest = rest[w:]
			cur += int(signedVintDelta(delta, w))
			if cur < 0 {
				return nil, fmt.Errorf("decode: EBML lacing frame size is negative")
			}
			sizes = append(sizes, cur)
		}
		// The last frame is the remainder, so only the leading frames are
		// recorded; sliceWebMPackets appends the remainder itself.
		return sliceWebMPackets(rest, sizes)
	}
}

// sliceWebMPackets cuts body into the leading sizes plus one final packet that
// takes the remainder. It enforces that the leading sizes fit and that no frame
// is empty, since lacing is attacker-controlled data and a 0-byte packet would
// be handed to the Opus decoder.
func sliceWebMPackets(body []byte, sizes []int) ([][]byte, error) {
	packets := make([][]byte, 0, len(sizes)+1)
	off := 0
	for _, size := range sizes {
		if size <= 0 || off+size > len(body) {
			return nil, fmt.Errorf("%w: lacing frame of %d bytes in a %d-byte block", errEBMLTruncated, size, len(body))
		}
		packets = append(packets, body[off:off+size])
		off += size
	}
	if off >= len(body) {
		return nil, fmt.Errorf("%w: lacing leaves a zero-length final packet", errEBMLTruncated)
	}
	packets = append(packets, body[off:])

	return packets, nil
}

// signedEBMLInt sign-extends a fixed-width signed integer field read with
// ebmlUint, such as DiscardPadding (RFC 8794 section 7.3): the sign bit is the
// most significant bit of the byte width.
func signedEBMLInt(v uint64, width int) int64 {
	if width <= 0 {
		return 0
	}
	bits := 8 * width
	if bits < 64 && v&(1<<(bits-1)) != 0 {
		v |= ^uint64(0) << bits
	}

	return int64(v)
}

// signedVintDelta sign-extends an EBML lacing size delta. A lacing delta is a
// VINT, so it carries 7*width data bits and its sign bit is the top of those.
func signedVintDelta(v uint64, width int) int64 {
	if width <= 0 {
		return 0
	}
	bits := 7 * width
	if bits < 64 && v&(1<<(bits-1)) != 0 {
		v |= ^uint64(0) << bits
	}

	return int64(v)
}

// --- forward-only reader -------------------------------------------

// forwardWebMOpus reads Opus packets from a stream that cannot seek. It holds
// no index, so it reports an unknown total and refuses to reposition, matching
// forwardOggOpus. It is used when OpenReader gets a source without io.Seeker.
type forwardWebMOpus struct {
	src *bufio.Reader
	off int64

	timestampScale uint64
	preSkip        int
	gainQ78        int16
	channels       int
	trackNumber    uint64

	// end is the exclusive end of the current master element, or the file end.
	segmentEnd    int64
	clusterEnd    int64
	clusterBaseNs int64

	pending    *webmBlock
	pendingIdx int

	pos int64
}

// newForwardWebMOpus parses the headers straight from the stream and stops at
// the first cluster.
func newForwardWebMOpus(r io.Reader) (*forwardWebMOpus, error) {
	f := &forwardWebMOpus{src: bufio.NewReaderSize(r, 64*1024), segmentEnd: -1}
	if err := f.parseHeaders(); err != nil {
		return nil, err
	}

	return f, nil
}

// readHeader reads the element header at the current offset and advances past
// it, returning an element whose offsets are absolute. The width of the ID and
// then of the size is discovered one byte at a time, because neither is known
// before its leading byte is seen.
func (f *forwardWebMOpus) readHeader() (ebmlElement, error) {
	var buf [ebmlMaxIDLen + ebmlMaxSizeLen]byte
	start := f.off

	// ID: read its leading byte, then the rest.
	if _, err := io.ReadFull(f.src, buf[:1]); err != nil {
		if errors.Is(err, io.EOF) {
			return ebmlElement{}, io.EOF
		}

		return ebmlElement{}, fmt.Errorf("%w: read element id: %w", errEBMLTruncated, err)
	}
	f.off++
	idw := vintWidth(buf[0])
	if idw < 1 || idw > ebmlMaxIDLen {
		return ebmlElement{}, fmt.Errorf("%w: id width %d", errEBMLBadVint, idw)
	}
	// The all-ones 1-byte ID is reserved (RFC 8794 section 5 plus the Matroska
	// errata); the seekable path rejects it in ebmlID, so this one must too.
	if idw == 1 && buf[0] == 0xFF {
		return ebmlElement{}, fmt.Errorf("%w: reserved id 0xFF", errEBMLBadVint)
	}
	if _, err := io.ReadFull(f.src, buf[1:idw]); err != nil {
		return ebmlElement{}, fmt.Errorf("%w: read element id: %w", errEBMLTruncated, err)
	}
	f.off += int64(idw - 1)

	// Size: same discovery, starting after the ID.
	if _, err := io.ReadFull(f.src, buf[idw:idw+1]); err != nil {
		return ebmlElement{}, fmt.Errorf("%w: read element size: %w", errEBMLTruncated, err)
	}
	f.off++
	szw := vintWidth(buf[idw])
	if szw < 1 || szw > ebmlMaxSizeLen {
		return ebmlElement{}, fmt.Errorf("%w: size width %d", errEBMLBadVint, szw)
	}
	if _, err := io.ReadFull(f.src, buf[idw+1:idw+szw]); err != nil {
		return ebmlElement{}, fmt.Errorf("%w: read element size: %w", errEBMLTruncated, err)
	}
	f.off += int64(szw - 1)

	e, err := readEBMLHeader(buf[:idw+szw], 0)
	if err != nil {
		return ebmlElement{}, err
	}
	e.DataOff += start
	if !e.Unknown {
		e.Next += start
	}

	return e, nil
}

// vintWidth returns the byte width a VINT's leading byte announces.
func vintWidth(first byte) int {
	if first == 0 {
		return 0
	}
	width := 1
	mask := byte(0x80)
	for first&mask == 0 {
		mask >>= 1
		width++
	}

	return width
}

// parseHeaders reads EBML, then walks Segment's children until the first
// cluster, so Info and Tracks are loaded before playback starts.
func (f *forwardWebMOpus) parseHeaders() error {
	ebml, err := f.readHeader()
	if err != nil {
		return err
	}
	if ebml.ID != idEBML {
		return fmt.Errorf("%w: leading element is %#x", errWebMOpusNotWebM, ebml.ID)
	}
	body, err := f.readN(int64(ebml.Size))
	if err != nil {
		return err
	}
	if err := validateWebMEBMLHeader(body); err != nil {
		return err
	}

	seg, err := f.readHeader()
	if err != nil {
		return err
	}
	if seg.ID != idSegment {
		return fmt.Errorf("%w: element after EBML is %#x, not Segment", errWebMOpusNotWebM, seg.ID)
	}
	if !seg.Unknown {
		f.segmentEnd = seg.Next
	}

	for f.segmentEnd < 0 || f.off < f.segmentEnd {
		e, err := f.readHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return err
		}
		if e.Unknown {
			e.Next = f.segmentEnd
		}
		switch e.ID {
		case idInfo:
			b, err := f.readN(int64(e.Size))
			if err != nil {
				return err
			}
			if err := f.parseInfoBody(b); err != nil {
				return err
			}
		case idTracks:
			b, err := f.readN(int64(e.Size))
			if err != nil {
				return err
			}
			if err := f.parseTracksBody(b); err != nil {
				return err
			}
		case idCues:
			// Cues cannot be used without seeking; skip it.
			if err := f.skipN(int64(e.Size)); err != nil {
				return err
			}
		case idCluster:
			f.clusterEnd = -1
			if !e.Unknown {
				f.clusterEnd = e.Next
			}

			if f.timestampScale == 0 {
				f.timestampScale = webmDefaultTimestampScale
			}
			if f.trackNumber == 0 {
				return fmt.Errorf("%w: no audio track entry in Tracks", errWebMOpusNotOpus)
			}

			return nil
		default:
			if err := f.skipN(int64(e.Size)); err != nil {
				return err
			}
		}
	}

	return errWebMOpusNoClusters
}

func (f *forwardWebMOpus) readN(size int64) ([]byte, error) {
	if size < 0 || size > 1<<30 {
		return nil, fmt.Errorf("%w: element size %d", errEBMLBadVint, size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(f.src, buf); err != nil {
		return nil, fmt.Errorf("%w: read %d bytes: %w", errEBMLTruncated, size, err)
	}
	f.off += size

	return buf, nil
}

func (f *forwardWebMOpus) skipN(size int64) error {
	if size < 0 {
		return fmt.Errorf("%w: element size %d", errEBMLBadVint, size)
	}
	if _, err := io.CopyN(io.Discard, f.src, size); err != nil {
		return fmt.Errorf("%w: skip %d bytes: %w", errEBMLTruncated, size, err)
	}
	f.off += size

	return nil
}

func (f *forwardWebMOpus) parseInfoBody(body []byte) error {
	return ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		if c.ID == idTimestampScale {
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			f.timestampScale = v
		}

		return nil
	})
}

func (f *forwardWebMOpus) parseTracksBody(body []byte) error {
	return ebmlChildren(body, 0, int64(len(body)), func(track ebmlElement) error {
		if track.ID != idTrackEntry || track.Unknown {
			return nil
		}
		return (webmTrackParser{}).parse(body[track.DataOff:track.Next], func(fields webmTrackFields) {
			if f.trackNumber != 0 {
				return
			}
			f.trackNumber = fields.trackNumber
			f.preSkip = fields.preSkip
			f.gainQ78 = fields.gain
			f.channels = fields.channels
		})
	})
}

func (f *forwardWebMOpus) PreSkip() int { return f.preSkip }

func (f *forwardWebMOpus) OutputGainQ78() int16 { return f.gainQ78 }

func (f *forwardWebMOpus) Channels() int { return f.channels }

// TotalGranule is always unknown: a forward-only stream has not read its end.
func (f *forwardWebMOpus) TotalGranule() int64 { return -1 }

func (f *forwardWebMOpus) Position() int64 { return f.pos }

// PositionExact is false: a forward-only stream never seeks, so its position is
// only a lower bound.
func (f *forwardWebMOpus) PositionExact() bool { return false }

// SeekGranule always fails with ErrNotSeekable: a forward-only stream cannot be
// repositioned, and the streamer's fallback matches that sentinel.
func (f *forwardWebMOpus) SeekGranule(int64, int64) error { return ErrNotSeekable }

// ReadPacket returns the next audio packet from the sequential block walk.
func (f *forwardWebMOpus) ReadPacket() ([]byte, int64, error) {
	for {
		if f.pending != nil && f.pendingIdx < len(f.pending.packets) {
			pkt := f.pending.packets[f.pendingIdx]
			gran := f.pos
			dur := opusPacketSamples48(pkt)
			f.pendingIdx++
			if dur > 0 {
				f.pos = gran + int64(dur)
			}

			return pkt, gran, nil
		}

		blk, err := f.nextBlock()
		if err != nil {
			return nil, -1, err
		}
		f.pending = &blk
		f.pendingIdx = 0
		f.pos = f.timestampToGranule(blk.timeNs)
	}
}

func (f *forwardWebMOpus) timestampToGranule(ns int64) int64 {
	return (ns*webmOpusSampleRate + 500_000_000) / 1_000_000_000
}

// nextBlock reads forward to the next SimpleBlock or BlockGroup of the audio
// track, crossing cluster boundaries.
func (f *forwardWebMOpus) nextBlock() (webmBlock, error) {
	for {
		if f.clusterEnd >= 0 && f.off >= f.clusterEnd {
			f.clusterEnd = -1
		}
		if f.segmentEnd >= 0 && f.off >= f.segmentEnd {
			return webmBlock{}, io.EOF
		}

		e, err := f.readHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return webmBlock{}, io.EOF
			}

			return webmBlock{}, err
		}
		if e.ID == idCluster {
			if !e.Unknown {
				f.clusterEnd = e.Next
			}

			continue
		}
		if e.ID == idTimestamp {
			body, err := f.readN(int64(e.Size))
			if err != nil {
				return webmBlock{}, err
			}
			ticks, err := ebmlUint(body, 0, int64(len(body)))
			if err != nil {
				return webmBlock{}, err
			}
			f.clusterBaseNs = int64(ticks) * int64(f.timestampScale)

			continue
		}
		if e.ID == ebmlIDVoid || e.ID == ebmlIDCRC32 {
			if err := f.skipN(int64(e.Size)); err != nil {
				return webmBlock{}, err
			}

			continue
		}
		if e.ID != idSimpleBlock && e.ID != idBlockGroup {
			if e.Unknown {
				return webmBlock{}, fmt.Errorf("%w: unexpected unknown-size element %#x", errEBMLTruncated, e.ID)
			}
			if err := f.skipN(int64(e.Size)); err != nil {
				return webmBlock{}, err
			}

			continue
		}

		body, err := f.readN(int64(e.Size))
		if err != nil {
			return webmBlock{}, err
		}
		blk, err := f.blockFromBody(e.ID, body)
		if err != nil {
			return webmBlock{}, err
		}
		if blk.track != f.trackNumber || len(blk.packets) == 0 {
			continue
		}

		return blk, nil
	}
}

// blockFromBody parses a SimpleBlock body directly or a BlockGroup body.
func (f *forwardWebMOpus) blockFromBody(id uint32, body []byte) (webmBlock, error) {
	if id == idSimpleBlock {
		return parseWebMBlock(body, f.timestampScale, f.clusterBaseNs)
	}

	var (
		blk webmBlock
		pad int64
	)
	err := ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		switch c.ID {
		case idBlock:
			b, err := parseWebMBlock(body[c.DataOff:c.Next], f.timestampScale, f.clusterBaseNs)
			if err != nil {
				return err
			}
			blk = b
		case idDiscardPadding:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			pad = signedEBMLInt(v, int(c.Size))
		}

		return nil
	})
	if err != nil {
		return webmBlock{}, err
	}
	blk.discardPad = pad

	return blk, nil
}

// validateWebMEBMLHeader checks DocType and EBMLMaxIDLength in an already-read
// EBML header body. It shares the rules with the seekable reader.
func validateWebMEBMLHeader(body []byte) error {
	docType := ""
	var maxID uint64
	err := ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		switch c.ID {
		case idDocType:
			docType = string(body[c.DataOff : c.DataOff+int64(c.Size)])
		case idEBMLMaxIDLen:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			maxID = v
		}

		return nil
	})
	if err != nil {
		return err
	}
	if docType != "webm" {
		return fmt.Errorf("%w: DocType is %q", errWebMOpusNotWebM, docType)
	}
	if maxID > ebmlMaxIDLen {
		return fmt.Errorf("%w: EBMLMaxIDLength %d exceeds %d", errWebMOpusNotWebM, maxID, ebmlMaxIDLen)
	}

	return nil
}

// --- shared track parsing ------------------------------------------

// webmTrackFields is the A_OPUS subset of a TrackEntry.
type webmTrackFields struct {
	trackNumber uint64
	preSkip     int
	seekPreRoll int
	gain        int16
	channels    int
}

// webmTrackParser parses one TrackEntry body, shared by both readers. It is a
// zero-size type so the shared logic has one home without either reader owning
// the other.
type webmTrackParser struct{}

func (webmTrackParser) parse(body []byte, adopt func(webmTrackFields)) error {
	var (
		trackType uint64
		fields    webmTrackFields
		codecID   string
		codecPriv []byte
		delayNS   uint64
		seekNS    uint64
		encrypted bool
	)
	err := ebmlChildren(body, 0, int64(len(body)), func(c ebmlElement) error {
		switch c.ID {
		case idTrackNumber:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			fields.trackNumber = v
		case idTrackType:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			trackType = v
		case idCodecID:
			codecID = string(body[c.DataOff : c.DataOff+int64(c.Size)])
		case idCodecPrivate:
			codecPriv = body[c.DataOff : c.DataOff+int64(c.Size)]
		case idCodecDelay:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			delayNS = v
		case idSeekPreRoll:
			v, err := ebmlUint(body, c.DataOff, int64(c.Size))
			if err != nil {
				return err
			}
			seekNS = v
		case idContentEnc:
			encrypted = true
		}

		return nil
	})
	if err != nil {
		return err
	}
	if trackType != webmTrackTypeAudio || codecID != "A_OPUS" {
		return nil
	}
	if encrypted {
		return errWebMOpusEncrypted
	}
	if len(codecPriv) == 0 {
		return fmt.Errorf("%w: track %d", errWebMOpusMissingCodecPriv, fields.trackNumber)
	}
	info, err := parseOggOpusHead(codecPriv)
	if err != nil {
		return fmt.Errorf("decode: WebM CodecPrivate: %w", err)
	}
	fields.channels = info.channels
	fields.gain = info.gain
	fields.seekPreRoll = int((seekNS*webmOpusSampleRate + 500_000_000) / 1_000_000_000)
	fields.preSkip = int((delayNS*webmOpusSampleRate + 500_000_000) / 1_000_000_000)
	if fields.preSkip == 0 {
		fields.preSkip = info.preSkip
	}
	adopt(fields)

	return nil
}

// --- factory, probe, registration ----------------------------------

func init() { Register(NewWebMOpusFactory()) }

// WebMOpusFactory reads Opus audio out of a WebM/Matroska container.
type WebMOpusFactory struct{}

// NewWebMOpusFactory returns a factory for the WebM/Matroska Opus decoder.
func NewWebMOpusFactory() *WebMOpusFactory { return &WebMOpusFactory{} }

func (f *WebMOpusFactory) Name() string { return "webm-opus" }

// FriendlyName is the label a UI shows for this codec.
func (f *WebMOpusFactory) FriendlyName() string { return "WebM" }

// Weight sits above the Ogg Opus factory for .webm and .weba, so a WebM file
// uses this reader; the extensions do not overlap, so it does not disturb the
// Ogg default.
func (f *WebMOpusFactory) Weight() int { return 90 }

func (f *WebMOpusFactory) Exts() []string { return []string{".webm", ".weba"} }

// Match accepts any EBML stream. Open then rejects a non-WebM DocType or a
// missing Opus track. Checking deeper would mean seeking, which sniffing must
// not do.
func (f *WebMOpusFactory) Match(magic []byte) bool {
	return len(magic) >= 4 && magic[0] == 0x1A && magic[1] == 0x45 && magic[2] == 0xDF && magic[3] == 0xA3
}

func (f *WebMOpusFactory) Open(path string) (Decoder, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// newWebMOpusDecoder closes fh through detachSource when the header parse
	// fails, so a failure here must not close it a second time.
	return newWebMOpusDecoder(fh, fh, pionWarmupFast, true)
}

// OpenReader decodes from a stream. A reader that can seek keeps native seek;
// one that cannot plays forward but refuses to reposition.
func (f *WebMOpusFactory) OpenReader(r io.Reader) (Decoder, error) {
	if rs, ok := r.(io.ReadSeeker); ok {
		return newWebMOpusDecoder(rs, nil, pionWarmupFast, true)
	}

	return newWebMOpusDecoder(r, nil, pionWarmupFast, false)
}

// Probe reads the Duration element in Info, which sits near the front of a WebM
// file, so DurationProbe is a small read rather than a tail walk. DurationScan
// walks the clusters when Info has no usable duration.
func (f *WebMOpusFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	return probeWebMOpus(path, opts.Duration)
}

// newWebMOpusDecoder builds a pionOpusDecoder over the WebM container. It
// reuses the decoder wholesale: only the container function and the parser
// label differ from the Ogg path.
func newWebMOpusDecoder(r io.Reader, closer io.Closer, warmup int64, seekable bool) (*pionOpusDecoder, error) {
	d, err := newPionOpusDecoderWith(openWebMOpusPackets, pionStreamSource{r: r, closer: closer, seekable: seekable}, warmup, "player/decode (webmopus)")
	if err != nil {
		return nil, err
	}

	return d, nil
}

// openWebMOpusPackets builds the WebM packet source for r, mirroring
// openOpusPackets: a seekable source gets the index-building reader, anything
// else the forward-only one.
func openWebMOpusPackets(r io.Reader) (opusPacketSource, error) {
	if rs, ok := r.(io.ReadSeeker); ok {
		return newWebMOpusReader(rs)
	}

	return newForwardWebMOpus(r)
}

// pionStreamSource is a pionSource over an already-open reader, used when the
// factory opened the file itself rather than through pionPathSource.
type pionStreamSource struct {
	r        io.Reader
	closer   io.Closer
	seekable bool
}

func (s pionStreamSource) open() (io.Reader, io.Closer, error) { return s.r, s.closer, nil }

func (s pionStreamSource) rewindable() bool { return s.seekable }

// --- probe ---------------------------------------------------------

// probeWebMOpus reports the stream shape without decoding. DurationUnknown
// reads only the headers; DurationProbe reads Info, which carries the float
// Duration near the front, so it is a small read; DurationScan additionally
// walks the clusters when Info has no usable Duration.
func probeWebMOpus(path string, mode core.DurationMode) (core.StreamInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return core.StreamInfo{}, err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return core.StreamInfo{}, err
	}

	info := core.StreamInfo{
		// The engine currency is always 48 kHz stereo float32, matching what
		// Open returns for the same file.
		Format:      core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		TotalFrames: -1,
	}

	w := &webMOpusReader{src: f, fileSize: size, total: -1}
	if err := w.parseHeaders(); err != nil {
		return core.StreamInfo{}, err
	}
	if mode == core.DurationUnknown {
		return info, nil
	}

	if w.hasDuration {
		total := w.durationToGranule(w.durationTicks)
		if total >= int64(w.preSkip) {
			info.TotalFrames = total - int64(w.preSkip)

			return info, nil
		}
	}
	if mode != core.DurationScan {
		// No Duration in Info and no permission for a full walk: reporting
		// unknown is honest, and guessing would corrupt duration downstream.
		return info, nil
	}
	if err := w.buildIndex(); err != nil {
		// A scan that cannot find the clusters leaves the total unknown rather
		// than failing a metadata probe over a file the decoder will report.
		return info, nil
	}
	if w.total >= int64(w.preSkip) {
		info.TotalFrames = w.total - int64(w.preSkip)
	}

	return info, nil
}
