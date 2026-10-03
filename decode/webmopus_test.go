package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"github.com/dlcuy22/player/core"
)

// --- EBML construction helpers -------------------------------------

// ebmlSize encodes a Data Size VINT for n (RFC 8794 section 6), choosing the
// shortest width that fits. It is the inverse of ebmlVint and lets the tests
// build real element headers rather than hand-writing byte literals.
func ebmlSize(n uint64) []byte {
	for width := 1; width <= 8; width++ {
		if n < 1<<(7*width)-1 {
			buf := make([]byte, width)
			v := n | 1<<(7*width)
			for i := width - 1; i >= 0; i-- {
				buf[i] = byte(v)
				v >>= 8
			}

			return buf
		}
	}

	panic("ebmlSize: value too large")
}

// ebmlIDBytes encodes an element ID VINT with its marker kept. The IDs used in
// these tests are 1 to 4 bytes, so the raw big-endian value is its own encoding.
func ebmlIDBytes(id uint32) []byte {
	switch {
	case id < 0x100:
		return []byte{byte(id)}
	case id < 0x10000:
		return []byte{byte(id >> 8), byte(id)}
	case id < 0x1000000:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// ebmlElem assembles id+size+body.
func ebmlElem(id uint32, body []byte) []byte {
	out := append(ebmlIDBytes(id), ebmlSize(uint64(len(body)))...)

	return append(out, body...)
}

// ebmlElemUnknown assembles a master element with the unknown-size sentinel.
func ebmlElemUnknown(id uint32, body []byte) []byte {
	out := append(ebmlIDBytes(id), 0xFF)

	return append(out, body...)
}

func ebmlUintElem(id uint32, v uint64) []byte {
	var body []byte
	for v > 0 {
		body = append([]byte{byte(v)}, body...)
		v >>= 8
	}

	return ebmlElem(id, body)
}

func ebmlStringElem(id uint32, s string) []byte { return ebmlElem(id, []byte(s)) }

func ebmlFloat32Elem(id uint32, v float32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], math.Float32bits(v))

	return ebmlElem(id, b[:])
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}

	return out
}

// hugeSizeElem builds an element whose size VINT is an 8-byte known value near
// 2^55, far larger than any test file. A reader that trusts it would try to
// allocate 2^55 bytes and panic with "makeslice: len out of range".
func hugeSizeElem(id uint32, body []byte) []byte {
	size := []byte{0x01, 0x00, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	return concat(ebmlIDBytes(id), size, body)
}

// webmFixtureSpec describes a synthetic WebM file to build.
type webmFixtureSpec struct {
	docType        string
	unknownSegment bool
	unknownCluster bool
	noCues         bool
	noDuration     bool
	durationTicks  float32 // Duration override in ticks; 0 keeps the default 2000
	tracks         []byte  // raw TrackEntry body, or nil for a default Opus track
	infoExtra      []byte  // appended inside Info
	seekHead       bool
	voids          bool // insert Void and CRC-32 elements at several levels
	// unknownVoid inserts an unknown-size Void at level 1, which cannot be
	// skipped because its extent is not encoded anywhere.
	unknownVoid bool
	// hugeTimestampSize writes the first cluster's Timestamp element with a
	// size VINT claiming the largest 8-byte value, to prove the reader bounds
	// allocations rather than panicking.
	hugeTimestampSize bool
	// hugeBlockSize writes the first SimpleBlock with a huge size VINT.
	hugeBlockSize bool
	clusters      []webmClusterSpec
}

// webmClusterSpec is one cluster's timestamp and blocks.
type webmClusterSpec struct {
	timeTicks uint64
	blocks    []webmBlockSpec
}

// webmBlockSpec is one block: a track number, a relative timestamp in ticks,
// the raw flag byte, and the payload after the block header.
type webmBlockSpec struct {
	track     uint64
	relTicks  int16
	flags     byte
	blockBody []byte // bytes after the track/timestamp/flags header
	group     bool   // wrap in a BlockGroup
	padNs     int64  // DiscardPadding in a BlockGroup
}

// opusTrackEntry builds a minimal A_OPUS TrackEntry with the given CodecDelay
// in nanoseconds and OpusHead pre-skip.
func opusTrackEntry(trackNumber uint64, codecDelayNS uint64, preSkip uint16, channels byte) []byte {
	return concat(
		ebmlUintElem(idTrackNumber, trackNumber),
		ebmlUintElem(idTrackType, webmTrackTypeAudio),
		ebmlStringElem(idCodecID, "A_OPUS"),
		ebmlElem(idCodecPrivate, opusHeadPacket(preSkip, channels)),
		ebmlUintElem(idCodecDelay, codecDelayNS),
		ebmlUintElem(idSeekPreRoll, 80_000_000),
	)
}

// blockHeader builds the Block/SimpleBlock header for one packet: a track VINT,
// an int16 relative timestamp and the flag byte.
func blockHeader(track uint64, relTicks int16, flags byte) []byte {
	var ts [2]byte
	binary.BigEndian.PutUint16(ts[:], uint16(relTicks))

	return concat(ebmlSize(track), ts[:], []byte{flags})
}

// buildWebM assembles a full EBML file from a spec.
func buildWebM(spec webmFixtureSpec) []byte {
	docType := spec.docType
	if docType == "" {
		docType = "webm"
	}
	tracks := spec.tracks
	if tracks == nil {
		tracks = opusTrackEntry(1, 6_500_000, 312, 2)
	}

	ebml := ebmlElem(idEBML, concat(
		ebmlUintElem(0x4286, 1), // EBMLVersion
		ebmlUintElem(0x42F7, 1), // EBMLReadVersion
		ebmlUintElem(idEBMLMaxIDLen, 4),
		ebmlUintElem(0x42F3, 8), // EBMLMaxSizeLength
		ebmlStringElem(idDocType, docType),
		ebmlUintElem(0x4287, 2), // DocTypeVersion
		ebmlUintElem(0x4285, 2), // DocTypeReadVersion
	))

	infoBody := ebmlUintElem(idTimestampScale, 1_000_000)
	if !spec.noDuration {
		ticks := float32(2000)
		if spec.durationTicks != 0 {
			ticks = spec.durationTicks
		}
		infoBody = append(infoBody, ebmlFloat32Elem(idDuration, ticks)...)
	}
	infoBody = append(infoBody, spec.infoExtra...)
	info := ebmlElem(idInfo, infoBody)

	trackElem := ebmlElem(idTracks, ebmlElem(idTrackEntry, tracks))

	var (
		children   [][]byte
		cues       []byte
		clusterIdx []int
	)
	if spec.seekHead {
		// A SeekHead whose SeekID contains the Cues ID bytes, the trap that
		// made a byte scan find the wrong offset.
		seekEntry := concat(
			ebmlElem(0x53AB, ebmlIDBytes(idCues)),
			ebmlUintElem(0x53AC, 999),
		)
		children = append(children, ebmlElem(idSeekHead, ebmlElem(0x4DBB, seekEntry)))
	}
	if spec.voids {
		children = append(children, ebmlElem(ebmlIDVoid, bytes.Repeat([]byte{0}, 32)))
		children = append(children, ebmlElem(ebmlIDCRC32, []byte{0, 0, 0, 0}))
	}
	if spec.unknownVoid {
		// An unknown-size Void: ID 0xEC then the all-ones size sentinel.
		children = append(children, []byte{ebmlIDVoid, 0xFF})
	}
	children = append(children, info, trackElem)

	for ci, cl := range spec.clusters {
		var blocks []byte
		for bi, b := range cl.blocks {
			header := blockHeader(b.track, b.relTicks, b.flags)
			body := concat(header, b.blockBody)
			if b.group {
				grp := ebmlElem(idBlock, body)
				if b.padNs != 0 {
					grp = concat(grp, ebmlElem(idDiscardPadding, int64Bytes(b.padNs)))
				}
				blocks = append(blocks, ebmlElem(idBlockGroup, grp)...)
			} else if spec.hugeBlockSize && ci == 0 && bi == 0 {
				blocks = append(blocks, hugeSizeElem(idSimpleBlock, body)...)
			} else {
				blocks = append(blocks, ebmlElem(idSimpleBlock, body)...)
			}
		}
		tsElem := ebmlUintElem(idTimestamp, cl.timeTicks)
		if spec.hugeTimestampSize && ci == 0 {
			tsElem = hugeSizeElem(idTimestamp, []byte{0x00})
		}
		clusterBody := concat(tsElem, blocks)
		var cluster []byte
		if spec.unknownCluster {
			cluster = ebmlElemUnknown(idCluster, clusterBody)
		} else {
			cluster = ebmlElem(idCluster, clusterBody)
		}
		clusterIdx = append(clusterIdx, len(children))
		children = append(children, cluster)
	}

	if !spec.noCues {
		cues = buildCuesFor(spec, children, clusterIdx)
		i := idxOfFirstCluster(children)
		children = append(children[:i:i], append([][]byte{cues}, children[i:]...)...)
	}

	segmentBody := concat(children...)
	var segment []byte
	if spec.unknownSegment {
		segment = ebmlElemUnknown(idSegment, segmentBody)
	} else {
		segment = ebmlElem(idSegment, segmentBody)
	}

	return concat(ebml, segment)
}

// buildCuesFor produces the Cues element for the clusters in children, with
// each CueClusterPosition relative to the Segment's data. The position depends
// on the length of Cues itself (which precedes the clusters), so the length is
// iterated until it stops growing, as a real muxer would.
func buildCuesFor(spec webmFixtureSpec, children [][]byte, clusterIdx []int) []byte {
	cuesLen := 0
	var cues []byte
	for range 8 {
		rel := cuesLen
		positions := make([]int, len(clusterIdx))
		for i, ci := range clusterIdx {
			for j := 0; j < ci; j++ {
				rel += len(children[j])
			}
			positions[i] = rel
			rel += len(children[ci])
		}
		var points []byte
		for i, pos := range positions {
			points = append(points, ebmlElem(idCuePoint, concat(
				ebmlUintElem(idCueTime, spec.clusters[i].timeTicks),
				ebmlElem(idCueTrackPos, concat(
					ebmlUintElem(idCueClusterPos, uint64(pos)),
					ebmlUintElem(idCueBlockNum, 1),
				)),
			))...)
		}
		next := ebmlElem(idCues, points)
		if len(next) == cuesLen {
			cues = next

			break
		}
		cuesLen = len(next)
		cues = next
	}

	return cues
}

func int64Bytes(v int64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(v))
	// Trim leading sign-extension bytes to the shortest signed form.
	i := 0
	for i < 7 {
		hi := buf[i]
		next := buf[i+1]
		if (hi == 0x00 && next&0x80 == 0) || (hi == 0xFF && next&0x80 != 0) {
			i++

			continue
		}

		break
	}

	return buf[i:]
}

func idxOfFirstCluster(children [][]byte) int {
	for i, c := range children {
		if len(c) == 0 {
			continue
		}
		id, _, err := ebmlID(c, 0)
		if err == nil && id == idCluster {
			return i
		}
	}

	return len(children)
}

// writeWebM writes a built file into a temp dir.
func writeWebM(t *testing.T, name string, body []byte) string {
	t.Helper()

	return writeFile(t, t.TempDir(), name, body)
}

// mustWebMReader opens a built file.
func mustWebMReader(t *testing.T, path string) *webMOpusReader {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })

	r, err := newWebMOpusReader(f)
	if err != nil {
		t.Fatalf("newWebMOpusReader(%s): %v", path, err)
	}

	return r
}

// --- parity with Ogg -----------------------------------------------

// TestWebMOpusParityWithOgg is the strongest proof that the container mapping is
// right: the same source muxed to Ogg and WebM must decode to the same PCM. A
// wrong pre-skip, timestamp base or packet split shows up immediately. The
// fixtures are produced from the same tone, so the two encodes are byte-equal
// here and the difference is at the noise floor.
func TestWebMOpusParityWithOgg(t *testing.T) {
	tests := []struct {
		ogg  string
		webm string
	}{
		{"stereo_2s.opus", "webm_stereo_2s.webm"},
		{"mono_1s.opus", "webm_mono_1s.webm"},
	}
	for _, tt := range tests {
		t.Run(tt.webm, func(t *testing.T) {
			ogg := decodeFixture(t, tt.ogg)

			d, err := NewWebMOpusFactory().Open(fixturePath(t, tt.webm))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()
			webm := decodeAll(t, d)

			common := min(len(ogg), len(webm))
			if common < 48000 {
				t.Fatalf("only %d common samples, want at least one second", common)
			}
			db := rmsDiffDB(ogg[:common], webm[:common])
			t.Logf("%s: %d common frames, difference %.2f dBFS", tt.webm, common/2, db)
			if db > parityFloorDBFS {
				t.Fatalf("WebM and Ogg differ by %.2f dBFS, want below %.0f", db, parityFloorDBFS)
			}
		})
	}
}

// --- totals --------------------------------------------------------

// TestWebMOpusTotalsMatchFFprobe pins the totals against ffprobe's report of the
// checked-in fixtures, and checks the Duration path and the cluster-scan path
// agree. ffprobe reports initial_padding=312 and 2.008 s / 1.008 s.
func TestWebMOpusTotalsMatchFFprobe(t *testing.T) {
	tests := []struct {
		fixture    string
		wantFrames int64
	}{
		// 2.008 s * 48000 - 312; 1.008 s * 48000 - 312.
		{"webm_stereo_2s.webm", 96072},
		{"webm_mono_1s.webm", 48072},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			d, err := NewWebMOpusFactory().Open(fixturePath(t, tt.fixture))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()

			if got := d.Info().TotalFrames; got != tt.wantFrames {
				t.Fatalf("TotalFrames = %d, want %d", got, tt.wantFrames)
			}

			// The cluster-scan path must agree with the Duration path within one
			// packet: the scan ends at the last block's timestamp plus its
			// packets, while Duration is the muxer's rounded container length.
			path := stripDuration(t, tt.fixture)
			scanned, err := NewWebMOpusFactory().Open(path)
			if err != nil {
				t.Fatalf("Open without Duration: %v", err)
			}
			defer scanned.Close()
			got := scanned.Info().TotalFrames
			if abs64(float64(got-tt.wantFrames)) > 5760 {
				t.Fatalf("cluster-scan TotalFrames = %d, want within one packet of %d", got, tt.wantFrames)
			}
		})
	}
}

// stripDuration replaces the Info Duration element with a Void element of the
// same total length, so the cluster-scan path is exercised without any Element
// Data Size patching: the tree shape and every offset are unchanged.
func stripDuration(t *testing.T, fixture string) string {
	t.Helper()

	src, err := os.ReadFile(fixturePath(t, fixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	e, err := readEBMLHeader(src, 0)
	if err != nil {
		t.Fatalf("read EBML header: %v", err)
	}
	seg, err := readEBMLHeader(src, e.Next)
	if err != nil {
		t.Fatalf("read Segment header: %v", err)
	}

	out := append([]byte(nil), src...)
	durOff := int64(0)
	if err := ebmlChildren(out, seg.DataOff, seg.Next, func(c ebmlElement) error {
		if c.ID != idInfo {
			return nil
		}
		return ebmlChildren(out, c.DataOff, c.Next, func(ic ebmlElement) error {
			if ic.ID == idDuration {
				durOff = ic.DataOff - int64(len(ebmlIDBytes(ic.ID))) - int64(len(ebmlSize(ic.Size)))
				total := ic.Next - durOff
				replaceWithVoid(t, out, durOff, total)
			}

			return nil
		})
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if durOff == 0 {
		t.Fatalf("no Duration element in %s", fixture)
	}

	return writeFile(t, t.TempDir(), fixture, out)
}

// replaceWithVoid overwrites the element at off, total bytes long, with a Void
// element of exactly the same length. Void is a global element the walker skips,
// so this deletes an element without disturbing any size or offset.
func replaceWithVoid(t *testing.T, buf []byte, off, total int64) {
	t.Helper()

	// 1 byte ID + size field + body == total.
	for body := total - 2; body >= 0; body-- {
		enc := ebmlSize(uint64(body))
		if int64(1+len(enc))+body == total {
			buf[off] = ebmlIDVoid
			copy(buf[off+1:], enc)
			for i := off + 1 + int64(len(enc)); i < off+total; i++ {
				buf[i] = 0
			}

			return
		}
	}

	t.Fatalf("cannot fit a Void into %d bytes", total)
}

// stripCues removes the top-level Cues element, leaving a same-length Void.
func stripCues(t *testing.T, path string) string {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	e, err := readEBMLHeader(src, 0)
	if err != nil {
		t.Fatalf("EBML header: %v", err)
	}
	seg, err := readEBMLHeader(src, e.Next)
	if err != nil {
		t.Fatalf("Segment header: %v", err)
	}

	out := append([]byte(nil), src...)
	found := false
	if err := ebmlChildren(out, seg.DataOff, seg.Next, func(c ebmlElement) error {
		if c.ID == idCues {
			start := c.DataOff - int64(len(ebmlIDBytes(c.ID))) - int64(len(ebmlSize(c.Size)))
			replaceWithVoid(t, out, start, c.Next-start)
			found = true
		}

		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !found {
		t.Fatalf("no Cues in %s", path)
	}

	return writeFile(t, t.TempDir(), "nocues.webm", out)
}

// --- seek ----------------------------------------------------------

// TestWebMOpusSeekMatchesStraightDecode seeks to several points and requires the
// PCM after convergence to match a straight decode. The fast window is not
// bit-exact at the landing point, so the first 200 ms are dropped, exactly as
// the libopusfile parity test does.
func TestWebMOpusSeekMatchesStraightDecode(t *testing.T) {
	full := func() []float32 {
		d, err := NewWebMOpusFactory().Open(fixturePath(t, "webm_stereo_2s.webm"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer d.Close()

		return decodeAll(t, d)
	}()

	targets := []int64{0, 1000, 12345, 30000, 48000, 70000}
	for _, at := range targets {
		d, err := NewWebMOpusFactory().Open(fixturePath(t, "webm_stereo_2s.webm"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := d.(Seeker).SeekFrame(at); err != nil {
			d.Close()
			t.Fatalf("SeekFrame(%d): %v", at, err)
		}
		// A window that stays inside the straight decode and leaves room for
		// the convergence skip.
		frames := min(int64(24000), int64(len(full)/2)-at)
		if frames <= seekConvergeFrames {
			d.Close()
			t.Fatalf("target %d leaves only %d frames after the convergence skip", at, frames)
		}
		got := readExactlyFrames(t, d, int(frames))
		d.Close()

		want := full[at*2 : (at+frames)*2]
		db := segmentDiffDB(want, got, seekConvergeFrames)
		t.Logf("seek to %d: difference after convergence %.2f dBFS", at, db)
		if db > parityFloorDBFS {
			t.Fatalf("seek to %d differs by %.2f dBFS, want below %.0f", at, db, parityFloorDBFS)
		}
	}
}

// TestWebMOpusSeekIsGranuleExact checks the reader lands on a packet boundary at
// or before the requested granule, mirroring the Ogg reader contract. It runs on
// a multi-cluster file so Cue lookup and cluster selection are actually
// exercised, unlike a single-cluster fixture where every seek re-anchors at 0.
func TestWebMOpusSeekIsGranuleExact(t *testing.T) {
	const per = 100
	body := multiClusterWebM(per)
	r := mustWebMReader(t, writeWebM(t, "granule.webm", body))
	if r.PreSkip() != 312 {
		t.Fatalf("PreSkip = %d, want 312", r.PreSkip())
	}
	if !r.PositionExact() {
		t.Fatal("PositionExact is false for a packet-aligned WebM stream")
	}
	if len(r.cues) != 3 {
		t.Fatalf("fixture has %d cues, want 3", len(r.cues))
	}

	// Decoded starts are 0, 96000, 192000. A target inside a cluster lands on
	// that cluster's first packet.
	cases := []struct {
		target int64
		want   int64
	}{
		{500, 0},
		{48000, 0},
		{96000, 96000},
		{150000, 96000},
		{192000, 192000},
		{280000, 192000},
	}
	for _, tt := range cases {
		if err := r.SeekGranule(tt.target, 0); err != nil {
			t.Fatalf("SeekGranule(%d): %v", tt.target, err)
		}
		if !r.PositionExact() {
			t.Fatalf("SeekGranule(%d): PositionExact is false", tt.target)
		}
		if r.Position() != tt.want {
			t.Fatalf("SeekGranule(%d): Position = %d, want %d", tt.target, r.Position(), tt.want)
		}
		if _, _, err := r.ReadPacket(); err != nil {
			t.Fatalf("SeekGranule(%d): ReadPacket: %v", tt.target, err)
		}
	}

	if err := r.SeekGranule(1<<40, 0); err == nil {
		t.Fatal("SeekGranule past the end returned nil")
	}
	if err := r.SeekGranule(-1, 0); err == nil {
		t.Fatal("SeekGranule(-1) returned nil")
	}
}

// TestWebMOpusSeekWithoutCues exercises the cluster-scan fallback: with Cues
// removed, the reader binary-searches its cluster index by decoded granule. It
// uses a multi-cluster file so the search has to pick, not just re-anchor.
func TestWebMOpusSeekWithoutCues(t *testing.T) {
	built := writeWebM(t, "withcues.webm", multiClusterWebM(100))
	stripped := stripCues(t, built)

	r := mustWebMReader(t, stripped)
	if len(r.cues) != 0 {
		t.Fatalf("Cues were not stripped: %d entries remain", len(r.cues))
	}
	// Cluster 1 starts at decoded granule 96000; target inside it.
	if err := r.SeekGranule(144000, 3840); err != nil {
		t.Fatalf("SeekGranule without Cues: %v", err)
	}
	if r.Position() > 144000 {
		t.Fatalf("Position = %d exceeds the target", r.Position())
	}
	pkt, _, err := r.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket after seek without Cues: %v", err)
	}
	if pkt[1] != 0xA1 {
		t.Fatalf("seek without Cues returned packet marker 0x%02x, want 0xA1 (cluster 1)", pkt[1])
	}
}

// --- lacing --------------------------------------------------------

// TestWebMOpusLacingModes crafts one block per lacing mode and checks the reader
// recovers the packets. ffmpeg never emits laced Opus here, so the bytes are
// built by hand rather than the case being skipped.
func TestWebMOpusLacingModes(t *testing.T) {
	p1 := bytes.Repeat([]byte{0xAA}, 7)
	p2 := bytes.Repeat([]byte{0xBB}, 5)
	p3 := bytes.Repeat([]byte{0xCC}, 9)

	tests := []struct {
		name   string
		lacing byte
		body   []byte
		want   [][]byte
	}{
		{
			name:   "none",
			lacing: 0,
			body:   concat(p1, p2, p3),
			want:   [][]byte{concat(p1, p2, p3)},
		},
		{
			// Xiph: frame count byte, then a size run per leading frame.
			name:   "xiph",
			lacing: 1,
			body:   concat([]byte{2, byte(len(p1)), byte(len(p2))}, p1, p2, p3),
			want:   [][]byte{p1, p2, p3},
		},
		{
			// Fixed: frame count byte, then equal-sized frames.
			name:   "fixed",
			lacing: 2,
			body:   concat([]byte{2}, p1, p1, p1),
			want:   [][]byte{p1, p1, p1},
		},
		{
			// EBML: frame count byte, first size VINT, then signed deltas.
			name:   "ebml",
			lacing: 3,
			body:   concat([]byte{2}, ebmlSize(uint64(len(p1))), signedVint(int64(len(p2)-len(p1))), p1, p2, p3),
			want:   [][]byte{p1, p2, p3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildWebM(webmFixtureSpec{
				clusters: []webmClusterSpec{{
					timeTicks: 0,
					blocks: []webmBlockSpec{{
						track:     1,
						flags:     tt.lacing << 1,
						blockBody: tt.body,
					}},
				}},
			})
			r := mustWebMReader(t, writeWebM(t, "lacing.webm", body))

			for i, want := range tt.want {
				pkt, _, err := r.ReadPacket()
				if err != nil {
					t.Fatalf("packet %d: %v", i, err)
				}
				if !bytes.Equal(pkt, want) {
					t.Fatalf("packet %d = %d bytes, want %d", i, len(pkt), len(want))
				}
			}
			if _, _, err := r.ReadPacket(); !errors.Is(err, io.EOF) {
				t.Fatalf("end err = %v, want io.EOF", err)
			}
		})
	}
}

// signedVint encodes v as an EBML signed VINT for lacing deltas: a VINT whose
// value keeps the sign bit and the all-ones prefix, per the block lacing rule.
func signedVint(v int64) []byte {
	// Range 0: 6 data bits plus a sign bit.
	var width int
	for _, w := range []int{1, 2, 3, 4, 5} {
		bits := 7*w - 1
		min := int64(-1) << (bits - 1)
		max := int64(1)<<(bits-1) - 1
		if v >= min && v <= max {
			width = w

			break
		}
	}
	if width == 0 {
		panic("signedVint: out of range")
	}
	raw := uint64(v)
	total := 7 * width
	if v < 0 {
		raw = (uint64(1) << total) + raw
	}
	// Set the marker bit at position 7*width.
	out := make([]byte, width)
	raw |= uint64(1) << total
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(raw)
		raw >>= 8
	}

	return out
}

// --- unknown size --------------------------------------------------

// TestWebMOpusUnknownSizeSegmentAndCluster builds a stream whose Segment and
// Cluster both carry the unknown-size sentinel, as a live or piped muxer emits.
func TestWebMOpusUnknownSizeSegmentAndCluster(t *testing.T) {
	p1 := bytes.Repeat([]byte{0x11}, 20)
	p2 := bytes.Repeat([]byte{0x22}, 20)
	body := buildWebM(webmFixtureSpec{
		unknownSegment: true,
		unknownCluster: true,
		clusters: []webmClusterSpec{{
			timeTicks: 0,
			blocks: []webmBlockSpec{
				{track: 1, flags: 0x80, blockBody: p1},
				{track: 1, relTicks: 20, flags: 0x80, blockBody: p2},
			},
		}},
	})
	r := mustWebMReader(t, writeWebM(t, "unknown.webm", body))

	got := readWebMPackets(t, r)
	if len(got) != 2 {
		t.Fatalf("read %d packets, want 2", len(got))
	}
	if !bytes.Equal(got[0].data, p1) || !bytes.Equal(got[1].data, p2) {
		t.Fatal("unknown-size walk returned the wrong packets")
	}
}

// --- missing Duration / Cues --------------------------------------

// TestWebMOpusWithoutDurationFallsBackToClusterScan builds a file with no
// Duration and checks the total comes from the cluster index: two 2.5 ms packets
// advance the decoded domain by 2*120 = 240 samples.
func TestWebMOpusWithoutDurationFallsBackToClusterScan(t *testing.T) {
	body := buildWebM(webmFixtureSpec{
		noDuration: true,
		clusters: []webmClusterSpec{{
			timeTicks: 0,
			blocks: []webmBlockSpec{
				{track: 1, flags: 0x80, blockBody: opusPacket2p5ms()},
				{track: 1, relTicks: 20, flags: 0x80, blockBody: opusPacket2p5ms()},
			},
		}},
	})
	r := mustWebMReader(t, writeWebM(t, "nodur.webm", body))
	if got := r.TotalGranule(); got != 240 {
		t.Fatalf("TotalGranule = %d, want 240 from the cluster index", got)
	}
}

// opusPacket2p5ms is a CELT 2.5 ms packet: a TOC of 0x80 and a payload byte. It
// gives the scan a packet length it can sum.
func opusPacket2p5ms() []byte { return []byte{0x80, 0x00} }

// opusPacket20ms is a CELT 20 ms packet: TOC 0x98 selects the 20 ms frame size,
// so opusPacketSamples48 reports 960 samples. It lets synthetic blocks advance
// the decoded-domain position by a full frame, like a real muxer's blocks.
func opusPacket20ms() []byte { return []byte{0x98, 0x00} }

// --- non-seekable --------------------------------------------------

// TestWebMOpusForwardMatchesSeekable proves the forward-only reader parses the
// same packets as the indexed one, and that it refuses to seek.
func TestWebMOpusForwardMatchesSeekable(t *testing.T) {
	want := decodeWebMFixture(t, "webm_stereo_2s.webm")

	f, err := os.Open(fixturePath(t, "webm_stereo_2s.webm"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	d, err := NewWebMOpusFactory().OpenReader(oneShotReader{f})
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer d.Close()

	if d.Info().TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1 for a forward-only source", d.Info().TotalFrames)
	}
	if db := rmsDiffDB(want, decodeAll(t, d)); db > parityFloorDBFS {
		t.Fatalf("forward-only WebM differs by %.2f dBFS", db)
	}
	if err := d.(Seeker).SeekFrame(1000); err == nil {
		t.Fatal("SeekFrame succeeded on a non-seekable WebM stream")
	}
}

// --- Void and CRC-32 ----------------------------------------------

// TestWebMOpusSkipsVoidAndCRC inserts Void and CRC-32 at several levels and
// requires the file to open and read normally.
func TestWebMOpusSkipsVoidAndCRC(t *testing.T) {
	p := bytes.Repeat([]byte{0x44}, 16)
	body := buildWebM(webmFixtureSpec{
		voids: true,
		clusters: []webmClusterSpec{{
			timeTicks: 0,
			blocks: []webmBlockSpec{
				{track: 1, flags: 0x80, blockBody: p},
				{track: 1, relTicks: 20, flags: 0x80, blockBody: p},
			},
		}},
	})
	r := mustWebMReader(t, writeWebM(t, "voids.webm", body))
	got := readWebMPackets(t, r)
	if len(got) != 2 {
		t.Fatalf("read %d packets, want 2 with Void/CRC skipped", len(got))
	}
}

// --- DiscardPadding ------------------------------------------------

// TestWebMOpusReadsBlockGroupDiscardPadding builds a BlockGroup with signed
// DiscardPadding and checks the block is read and the padding recorded.
func TestWebMOpusReadsBlockGroupDiscardPadding(t *testing.T) {
	p := []byte{0x80, 0x00}
	body := buildWebM(webmFixtureSpec{
		clusters: []webmClusterSpec{{
			timeTicks: 0,
			blocks: []webmBlockSpec{{
				track:     1,
				flags:     0x80,
				blockBody: p,
				group:     true,
				padNs:     -5_000_000, // negative padding is legal
			}},
		}},
	})
	r := mustWebMReader(t, writeWebM(t, "pad.webm", body))
	pkt, _, err := r.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if !bytes.Equal(pkt, p) {
		t.Fatalf("packet = %v, want %v", pkt, p)
	}
	if r.block.discardPad != -5_000_000 {
		t.Fatalf("discardPad = %d, want -5000000", r.block.discardPad)
	}
}

// --- negative cases ------------------------------------------------

// TestWebMOpusRejectsBadStreams requires a clear error, never a panic, for each
// malformed header.
func TestWebMOpusRejectsBadStreams(t *testing.T) {
	tests := []struct {
		name string
		spec webmFixtureSpec
	}{
		{
			name: "bad doctype",
			spec: webmFixtureSpec{docType: "matroska"},
		},
		{
			name: "bad codecid",
			spec: webmFixtureSpec{tracks: concat(
				ebmlUintElem(idTrackNumber, 1),
				ebmlUintElem(idTrackType, webmTrackTypeAudio),
				ebmlStringElem(idCodecID, "A_VORBIS"),
				ebmlElem(idCodecPrivate, opusHeadPacket(312, 2)),
			)},
		},
		{
			name: "missing codecprivate",
			spec: webmFixtureSpec{tracks: concat(
				ebmlUintElem(idTrackNumber, 1),
				ebmlUintElem(idTrackType, webmTrackTypeAudio),
				ebmlStringElem(idCodecID, "A_OPUS"),
			)},
		},
		{
			name: "encrypted",
			spec: webmFixtureSpec{tracks: concat(
				opusTrackEntry(1, 6_500_000, 312, 2),
				ebmlElem(idContentEnc, nil),
			)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeWebM(t, "bad.webm", buildWebM(tt.spec))
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()
			if _, err := newWebMOpusReader(f); err == nil {
				t.Fatal("reader accepted a malformed stream")
			}
		})
	}
}

// TestWebMOpusRejectsBadIDAndPanicFree feeds corrupt IDs and truncations and
// requires errors rather than panics.
func TestWebMOpusRejectsBadIDAndPanicFree(t *testing.T) {
	cases := map[string][]byte{
		"empty":          {},
		"not ebml":       []byte("this is not ebml at all, not even close"),
		"reserved id":    {0xFF, 0x01, 0x02, 0x03},
		"zero leading":   {0x00, 0x00, 0x00, 0x00},
		"truncated head": {0x1A, 0x45, 0xDF, 0xA3},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), name+".webm", body)
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()
			if _, err := newWebMOpusReader(f); err == nil {
				t.Fatal("reader accepted corrupt input")
			}
		})
	}
}

// --- factory and descriptor ---------------------------------------

func TestWebMOpusFactoryContract(t *testing.T) {
	f := NewWebMOpusFactory()
	if f.Name() != "webm-opus" {
		t.Fatalf("Name = %q", f.Name())
	}
	if f.FriendlyName() != "WebM" {
		t.Fatalf("FriendlyName = %q", f.FriendlyName())
	}
	if f.Weight() != 90 {
		t.Fatalf("Weight = %d, want 90", f.Weight())
	}
	exts := f.Exts()
	if len(exts) != 2 || exts[0] != ".webm" || exts[1] != ".weba" {
		t.Fatalf("Exts = %v", exts)
	}
	if !f.Match([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x00}) {
		t.Fatal("Match rejected an EBML header")
	}
	if f.Match([]byte("OggS")) {
		t.Fatal("Match accepted an Ogg header")
	}
	if f.Match([]byte{0x1A, 0x45}) {
		t.Fatal("Match accepted a truncated header")
	}
}

// TestWebMOpusDescriptor pins the diagnostic labels: the codec is still
// pion/opus, and the parser names the container actually read.
func TestWebMOpusDescriptor(t *testing.T) {
	d, err := NewWebMOpusFactory().Open(fixturePath(t, "webm_stereo_2s.webm"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	decoder, parser := Describe(d)
	if decoder != "pion/opus" {
		t.Fatalf("DecoderName = %q, want pion/opus", decoder)
	}
	if parser != "player/decode (webmopus)" {
		t.Fatalf("ParserName = %q, want player/decode (webmopus)", parser)
	}
}

// TestWebMOpusProbeReadsDurationFront pins the probe: Duration is in Info near
// the front, so DurationProbe reports the total without touching the clusters.
func TestWebMOpusProbeReadsDurationFront(t *testing.T) {
	info, err := NewWebMOpusFactory().Probe(fixturePath(t, "webm_stereo_2s.webm"), ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != 96072 {
		t.Fatalf("TotalFrames = %d, want 96072", info.TotalFrames)
	}
	if info.Format != (core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}) {
		t.Fatalf("Format = %+v", info.Format)
	}

	unknown, err := NewWebMOpusFactory().Probe(fixturePath(t, "webm_stereo_2s.webm"), ProbeOptions{Duration: core.DurationUnknown})
	if err != nil {
		t.Fatalf("Probe(unknown): %v", err)
	}
	if unknown.TotalFrames != -1 {
		t.Fatalf("DurationUnknown TotalFrames = %d, want -1", unknown.TotalFrames)
	}
}

// --- regression coverage -------------------------------------------

// TestWebMOpusRejectsHugeElementSize is the panic regression: an element size
// VINT claiming ~2^55 must produce an error, never a make([]byte) panic. Both
// the Cluster Timestamp path and the SimpleBlock path are covered, because the
// blocker was reachable from both.
func TestWebMOpusRejectsHugeElementSize(t *testing.T) {
	tests := []struct {
		name string
		spec webmFixtureSpec
	}{
		{
			name: "huge timestamp size",
			spec: webmFixtureSpec{
				hugeTimestampSize: true,
				clusters: []webmClusterSpec{{
					timeTicks: 0,
					blocks:    []webmBlockSpec{{track: 1, flags: 0x80, blockBody: []byte{0x11, 0x22}}},
				}},
			},
		},
		{
			name: "huge simpleblock size",
			spec: webmFixtureSpec{
				hugeBlockSize: true,
				clusters: []webmClusterSpec{{
					timeTicks: 0,
					blocks:    []webmBlockSpec{{track: 1, flags: 0x80, blockBody: []byte{0x11, 0x22}}},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeWebM(t, "huge.webm", buildWebM(tt.spec))
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()

			// Opening is allowed to succeed for a corrupt cluster (the index is
			// built from headers), so the read path must also be panic-free.
			r, err := newWebMOpusReader(f)
			if err != nil {
				return
			}
			for range 8 {
				if _, _, err := r.ReadPacket(); err != nil {
					return
				}
			}
			t.Fatal("reader consumed a block with a 2^55-byte size")
		})
	}
}

// multiClusterWebM builds a three-cluster file with packetsPer blocks of one
// 20 ms packet each, so the decoded-domain granule of cluster i is i *
// packetsPer * 960. Cluster timestamps are deliberately stamped behind the
// decoded starts (one tick apart) to prove the reader indexes by the decoded
// domain, not by the millisecond timestamp grid. The packet payload byte is
// 0xA0+i so a seek result identifies its cluster.
func multiClusterWebM(packetsPer int) []byte {
	clusters := make([]webmClusterSpec, 3)
	for i := range clusters {
		var blocks []webmBlockSpec
		for j := range packetsPer {
			blocks = append(blocks, webmBlockSpec{
				track:    1,
				relTicks: int16(j * 20),
				flags:    0x80,
				// First byte is the TOC (0x98: CELT 20 ms, 960 samples); the
				// second identifies the cluster so a seek result is checkable.
				blockBody: []byte{0x98, byte(0xA0 + i)},
			})
		}
		clusters[i] = webmClusterSpec{timeTicks: uint64(i), blocks: blocks}
	}

	return buildWebM(webmFixtureSpec{noDuration: true, clusters: clusters})
}

// TestWebMOpusSeekNeverOvershootsTarget pins the lower-bound contract: after a
// seek, Position must be at or before the target. The cluster timestamps are
// stamped behind the decoded starts, so a timestamp-based seek would pick a
// cluster whose first block is past the target; the decoded-domain index must
// not.
func TestWebMOpusSeekNeverOvershootsTarget(t *testing.T) {
	const per = 100 // packets per cluster: 96000 samples, 2 s each
	body := multiClusterWebM(per)
	path := writeWebM(t, "multi.webm", body)

	// Decoded starts are 0, 96000, 192000; total is 288000.
	targets := []int64{0, 1, 95999, 96000, 100000, 191999, 192000, 200000, 288000}
	for _, target := range targets {
		r := mustWebMReader(t, path)
		if err := r.SeekGranule(target, 3840); err != nil {
			t.Fatalf("SeekGranule(%d): %v", target, err)
		}
		if r.Position() > target {
			t.Fatalf("SeekGranule(%d): Position = %d exceeds the target", target, r.Position())
		}
	}
}

// TestWebMOpusSeekSelectsCorrectCluster proves the Cues lookup actually chooses
// a cluster: seeking into each cluster must return that cluster's packet, which
// the payload byte identifies.
func TestWebMOpusSeekSelectsCorrectCluster(t *testing.T) {
	const per = 100
	body := multiClusterWebM(per)
	path := writeWebM(t, "select.webm", body)

	r := mustWebMReader(t, path)
	if len(r.clusters) != 3 {
		t.Fatalf("built %d clusters, want 3", len(r.clusters))
	}
	if len(r.cues) != 3 {
		t.Fatalf("built %d cues, want 3", len(r.cues))
	}
	for i, cl := range r.clusters {
		if want := int64(i * per * 960); cl.startGranule != want {
			t.Fatalf("cluster %d startGranule = %d, want %d", i, cl.startGranule, want)
		}
	}

	cases := []struct {
		target   int64
		wantByte byte
	}{
		{48000, 0xA0},  // 1 s: inside cluster 0
		{144000, 0xA1}, // 3 s: inside cluster 1
		{240000, 0xA2}, // 5 s: inside cluster 2
	}
	for _, tt := range cases {
		if err := r.SeekGranule(tt.target, 0); err != nil {
			t.Fatalf("SeekGranule(%d): %v", tt.target, err)
		}
		pkt, _, err := r.ReadPacket()
		if err != nil {
			t.Fatalf("ReadPacket after seek to %d: %v", tt.target, err)
		}
		if pkt[1] != tt.wantByte {
			t.Fatalf("seek to %d returned packet marker 0x%02x, want 0x%02x", tt.target, pkt[1], tt.wantByte)
		}
	}
}

// TestWebMOpusMultiClusterSeekOnRealFixture runs the seek path on the real
// 23-cluster corpus when it is present. It is the strongest seek proof but the
// file is not a checked-in fixture, so it skips when absent.
//
// It drives the decoder with a 2 s warm-up rather than the default 80 ms: on
// this content pion's codec state needs about 2 s to converge, so the fast
// window leaves a transient that outlives any convergence skip. With the long
// window the result is bit-exact, which can only happen if the reader lands on
// the right packet boundary with the right granule mapping.
func TestWebMOpusMultiClusterSeekOnRealFixture(t *testing.T) {
	path := realWebMPath(t)

	full := func() []float32 {
		d, err := NewWebMOpusFactory().Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer d.Close()

		return decodeAll(t, d)
	}()

	targets := []int64{48000, 30 * 48000, 120 * 48000}
	for _, at := range targets {
		fh, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		d, err := newWebMOpusDecoder(fh, fh, 96000, true)
		if err != nil {
			fh.Close()
			t.Fatalf("decode: %v", err)
		}
		if err := d.SeekFrame(at); err != nil {
			d.Close()
			t.Fatalf("SeekFrame(%d): %v", at, err)
		}
		got := readExactlyFrames(t, d, 48000)
		d.Close()

		want := full[at*2 : (at+48000)*2]
		if !bytes.Equal(float32sToBytes(want), float32sToBytes(got)) {
			t.Fatalf("real multi-cluster seek to %d is not bit-exact against a straight decode", at)
		}
	}
}

// realWebMPath resolves the real YouTube itag-251 WebM used for end-to-end
// checks, if it is present.
func realWebMPath(t *testing.T) string {
	t.Helper()

	for _, p := range []string{"/tmp/opencode/sample251.webm"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	t.Skip("real WebM corpus not present")

	return ""
}

// TestWebMOpusDiscardPaddingChangesTotal proves DiscardPadding affects the total,
// not just the recorded field: a positive padding shortens the last block, a
// negative one lengthens it. The block holds one 2.5 ms packet (120 samples), so
// a +1 ms padding trims 48 samples and a -1 ms padding adds 48.
func TestWebMOpusDiscardPaddingChangesTotal(t *testing.T) {
	build := func(padNs int64) []byte {
		return buildWebM(webmFixtureSpec{
			noDuration: true,
			clusters: []webmClusterSpec{{
				timeTicks: 0,
				blocks: []webmBlockSpec{{
					track:     1,
					relTicks:  20,
					flags:     0x80,
					blockBody: opusPacket2p5ms(),
					group:     true,
					padNs:     padNs,
				}},
			}},
		})
	}

	plain := mustWebMReader(t, writeWebM(t, "pad-none.webm", build(0))).TotalGranule()
	positive := mustWebMReader(t, writeWebM(t, "pad-pos.webm", build(1_000_000))).TotalGranule()
	negative := mustWebMReader(t, writeWebM(t, "pad-neg.webm", build(-1_000_000))).TotalGranule()

	if plain != 120 {
		t.Fatalf("unpadded TotalGranule = %d, want 120", plain)
	}
	if positive != 72 {
		t.Fatalf("positive padding TotalGranule = %d, want 72", positive)
	}
	if negative != 168 {
		t.Fatalf("negative padding TotalGranule = %d, want 168 (padding adds)", negative)
	}
}

// TestWebMOpusSkipsUnknownSizeVoid is the truncation regression: an unknown-size
// Void at level 1 cannot be skipped, so the reader must error rather than drop
// every cluster after it.
func TestWebMOpusSkipsUnknownSizeVoid(t *testing.T) {
	body := buildWebM(webmFixtureSpec{
		unknownVoid: true,
		clusters: []webmClusterSpec{{
			timeTicks: 0,
			blocks:    []webmBlockSpec{{track: 1, flags: 0x80, blockBody: []byte{0xAA, 0xBB}}},
		}},
	})
	f, err := os.Open(writeWebM(t, "unkvoid.webm", body))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if _, err := newWebMOpusReader(f); err == nil {
		t.Fatal("reader silently accepted an unknown-size Void and dropped its clusters")
	}
}

// TestWebMOpusLacingNegativeDeltaAndEmptyFrames covers the two lacing edge
// cases the reviewer flagged: an EBML delta that makes a frame smaller than the
// previous one, and a lacing layout that would leave a zero-length packet.
func TestWebMOpusLacingNegativeDeltaAndEmptyFrames(t *testing.T) {
	p1 := bytes.Repeat([]byte{0x11}, 9)
	p2 := bytes.Repeat([]byte{0x22}, 4)
	p3 := bytes.Repeat([]byte{0x33}, 6)

	// EBML lacing, three frames: first 9, delta -5 gives 4, last is remainder 6.
	body := concat([]byte{2}, ebmlSize(uint64(len(p1))), signedVint(int64(len(p2)-len(p1))), p1, p2, p3)
	file := buildWebM(webmFixtureSpec{
		clusters: []webmClusterSpec{{
			timeTicks: 0,
			blocks:    []webmBlockSpec{{track: 1, flags: 3 << 1, blockBody: body}},
		}},
	})
	r := mustWebMReader(t, writeWebM(t, "negdelta.webm", file))
	for i, want := range [][]byte{p1, p2, p3} {
		pkt, _, err := r.ReadPacket()
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if !bytes.Equal(pkt, want) {
			t.Fatalf("packet %d = %d bytes, want %d", i, len(pkt), len(want))
		}
	}

	// A Xiph block whose leading sizes consume the whole body leaves a
	// zero-length final packet, which must be rejected rather than handed to the
	// decoder. The block is the only one, so the error surfaces at open.
	empty := concat([]byte{1, byte(len(p1))}, p1)
	bad := buildWebM(webmFixtureSpec{
		clusters: []webmClusterSpec{{
			timeTicks: 0,
			blocks:    []webmBlockSpec{{track: 1, flags: 1 << 1, blockBody: empty}},
		}},
	})
	bf, err := os.Open(writeWebM(t, "emptyframe.webm", bad))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer bf.Close()
	if _, err := newWebMOpusReader(bf); err == nil {
		t.Fatal("reader accepted a zero-length laced frame")
	}
}

// TestWebMOpusForwardRejectsReservedID aligns the forward reader with the
// seekable one: the reserved 1-byte ID 0xFF must be rejected, not parsed.
func TestWebMOpusForwardRejectsReservedID(t *testing.T) {
	f, err := os.Open(writeWebM(t, "reserved.webm", buildWebM(webmFixtureSpec{
		clusters: []webmClusterSpec{{timeTicks: 0, blocks: []webmBlockSpec{{track: 1, flags: 0x80, blockBody: []byte{0x11}}}}},
	})))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	_, err = newForwardWebMOpus(oneShotReader{f})
	if err != nil {
		t.Fatalf("forward reader rejected a valid file: %v", err)
	}

	// A stream whose second element uses the reserved 0xFF ID must error.
	bad := concat(
		ebmlElem(idEBML, concat(ebmlStringElem(idDocType, "webm"), ebmlUintElem(idEBMLMaxIDLen, 4))),
		[]byte{0xFF, 0x01},
	)
	f2, err := os.Open(writeFile(t, t.TempDir(), "resid.webm", bad))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f2.Close()
	if _, err := newForwardWebMOpus(oneShotReader{f2}); err == nil {
		t.Fatal("forward reader accepted the reserved ID 0xFF")
	}
}

// --- helpers -------------------------------------------------------

type webmPacketRead struct {
	data []byte
	gran int64
}

// decodeWebMFixture decodes a WebM fixture through the WebM factory.
func decodeWebMFixture(t *testing.T, name string) []float32 {
	t.Helper()

	d, err := NewWebMOpusFactory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	defer d.Close()

	return decodeAll(t, d)
}

func readWebMPackets(t *testing.T, r *webMOpusReader) []webmPacketRead {
	t.Helper()

	var out []webmPacketRead
	for {
		pkt, g, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		out = append(out, webmPacketRead{data: append([]byte(nil), pkt...), gran: g})
	}
}
