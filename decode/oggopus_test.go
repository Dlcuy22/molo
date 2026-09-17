package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// --- synthetic construction helpers --------------------------------

// rawOggPage assembles a page from explicit lacing values and a body, with a
// correct checksum. oggPage cannot express a packet that spans a page boundary
// because it always terminates a packet, so continued-packet tests build pages
// here instead.
func rawOggPage(granule int64, flags byte, seq uint32, lacing []byte, body []byte) []byte {
	page := make([]byte, oggHeaderLen+len(lacing)+len(body))
	copy(page, oggCapture)
	page[4] = 0
	page[5] = flags
	binary.LittleEndian.PutUint64(page[oggGranuleOff:oggGranuleOff+oggGranuleSize], uint64(granule))
	binary.LittleEndian.PutUint32(page[18:22], seq)
	page[oggSegmentOff] = byte(len(lacing))
	copy(page[oggHeaderLen:], lacing)
	copy(page[oggHeaderLen+len(lacing):], body)

	binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))

	return page
}

// opusHeadWithGain is opusHeadPacket with the signed Q7.8 output gain patched.
func opusHeadWithGain(preSkip uint16, channels byte, gain int16) []byte {
	p := opusHeadPacket(preSkip, channels)
	binary.LittleEndian.PutUint16(p[16:18], uint16(gain))

	return p
}

// opusHeadFamily1 builds an ID header with a channel mapping table for family
// 1, which the reader deliberately does not support.
func opusHeadFamily1(preSkip uint16, channels byte) []byte {
	p := make([]byte, 21+int(channels))
	copy(p, "OpusHead")
	p[8] = 1
	p[9] = channels
	binary.LittleEndian.PutUint16(p[10:12], preSkip)
	binary.LittleEndian.PutUint32(p[12:16], 48000)
	p[18] = 1
	p[19] = 1
	p[20] = channels - 1
	for i := range int(channels) {
		p[21+i] = byte(i)
	}

	return p
}

// --- reader helpers ------------------------------------------------

type oggPacketRead struct {
	data    []byte
	granule int64
}

func mustNewOggOpusReader(t *testing.T, path string) *OggOpusReader {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })

	r, err := NewOggOpusReader(f)
	if err != nil {
		t.Fatalf("NewOggOpusReader(%s): %v", path, err)
	}

	return r
}

func readOggOpusPackets(t *testing.T, r *OggOpusReader) []oggPacketRead {
	t.Helper()

	var out []oggPacketRead
	for {
		pkt, granule, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		out = append(out, oggPacketRead{data: append([]byte(nil), pkt...), granule: granule})
	}
}

func linearOggOpusRead(t *testing.T, path string) []oggPacketRead {
	t.Helper()

	return readOggOpusPackets(t, mustNewOggOpusReader(t, path))
}

// realOpusPath resolves the 139 s reference file at the repo root. It is not a
// checked-in fixture, so tests that need it skip rather than fail when absent.
func realOpusPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join("..", "チェリーポップ - DECO_27.opus")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("reference file not present: %v", err)
	}

	return path
}

// fixtureOpusHead hand-parses the ID header straight from the bytes, giving the
// reader an independent expectation to match.
func fixtureOpusHead(t *testing.T, path string) (preSkip int, gain int16, channels int) {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	off := oggHeaderLen + int(src[oggSegmentOff])
	if string(src[off:off+8]) != "OpusHead" {
		t.Fatalf("%s does not start with OpusHead", path)
	}

	return int(binary.LittleEndian.Uint16(src[off+10 : off+12])),
		int16(binary.LittleEndian.Uint16(src[off+16 : off+18])),
		int(src[off+9])
}

// --- properties ----------------------------------------------------

func TestOggOpusReaderProperties(t *testing.T) {
	path := writeOgg(t, "props.opus",
		oggPage(0, 0x02, 0, opusHeadWithGain(312, 2, -1536)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		oggPage(960, 0x00, 2, bytes.Repeat([]byte{0x5a}, 64)),
	)

	r := mustNewOggOpusReader(t, path)
	if got := r.PreSkip(); got != 312 {
		t.Fatalf("PreSkip = %d, want 312", got)
	}
	if got := r.OutputGainQ78(); got != -1536 {
		t.Fatalf("OutputGainQ78 = %d, want -1536 (a signed value)", got)
	}
	if got := r.Channels(); got != 2 {
		t.Fatalf("Channels = %d, want 2", got)
	}
	if got := r.SampleRate(); got != 48000 {
		t.Fatalf("SampleRate = %d, want 48000", got)
	}
	if got := r.TotalGranule(); got != 960 {
		t.Fatalf("TotalGranule = %d, want 960", got)
	}
}

func TestOggOpusReaderPropertiesOnFixtures(t *testing.T) {
	for _, name := range []string{"stereo_2s.opus", "mono_1s.opus", "short_stereo.opus"} {
		t.Run(name, func(t *testing.T) {
			path := fixturePath(t, name)
			wantPreSkip, wantGain, wantCh := fixtureOpusHead(t, path)

			r := mustNewOggOpusReader(t, path)
			if got := r.PreSkip(); got != wantPreSkip {
				t.Fatalf("PreSkip = %d, want %d", got, wantPreSkip)
			}
			if got := r.OutputGainQ78(); got != wantGain {
				t.Fatalf("OutputGainQ78 = %d, want %d", got, wantGain)
			}
			if got := r.Channels(); got != wantCh {
				t.Fatalf("Channels = %d, want %d", got, wantCh)
			}
		})
	}
}

func TestOggOpusReaderGainIsSignedQ78(t *testing.T) {
	cases := []int16{0, 256, -256, 32767, -32768}
	for _, gain := range cases {
		t.Run(fmt.Sprintf("%d", gain), func(t *testing.T) {
			path := writeOgg(t, "gain.opus",
				oggPage(0, 0x02, 0, opusHeadWithGain(312, 1, gain)),
				oggPage(0, 0x00, 1, []byte("OpusTags")),
				oggPage(480, 0x00, 2, []byte("a")),
			)

			if got := mustNewOggOpusReader(t, path).OutputGainQ78(); got != gain {
				t.Fatalf("OutputGainQ78 = %d, want %d", got, gain)
			}
		})
	}
}

func TestOggOpusReaderGranuleIncludesPreSkip(t *testing.T) {
	// RFC 7845 section 4.3: playable sample = granule - pre-skip. The reader
	// exposes both so a caller can convert a playable frame to a seek target.
	const (
		preSkip = 312
		granule = 96000
	)
	path := writeOgg(t, "granule.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(preSkip, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		oggPage(granule, 0x04, 2, bytes.Repeat([]byte{1}, 200)),
	)

	r := mustNewOggOpusReader(t, path)
	if got := r.TotalGranule() - int64(r.PreSkip()); got != granule-preSkip {
		t.Fatalf("playable length = %d, want %d", got, granule-preSkip)
	}
	if err := r.SeekGranule(granule, 0); err != nil {
		t.Fatalf("SeekGranule(pre-skip adjusted target): %v", err)
	}
	if _, g, err := r.ReadPacket(); err != nil || g != granule {
		t.Fatalf("packet granule = %d, err = %v, want %d", g, err, granule)
	}
}

// --- rejection -----------------------------------------------------

func TestOggOpusReaderRejectsNonOpus(t *testing.T) {
	notOgg := writeFile(t, t.TempDir(), "plain.opus", []byte("this is not an Ogg stream"))

	vorbis := writeOgg(t, "vorbis.opus", oggPage(0, 0x02, 0, []byte("\x01vorbis payload")))
	short := writeOgg(t, "shortopus.opus", oggPage(0, 0x02, 0, []byte("Opus")))

	for _, path := range []string{vorbis, short} {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		_, err = NewOggOpusReader(f)
		f.Close()
		if !errors.Is(err, ErrOggOpusNotOpus) {
			t.Fatalf("%s: err = %v, want ErrOggOpusNotOpus", filepath.Base(path), err)
		}
	}

	f, err := os.Open(notOgg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := NewOggOpusReader(f); err == nil {
		t.Fatal("reader accepted a file that is not Ogg at all")
	}
}

func TestOggOpusReaderRejectsUnsupportedMappingFamily(t *testing.T) {
	path := writeOgg(t, "family1.opus",
		oggPage(0, 0x02, 0, opusHeadFamily1(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
	)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if _, err := NewOggOpusReader(f); !errors.Is(err, ErrOggOpusUnsupportedMapping) {
		t.Fatalf("err = %v, want ErrOggOpusUnsupportedMapping", err)
	}
}

// --- packet reassembly ---------------------------------------------

func TestOggOpusReaderReassemblesPacketOver255Bytes(t *testing.T) {
	// oggPage splits a >= 255 byte packet into 255-byte lacing runs, so this
	// exercises reassembly from several lacing values on one page.
	big := make([]byte, 700)
	for i := range big {
		big[i] = byte(i)
	}
	small := []byte("tail")
	path := writeOgg(t, "big.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		oggPage(960, 0x04, 2, big, small),
	)

	r := mustNewOggOpusReader(t, path)
	pkt, granule, err := r.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if !bytes.Equal(pkt, big) {
		t.Fatalf("packet = %d bytes, want the %d-byte input equal", len(pkt), len(big))
	}
	if granule != 960 {
		t.Fatalf("granule = %d, want 960", granule)
	}
	pkt, _, err = r.ReadPacket()
	if err != nil || !bytes.Equal(pkt, small) {
		t.Fatalf("second packet = %q, err = %v, want %q", pkt, err, small)
	}
}

func TestOggOpusReaderReassemblesPacketAcrossPages(t *testing.T) {
	head := bytes.Repeat([]byte{0x11}, 510)
	tail := bytes.Repeat([]byte{0x22}, 90)

	// Page A ends mid-packet, so its granule is the special -1 value.
	// Page B continues it and completes it, then carries one more packet.
	pageA := rawOggPage(-1, 0x00, 2, []byte{255, 255}, head)
	pageB := rawOggPage(960, 0x01, 3, []byte{90, 5}, append(append([]byte(nil), tail...), []byte("extra")...))
	path := writeOgg(t, "continued.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		pageA, pageB,
	)

	r := mustNewOggOpusReader(t, path)

	pkt, granule, err := r.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if want := append(append([]byte(nil), head...), tail...); !bytes.Equal(pkt, want) {
		t.Fatalf("continued packet = %d bytes, want %d", len(pkt), len(want))
	}
	if granule != 960 {
		t.Fatalf("continued packet granule = %d, want 960 (page B)", granule)
	}

	pkt, granule, err = r.ReadPacket()
	if err != nil || string(pkt) != "extra" || granule != 960 {
		t.Fatalf("second packet = %q granule=%d err=%v", pkt, granule, err)
	}
	if _, _, err := r.ReadPacket(); !errors.Is(err, io.EOF) {
		t.Fatalf("end err = %v, want io.EOF", err)
	}
}

func TestOggOpusReaderSkipsCommentHeaderAcrossPages(t *testing.T) {
	// A comment header can span many pages (the reference file spends 280).
	// This exercises the lacing-only skip: the fast path must not confuse tag
	// bytes with audio.
	tag := append([]byte("OpusTags"), bytes.Repeat([]byte("x"), 532)...)
	tagPageA := rawOggPage(0, 0x00, 1, []byte{255, 255}, tag[:510])
	tagPageB := rawOggPage(0, 0x01, 2, []byte{byte(len(tag) - 510)}, tag[510:])
	audio := []byte("audio-packet")
	path := writeOgg(t, "spantags.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		tagPageA, tagPageB,
		oggPage(960, 0x00, 3, audio),
	)

	r := mustNewOggOpusReader(t, path)
	if got := r.TotalGranule(); got != 960 {
		t.Fatalf("TotalGranule = %d, want 960", got)
	}
	if len(r.index) != 1 || r.index[0].granule != 960 {
		t.Fatalf("index = %+v, want one entry at granule 960", r.index)
	}
	pkt, granule, err := r.ReadPacket()
	if err != nil || !bytes.Equal(pkt, audio) || granule != 960 {
		t.Fatalf("first audio packet = %q granule=%d err=%v", pkt, granule, err)
	}
	if _, _, err := r.ReadPacket(); !errors.Is(err, io.EOF) {
		t.Fatalf("end err = %v, want io.EOF", err)
	}
}

func TestOggOpusChecksumMatchesBitwiseReference(t *testing.T) {
	// oggCRC in ogg_probe_test.go is a separate bit-by-bit implementation, so
	// agreement rules out a shared table bug.
	page := rawOggPage(12345, 0x01, 7, []byte{255, 255, 10}, bytes.Repeat([]byte{0x5a}, 520))
	zeroed := append([]byte(nil), page...)
	for i := 22; i < 26; i++ {
		zeroed[i] = 0
	}

	want := oggCRC(zeroed)
	if got := oggOpusChecksum(zeroed); got != want {
		t.Fatalf("oggOpusChecksum = %08x, want %08x", got, want)
	}
	if stored := binary.LittleEndian.Uint32(page[22:26]); stored != want {
		t.Fatalf("page checksum field = %08x, want %08x", stored, want)
	}
}

func TestOggOpusReaderWithoutCommentHeader(t *testing.T) {
	// A missing OpusTags is non-conformant but playable; the second packet must
	// be handed back as audio rather than dropped.
	audio := []byte("audio-without-tags")
	path := writeOgg(t, "notags.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(960, 0x04, 1, audio),
	)

	r := mustNewOggOpusReader(t, path)
	if got := r.TotalGranule(); got != 960 {
		t.Fatalf("TotalGranule = %d, want 960", got)
	}
	pkt, granule, err := r.ReadPacket()
	if err != nil || !bytes.Equal(pkt, audio) || granule != 960 {
		t.Fatalf("first packet = %q granule=%d err=%v, want the audio packet", pkt, granule, err)
	}
}

func TestOggOpusReaderCommentHeaderSharesHeadPage(t *testing.T) {
	// When OpusHead and OpusTags share a page the lacing-only fast path cannot
	// be used, so it must fall back and still find the first audio packet.
	audio := []byte("audio-after-shared-head")
	path := writeOgg(t, "sharedhead.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2), []byte("OpusTags")),
		oggPage(960, 0x04, 1, audio),
	)

	r := mustNewOggOpusReader(t, path)
	pkt, granule, err := r.ReadPacket()
	if err != nil || !bytes.Equal(pkt, audio) || granule != 960 {
		t.Fatalf("first packet = %q granule=%d err=%v, want the audio packet", pkt, granule, err)
	}
}

func TestOggOpusReaderHeaderOnlyStream(t *testing.T) {
	path := writeOgg(t, "headers.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
	)

	r := mustNewOggOpusReader(t, path)
	if got := r.TotalGranule(); got != -1 {
		t.Fatalf("TotalGranule = %d, want -1 for a stream with no audio", got)
	}
	if _, _, err := r.ReadPacket(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadPacket err = %v, want io.EOF", err)
	}
	if err := r.SeekGranule(0, 0); err == nil {
		t.Fatal("SeekGranule succeeded on a stream with no audio pages")
	}
}

func TestOggOpusReaderSeekOnSyntheticPages(t *testing.T) {
	// Five single-packet pages with known granules, independent of the
	// reference file, so the seek page choice is pinned by exact numbers.
	path := writeOgg(t, "seekpages.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		oggPage(960, 0x00, 2, []byte("p1")),
		oggPage(1920, 0x00, 3, []byte("p2")),
		oggPage(2880, 0x00, 4, []byte("p3")),
		oggPage(3840, 0x00, 5, []byte("p4")),
		oggPage(4800, 0x04, 6, []byte("p5")),
	)
	linear := linearOggOpusRead(t, path)
	if len(linear) != 5 {
		t.Fatalf("linear read has %d packets, want 5", len(linear))
	}

	r := mustNewOggOpusReader(t, path)
	tests := []struct {
		target  int64
		preroll int64
		want    int64
	}{
		{0, 0, 960},
		{960, 0, 960},
		{1000, 0, 960},
		{2880, 0, 2880},
		{3000, 0, 2880},
		{3000, 3840, 960},
		{4799, 3840, 960},
		{4800, 0, 4800},
	}
	for _, tt := range tests {
		if err := r.SeekGranule(tt.target, tt.preroll); err != nil {
			t.Fatalf("SeekGranule(%d, %d): %v", tt.target, tt.preroll, err)
		}
		pkt, granule, err := r.ReadPacket()
		if err != nil {
			t.Fatalf("SeekGranule(%d, %d): ReadPacket: %v", tt.target, tt.preroll, err)
		}
		if granule != tt.want {
			t.Fatalf("SeekGranule(%d, %d): first granule = %d, want %d", tt.target, tt.preroll, granule, tt.want)
		}
		if string(pkt) != fmt.Sprintf("p%d", tt.want/960) {
			t.Fatalf("SeekGranule(%d, %d): first packet = %q", tt.target, tt.preroll, pkt)
		}
	}
}

func TestOggOpusReaderSeekSkipsIncompleteLeadingPacket(t *testing.T) {
	// Page Y begins with the tail of a packet whose start was on page X, so a
	// seek to page Y must skip it (RFC 7845 section 3) and return the first
	// packet that is complete there.
	packetA := append(bytes.Repeat([]byte{0x11}, 255), bytes.Repeat([]byte{0x22}, 10)...)
	pageX := rawOggPage(-1, 0x00, 2, []byte{255}, bytes.Repeat([]byte{0x11}, 255))
	pageY := rawOggPage(960, 0x01, 3, []byte{10, 8}, append(bytes.Repeat([]byte{0x22}, 10), []byte("packet-b")...))
	pageZ := rawOggPage(1920, 0x00, 4, []byte{8}, []byte("packet-c"))
	path := writeOgg(t, "joinmid.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		pageX, pageY, pageZ,
	)

	// A linear read reassembles the continued packet.
	linear := linearOggOpusRead(t, path)
	if len(linear) != 3 || !bytes.Equal(linear[0].data, packetA) {
		t.Fatalf("linear read = %d packets, first = %d bytes, want %d", len(linear), len(linear[0].data), len(packetA))
	}

	r := mustNewOggOpusReader(t, path)
	if err := r.SeekGranule(960, 0); err != nil {
		t.Fatalf("SeekGranule(960): %v", err)
	}
	pkt, granule, err := r.ReadPacket()
	if err != nil || string(pkt) != "packet-b" || granule != 960 {
		t.Fatalf("after mid-packet seek: packet = %q granule = %d err = %v", pkt, granule, err)
	}
	if pkt, granule, err = r.ReadPacket(); err != nil || string(pkt) != "packet-c" || granule != 1920 {
		t.Fatalf("next packet = %q granule = %d err = %v", pkt, granule, err)
	}
}

func TestOggOpusReaderRejectsDroppedContinuation(t *testing.T) {
	// A page ending with a 255 lacing value must be continued by the next page;
	// one that is not is malformed framing.
	path := writeOgg(t, "dropped.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		rawOggPage(-1, 0x00, 2, []byte{255}, bytes.Repeat([]byte{0x33}, 255)),
		rawOggPage(960, 0x00, 3, []byte{4}, []byte("tail")),
	)

	r := mustNewOggOpusReader(t, path)
	if _, _, err := r.ReadPacket(); !errors.Is(err, ErrOggOpusBadPage) {
		t.Fatalf("err = %v, want ErrOggOpusBadPage", err)
	}
}

func TestOggOpusReaderFixturesCarryLargePackets(t *testing.T) {
	tests := []struct {
		fixture     string
		wantPackets int
		wantTotal   int64
	}{
		{"stereo_2s.opus", 101, 96312},
		{"mono_1s.opus", 51, 48312},
		{"short_stereo.opus", 13, 12312},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			r := mustNewOggOpusReader(t, fixturePath(t, tt.fixture))
			pkts := readOggOpusPackets(t, r)

			if len(pkts) != tt.wantPackets {
				t.Fatalf("read %d audio packets, want %d", len(pkts), tt.wantPackets)
			}
			if r.TotalGranule() != tt.wantTotal {
				t.Fatalf("TotalGranule = %d, want %d", r.TotalGranule(), tt.wantTotal)
			}
			over255 := 0
			for _, p := range pkts {
				if bytes.HasPrefix(p.data, opusTagsTag) {
					t.Fatalf("OpusTags leaked into the audio packets")
				}
				if len(p.data) > 255 {
					over255++
				}
			}
			if over255 == 0 {
				t.Fatal("no packet exceeded 255 bytes, so lacing reassembly was not exercised")
			}
		})
	}
}

// --- granule indexing ----------------------------------------------

func TestOggOpusReaderIndexIsMonotonicAndSkipsMinusOne(t *testing.T) {
	// Two synthetic pages with granule -1 bracket a completed packet so the
	// index has to skip the spanned pages and keep only the completing one.
	pageA := rawOggPage(-1, 0x00, 2, []byte{255}, bytes.Repeat([]byte{1}, 255))
	pageB := rawOggPage(-1, 0x01, 3, []byte{255}, bytes.Repeat([]byte{2}, 255))
	// Malformed: a -1 granule with a completed packet. It must still not be
	// treated as a position.
	pageD := rawOggPage(-1, 0x00, 4, []byte{20}, bytes.Repeat([]byte{4}, 20))
	pageC := rawOggPage(1000, 0x01, 5, []byte{100}, bytes.Repeat([]byte{3}, 100))
	path := writeOgg(t, "minusone.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		pageA, pageB, pageD, pageC,
	)

	r := mustNewOggOpusReader(t, path)
	if len(r.index) != 1 {
		t.Fatalf("index has %d entries, want 1 (only the completing page)", len(r.index))
	}
	if r.index[0].granule != 1000 {
		t.Fatalf("index[0].granule = %d, want 1000", r.index[0].granule)
	}
	if r.index[0].start != 0 {
		t.Fatalf("index[0].start = %d, want 0", r.index[0].start)
	}
	for _, e := range r.index {
		if e.granule < 0 {
			t.Fatalf("index holds a -1 granule: %+v", e)
		}
	}
}

func TestOggOpusReaderIndexOnReferenceFile(t *testing.T) {
	path := realOpusPath(t)
	r := mustNewOggOpusReader(t, path)

	// The reference file spends pages 1..279 on an oversized OpusTags packet
	// and has 139 audio pages after it, 138 of them with a real granule.
	if len(r.index) != 139 {
		t.Fatalf("index has %d entries, want 139", len(r.index))
	}
	if r.TotalGranule() != 6671040 {
		t.Fatalf("TotalGranule = %d, want 6671040", r.TotalGranule())
	}
	prev := int64(-1)
	for i, e := range r.index {
		if e.granule < 0 {
			t.Fatalf("index[%d] has granule -1", i)
		}
		if e.granule <= prev {
			t.Fatalf("index[%d].granule = %d, not increasing past %d", i, e.granule, prev)
		}
		if e.start > e.granule {
			t.Fatalf("index[%d].start = %d exceeds granule %d", i, e.start, e.granule)
		}
		prev = e.granule
	}
}

// --- seeking -------------------------------------------------------

func TestOggOpusReaderSeekMatchesLinearRead(t *testing.T) {
	path := realOpusPath(t)
	linear := linearOggOpusRead(t, path)
	r := mustNewOggOpusReader(t, path)

	targets := []int64{0, 5000, 48000, 96000, 250000, 480000, 960000, 2000000, 6000000}
	for _, preroll := range []int64{0, 3840} {
		for _, target := range targets {
			if target > r.TotalGranule() {
				continue
			}
			if err := r.SeekGranule(target, preroll); err != nil {
				t.Fatalf("SeekGranule(%d, %d): %v", target, preroll, err)
			}

			entry := r.index[expectedSeekIndex(r.index, target, preroll)]
			got := readN(t, r, 12)
			if len(got) == 0 {
				t.Fatalf("SeekGranule(%d, %d) produced no packets", target, preroll)
			}
			if got[0].granule != entry.granule {
				t.Fatalf("SeekGranule(%d, %d): first packet granule = %d, want page %d",
					target, preroll, got[0].granule, entry.granule)
			}

			at := -1
			for i := range linear {
				if linear[i].granule == got[0].granule && bytes.Equal(linear[i].data, got[0].data) {
					at = i

					break
				}
			}
			if at < 0 {
				t.Fatalf("SeekGranule(%d, %d): first packet not found in a linear read", target, preroll)
			}
			if at+len(got) > len(linear) {
				t.Fatalf("SeekGranule(%d, %d): linear read too short to compare", target, preroll)
			}
			for k := range got {
				if got[k].granule != linear[at+k].granule || !bytes.Equal(got[k].data, linear[at+k].data) {
					t.Fatalf("SeekGranule(%d, %d): packet %d differs from the linear read", target, preroll, k)
				}
			}
		}
	}
}

// expectedSeekIndex mirrors the documented rule: the last page with a granule
// at or before target-preroll, falling back to the first page when the target
// is earlier than the stream's first granule.
func expectedSeekIndex(index []oggOpusPageEntry, target, preroll int64) int {
	want := target - preroll
	if want < 0 {
		want = 0
	}
	best := 0
	for i, e := range index {
		if e.granule <= want {
			best = i
		} else {
			break
		}
	}

	return best
}

func readN(t *testing.T, r *OggOpusReader, n int) []oggPacketRead {
	t.Helper()

	var out []oggPacketRead
	for range n {
		pkt, granule, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		out = append(out, oggPacketRead{data: append([]byte(nil), pkt...), granule: granule})
	}

	return out
}

func TestOggOpusReaderSeekBoundaries(t *testing.T) {
	path := realOpusPath(t)
	linear := linearOggOpusRead(t, path)
	r := mustNewOggOpusReader(t, path)
	last := r.TotalGranule()

	if err := r.SeekGranule(0, 0); err != nil {
		t.Fatalf("SeekGranule(0): %v", err)
	}
	if got := readN(t, r, 1); len(got) != 1 || !bytes.Equal(got[0].data, linear[0].data) {
		t.Fatal("SeekGranule(0) did not return the first packet")
	}

	// Reading forward then seeking backwards must restore an earlier packet.
	readN(t, r, 200)
	if err := r.SeekGranule(0, 0); err != nil {
		t.Fatalf("backwards SeekGranule(0): %v", err)
	}
	if got := readN(t, r, 1); len(got) != 1 || !bytes.Equal(got[0].data, linear[0].data) {
		t.Fatal("backwards seek did not restore the stream head")
	}

	// target == lastGranule is valid and lands on the final audio page.
	if err := r.SeekGranule(last, 0); err != nil {
		t.Fatalf("SeekGranule(lastGranule): %v", err)
	}
	if got := readN(t, r, 1); len(got) != 1 || got[0].granule != last {
		t.Fatalf("SeekGranule(lastGranule) first packet granule = %v, want %d", got, last)
	}

	if err := r.SeekGranule(last+1, 0); err == nil {
		t.Fatal("SeekGranule past the end returned nil")
	}
	if err := r.SeekGranule(-1, 0); err == nil {
		t.Fatal("SeekGranule(-1) returned nil")
	}
}

func TestOggOpusReaderPositionTracksGranule(t *testing.T) {
	path := writeOgg(t, "position.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		oggPage(960, 0x00, 2, []byte("first")),
		oggPage(1920, 0x00, 3, []byte("second")),
	)

	r := mustNewOggOpusReader(t, path)
	if got := r.Position(); got != 0 {
		t.Fatalf("Position before any read = %d, want 0", got)
	}
	if _, _, err := r.ReadPacket(); err != nil {
		t.Fatalf("ReadPacket first: %v", err)
	}
	if _, _, err := r.ReadPacket(); err != nil {
		t.Fatalf("ReadPacket second: %v", err)
	}
	if got := r.Position(); got != 960 {
		t.Fatalf("Position = %d, want 960 (end of the first page)", got)
	}
}

// --- integrity and lifetime ----------------------------------------

func TestOggOpusReaderReportsChecksumMismatch(t *testing.T) {
	// New only reads page headers past the mandatory headers, so a checksum
	// error in an audio page surfaces when that page is consumed.
	page := oggPage(960, 0x00, 2, bytes.Repeat([]byte{7}, 300))
	page[len(page)-1] ^= 0xff
	path := writeOgg(t, "corrupt.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		page,
	)

	r := mustNewOggOpusReader(t, path)
	if _, _, err := r.ReadPacket(); !errors.Is(err, ErrOggOpusChecksum) {
		t.Fatalf("err = %v, want ErrOggOpusChecksum", err)
	}
}

func TestOggOpusReaderRejectsCorruptHeadPage(t *testing.T) {
	page := oggPage(0, 0x02, 0, opusHeadPacket(312, 2))
	page[len(page)-1] ^= 0xff
	path := writeOgg(t, "corrupthead.opus", page)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if _, err := NewOggOpusReader(f); !errors.Is(err, ErrOggOpusChecksum) {
		t.Fatalf("err = %v, want ErrOggOpusChecksum", err)
	}
}

func TestOggOpusReaderTruncatedFileReportsUnknownTotal(t *testing.T) {
	src, err := os.ReadFile(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := writeFile(t, t.TempDir(), "truncated.opus", src[:len(src)-64])

	r := mustNewOggOpusReader(t, path)
	if got := r.TotalGranule(); got != -1 {
		t.Fatalf("TotalGranule = %d, want -1 for a truncated final page", got)
	}
	// The pages that did make it into the index are still seekable.
	if len(r.index) == 0 {
		t.Fatal("truncated file produced no index entries")
	}
	if err := r.SeekGranule(r.index[0].granule, 0); err != nil {
		t.Fatalf("SeekGranule on a truncated stream: %v", err)
	}
}

func TestOggOpusReaderDoesNotOwnOrLeakInput(t *testing.T) {
	before := runtime.NumGoroutine()

	for range 3 {
		f, err := os.Open(fixturePath(t, "short_stereo.opus"))
		if err != nil {
			t.Fatalf("open fixture: %v", err)
		}
		r, err := NewOggOpusReader(f)
		if err != nil {
			f.Close()
			t.Fatalf("NewOggOpusReader: %v", err)
		}
		readOggOpusPackets(t, r)

		// The reader was handed the file, so it must still be open and usable.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			t.Fatalf("input was closed by the reader: %v", err)
		}
		var magic [4]byte
		if _, err := io.ReadFull(f, magic[:]); err != nil {
			f.Close()
			t.Fatalf("input unreadable after the reader used it: %v", err)
		}
		f.Close()
	}

	runtime.GC()
	// Allow one background goroutine of slack so the check cannot flake on
	// runtime bookkeeping while still catching a reader that spawns workers.
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines grew from %d to %d", before, after)
	}
}

// --- seek cost -----------------------------------------------------

// TestOggOpusReaderSeekCost measures the index-plus-binary-search seek against
// the pion decode-and-discard path it replaces. Numbers are logged, not
// asserted, except that the new path must not be slower overall.
func TestOggOpusReaderSeekCost(t *testing.T) {
	path := realOpusPath(t)

	buildStart := time.Now()
	r := mustNewOggOpusReader(t, path)
	build := time.Since(buildStart)
	t.Logf("NewOggOpusReader: %v for %d indexed pages", build, len(r.index))

	d, err := NewPionOpusFactory().Open(path)
	if err != nil {
		t.Fatalf("open pion decoder: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)

	targets := []int64{10, 30, 60, 120}
	var nativeTotal, discardTotal time.Duration
	for _, seconds := range targets {
		frame := seconds * 48000
		if frame > r.TotalGranule()-int64(r.PreSkip()) {
			continue
		}

		start := time.Now()
		if err := r.SeekGranule(frame+int64(r.PreSkip()), 3840); err != nil {
			t.Fatalf("SeekGranule at %ds: %v", seconds, err)
		}
		native := time.Since(start)

		start = time.Now()
		if err := seeker.SeekFrame(frame); err != nil {
			t.Fatalf("pion SeekFrame at %ds: %v", seconds, err)
		}
		discard := time.Since(start)

		nativeTotal += native
		discardTotal += discard
		t.Logf("seek %3ds: granule-index %v, decode-and-discard %v", seconds, native, discard)
	}
	t.Logf("total over %d targets: granule-index %v, decode-and-discard %v", len(targets), nativeTotal, discardTotal)

	if discardTotal > 0 && nativeTotal >= discardTotal {
		t.Fatalf("granule-index seek (%v) was not faster than decode-and-discard (%v)", nativeTotal, discardTotal)
	}
}

func TestOggOpusReaderSeekCostOnFixtures(t *testing.T) {
	for _, name := range []string{"stereo_2s.opus", "mono_1s.opus", "short_stereo.opus"} {
		t.Run(name, func(t *testing.T) {
			path := fixturePath(t, name)

			start := time.Now()
			r := mustNewOggOpusReader(t, path)
			build := time.Since(start)

			var seek time.Duration
			for _, target := range []int64{0, 12000, 24000, 48000} {
				if target > r.TotalGranule() {
					continue
				}
				s := time.Now()
				if err := r.SeekGranule(target, 3840); err != nil {
					t.Fatalf("SeekGranule(%d): %v", target, err)
				}
				seek += time.Since(s)
			}
			t.Logf("%s: build %v, 4 seeks %v", name, build, seek)
		})
	}
}

func BenchmarkOggOpusReaderSeek(b *testing.B) {
	f, err := os.Open(filepath.Join("testdata", "stereo_2s.opus"))
	if err != nil {
		b.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	r, err := NewOggOpusReader(f)
	if err != nil {
		b.Fatalf("NewOggOpusReader: %v", err)
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := r.SeekGranule(int64(24000+i%24000), 3840); err != nil {
			b.Fatalf("SeekGranule: %v", err)
		}
	}
}
