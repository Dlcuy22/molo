package decode

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/dlcuy22/player/core"
)

// oggCRC computes the Ogg page checksum bit by bit. It is deliberately a
// different implementation from the table-driven one used by the probe, so a
// test that passes both cannot be a shared bug.
func oggCRC(page []byte) uint32 {
	var crc uint32
	for _, b := range page {
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}

	return crc
}

// oggPage builds one Ogg page carrying the given packets, with a correct
// checksum over the zeroed checksum field.
func oggPage(granule uint64, headerType byte, seq uint32, packets ...[]byte) []byte {
	var lacing, body []byte
	for _, p := range packets {
		for len(p) >= 255 {
			lacing = append(lacing, 255)
			body = append(body, p[:255]...)
			p = p[255:]
		}
		lacing = append(lacing, byte(len(p)))
		body = append(body, p...)
	}

	page := make([]byte, 27+len(lacing))
	copy(page, "OggS")
	page[4] = 0
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], 0x1234)
	binary.LittleEndian.PutUint32(page[18:22], seq)
	page[26] = byte(len(lacing))
	copy(page[27:], lacing)
	page = append(page, body...)

	binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))

	return page
}

func opusHeadPacket(preSkip uint16, channels byte) []byte {
	p := make([]byte, 19)
	copy(p, "OpusHead")
	p[8] = 1
	p[9] = channels
	binary.LittleEndian.PutUint16(p[10:12], preSkip)
	binary.LittleEndian.PutUint32(p[12:16], 48000)

	return p
}

func writeOgg(t *testing.T, name string, pages ...[]byte) string {
	t.Helper()

	var body []byte
	for _, p := range pages {
		body = append(body, p...)
	}

	return writeFile(t, t.TempDir(), name, body)
}

func TestProbeOggOpusUsesGranuleMinusPreSkip(t *testing.T) {
	tests := []struct {
		name    string
		granule uint64
		preSkip uint16
		want    int64
	}{
		{"plain", 96000, 312, 95688},
		{"zero pre-skip", 48000, 0, 48000},
		{"odd pre-skip", 12345, 7, 12338},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeOgg(t, "synthetic.opus",
				oggPage(0, 0x02, 0, opusHeadPacket(tt.preSkip, 2)),
				oggPage(tt.granule, 0x04, 1, []byte("tiny audio")),
			)

			info, err := probeOggOpus(path, core.DurationProbe, oggTailWindow)
			if err != nil {
				t.Fatalf("probeOggOpus: %v", err)
			}
			if info.TotalFrames != tt.want {
				t.Fatalf("TotalFrames = %d, want %d", info.TotalFrames, tt.want)
			}
			if info.Format != (core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}) {
				t.Fatalf("Format = %+v", info.Format)
			}
		})
	}
}

func TestProbeOggOpusScanRejectsGranuleBeforePreSkip(t *testing.T) {
	// A scan that finds granule < preSkip is corrupt data, not a negative frame
	// count. The scan must fail the same way the tail path does instead of
	// reporting a negative total. Junk on the tail forces the scan path.
	path := writeOgg(t, "badgranule.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(10, 0x04, 1, []byte("tiny audio")),
		[]byte("junk"),
	)

	if _, err := probeOggOpus(path, core.DurationScan, oggTailWindow); err == nil {
		t.Fatal("scan accepted a granule that precedes pre-skip")
	}
}

func TestProbeOggOpusOnFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    int64
	}{
		{"stereo_2s.opus", 96000},
		{"mono_1s.opus", 48000},
		{"short_stereo.opus", 12000},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			info, err := probeOggOpus(fixturePath(t, tt.fixture), core.DurationProbe, oggTailWindow)
			if err != nil {
				t.Fatalf("probeOggOpus: %v", err)
			}
			if info.TotalFrames != tt.want {
				t.Fatalf("TotalFrames = %d, want %d", info.TotalFrames, tt.want)
			}
		})
	}
}

func TestProbeOggOpusUnknownDurationSkipsTail(t *testing.T) {
	// A file whose tail cannot be parsed still probes cheaply when the caller
	// does not need a duration.
	path := writeOgg(t, "headonly.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		[]byte("garbage tail that is not an ogg page"),
	)

	info, err := probeOggOpus(path, core.DurationUnknown, oggTailWindow)
	if err != nil {
		t.Fatalf("probeOggOpus: %v", err)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1", info.TotalFrames)
	}

	// DurationProbe still parses the head and reports an unknown total rather
	// than failing the whole probe; -1 is the documented "could not tell".
	info, err = probeOggOpus(path, core.DurationProbe, oggTailWindow)
	if err != nil {
		t.Fatalf("probeOggOpus: %v", err)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1", info.TotalFrames)
	}
	if info.Format.Ch != 2 {
		t.Fatalf("Format.Ch = %d, want 2 (the decoded output contract)", info.Format.Ch)
	}
}

func TestProbeOggOpusDoesNotReadBody(t *testing.T) {
	// Corrupting everything between the head page and the final page would break
	// any decoder or full-file reader. A tail probe must still succeed, which is
	// the observable difference between "reads the tail" and "reads the file".
	src, err := os.ReadFile(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	corrupt := make([]byte, len(src))
	copy(corrupt, src)
	for i := 200; i < len(corrupt)-2000; i++ {
		corrupt[i] ^= 0xff
	}
	path := writeFile(t, t.TempDir(), "corrupt-middle.opus", corrupt)

	info, err := probeOggOpus(path, core.DurationProbe, oggTailWindow)
	if err != nil {
		t.Fatalf("probeOggOpus on corrupt-middle file: %v", err)
	}
	if info.TotalFrames != 96000 {
		t.Fatalf("TotalFrames = %d, want 96000", info.TotalFrames)
	}
}

func TestProbeOggOpusRejectsTruncatedFinalPage(t *testing.T) {
	src, err := os.ReadFile(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := writeFile(t, t.TempDir(), "truncated.opus", src[:len(src)-64])

	info, probeErr := probeOggOpus(path, core.DurationProbe, oggTailWindow)
	if probeErr != nil {
		t.Fatalf("probeOggOpus: %v", probeErr)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1 for a truncated final page", info.TotalFrames)
	}
}

func TestProbeOggOpusFallsBackWhenFinalPageOutOfWindow(t *testing.T) {
	// With a window smaller than the final page its OggS header lies outside the
	// readable range, so a correct probe reports "unknown" instead of guessing.
	path := writeOgg(t, "smallwindow.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(48000, 0x04, 1, make([]byte, 512)),
	)

	info, err := probeOggOpus(path, core.DurationProbe, 64)
	if err != nil {
		t.Fatalf("probeOggOpus: %v", err)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1 (out-of-window page must not be guessed)", info.TotalFrames)
	}

	wide, err := probeOggOpus(path, core.DurationProbe, oggTailWindow)
	if err != nil || wide.TotalFrames != 47688 {
		t.Fatalf("wide window: frames=%d err=%v, want 47688", wide.TotalFrames, err)
	}
}

func TestProbeOggOpusTrailingGarbageYieldsUnknown(t *testing.T) {
	// Bytes after the final page mean the last page no longer ends at EOF.
	// Reporting unknown is safer than reporting a stale granule.
	src, err := os.ReadFile(fixturePath(t, "short_stereo.opus"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := writeFile(t, t.TempDir(), "trailing.opus", append(src, []byte("junk")...))

	info, err := probeOggOpus(path, core.DurationProbe, oggTailWindow)
	if err != nil {
		t.Fatalf("probeOggOpus: %v", err)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1", info.TotalFrames)
	}
}

func TestProbeOggOpusScanRecoversFromDamagedTail(t *testing.T) {
	// Appending bytes invalidates the tail probe but leaves every page intact,
	// so DurationScan can still walk the pages and recover the true total.
	src, err := os.ReadFile(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := writeFile(t, t.TempDir(), "trailing.opus", append(src, []byte("junk")...))

	info, err := probeOggOpus(path, core.DurationProbe, oggTailWindow)
	if err != nil {
		t.Fatalf("probeOggOpus: %v", err)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("tail probe TotalFrames = %d, want -1", info.TotalFrames)
	}

	scanned, err := probeOggOpus(path, core.DurationScan, oggTailWindow)
	if err != nil {
		t.Fatalf("probeOggOpus scan: %v", err)
	}
	if scanned.TotalFrames != 96000 {
		t.Fatalf("scanned TotalFrames = %d, want 96000", scanned.TotalFrames)
	}
}

func TestProbeOggOpusScanMatchesTailOnHealthyFile(t *testing.T) {
	path := fixturePath(t, "stereo_2s.opus")

	tail, err := probeOggOpus(path, core.DurationProbe, oggTailWindow)
	if err != nil {
		t.Fatalf("tail probe: %v", err)
	}
	scanned, err := probeOggOpus(path, core.DurationScan, oggTailWindow)
	if err != nil {
		t.Fatalf("scan probe: %v", err)
	}
	if tail.TotalFrames != scanned.TotalFrames {
		t.Fatalf("tail=%d scan=%d, want identical", tail.TotalFrames, scanned.TotalFrames)
	}
}

func TestProbeOggOpusRejectsNonOpus(t *testing.T) {
	path := writeOgg(t, "vorbis.ogg",
		oggPage(0, 0x02, 0, []byte("\x01vorbis payload")),
	)

	if _, err := probeOggOpus(path, core.DurationProbe, oggTailWindow); err == nil {
		t.Fatal("probe accepted a Vorbis stream")
	}
}

func TestProbeOggOpusRejectsTinyAndMissingFiles(t *testing.T) {
	tiny := writeFile(t, t.TempDir(), "tiny.opus", []byte("OggS"))

	if _, err := probeOggOpus(tiny, core.DurationProbe, oggTailWindow); err == nil {
		t.Fatal("probe accepted a 4-byte file")
	}
	if _, err := probeOggOpus(filepath.Join(t.TempDir(), "absent.opus"), core.DurationProbe, oggTailWindow); err == nil {
		t.Fatal("probe accepted a missing file")
	}
}

// patchOpusHeadGain rewrites the Q7.8 dB output gain of the first page and
// repairs its checksum, so a header-only field can be exercised without an
// encoder that emits a nonzero gain.
func patchOpusHeadGain(t *testing.T, srcPath, dstName string, gainQ78 int16) string {
	t.Helper()

	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	segments := int(src[oggSegmentOff])
	payload := oggHeaderLen + segments
	if string(src[payload:payload+8]) != "OpusHead" {
		t.Fatal("fixture does not start with OpusHead")
	}
	binary.LittleEndian.PutUint16(src[payload+16:payload+18], uint16(gainQ78))

	// The first page ends where its lacing values end, spread across the page.
	pageEnd, err := oggPageEnd(src, 0)
	if err != nil {
		t.Fatalf("locate first page: %v", err)
	}
	page := src[:pageEnd]
	for i := 22; i < 26; i++ {
		page[i] = 0
	}
	binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))

	return writeFile(t, t.TempDir(), dstName, src)
}
