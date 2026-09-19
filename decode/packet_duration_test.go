package decode

import (
	"bytes"
	"os"
	"testing"
)

// TestOpusPacketSamples48 pins the TOC duration table against the reference
// semantics in opus_packet_get_samples_per_frame and opus_packet_get_nb_frames.
// A skip decision that mistimes a packet either drops audio that was needed for
// the codec's warm-up or wastes a decode, so the table is pinned directly.
func TestOpusPacketSamples48(t *testing.T) {
	tests := []struct {
		name   string
		packet []byte
		want   int
	}{
		{"empty", nil, -1},
		{"silk nb 10ms", []byte{0 << 3, 0x00}, 480},
		{"silk nb 20ms", []byte{1 << 3, 0x00}, 960},
		{"silk nb 40ms", []byte{2 << 3, 0x00}, 1920},
		{"silk nb 60ms", []byte{3 << 3, 0x00}, 2880},
		{"silk mb 20ms", []byte{5 << 3, 0x00}, 960},
		{"silk wb 60ms", []byte{11 << 3, 0x00}, 2880},
		{"hybrid swb 10ms", []byte{12 << 3, 0x00}, 480},
		{"hybrid fb 10ms", []byte{14 << 3, 0x00}, 480},
		{"hybrid fb 20ms", []byte{15 << 3, 0x00}, 960},
		{"celt nb 2.5ms", []byte{16 << 3, 0x00}, 120},
		{"celt nb 5ms", []byte{17 << 3, 0x00}, 240},
		{"celt wb 10ms", []byte{22 << 3, 0x00}, 480},
		{"celt swb 20ms", []byte{27 << 3, 0x00}, 960},
		{"celt fb 20ms", []byte{31 << 3, 0x00}, 960},
		{"two equal frames", []byte{31<<3 | 0x1, 0x00}, 1920},
		{"two different frames", []byte{31<<3 | 0x2, 0x00, 0x00}, 1920},
		{"code 3 three frames", []byte{31<<3 | 0x3, 0x03, 0x00}, 2880},
		{"code 3 max 120ms", []byte{31<<3 | 0x3, 0x06, 0x00}, 5760},
		{"code 3 missing count", []byte{28<<3 | 0x3}, -1},
		{"code 3 zero frames", []byte{31<<3 | 0x3, 0x00}, -1},
		{"over 120ms", []byte{31<<3 | 0x3, 0x30}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := opusPacketSamples48(tt.packet); got != tt.want {
				t.Fatalf("opusPacketSamples48(%v) = %d, want %d", tt.packet, got, tt.want)
			}
		})
	}
}

// TestOpusSamplesFromTOCRejectsTruncatedCode3 pins the reader-side contract: a
// code-3 TOC with no second byte is untimeable, so a skip must fall back to
// decoding rather than guess.
func TestOpusSamplesFromTOCRejectsTruncatedCode3(t *testing.T) {
	const toc = 31<<3 | 0x3 // CELT fullband 20 ms with the code-3 frame count.
	if got := opusSamplesFromTOC(toc, 0, false); got != -1 {
		t.Fatalf("truncated code 3 = %d, want -1", got)
	}
	if got := opusSamplesFromTOC(toc, 0x03, true); got != 2880 {
		t.Fatalf("code 3 with count = %d, want 2880", got)
	}
}

// TestOpusPacketSamples48MatchesFixtureDecode cross-checks the TOC sum of a real
// fixture against the decoder: the durations must add up to the granule span,
// or the skip would drift out of the target.
func TestOpusPacketSamples48MatchesFixtureDecode(t *testing.T) {
	for _, name := range []string{"stereo_2s.opus", "mono_1s.opus", "short_stereo.opus"} {
		t.Run(name, func(t *testing.T) {
			r := mustNewOggOpusReader(t, fixturePath(t, name))
			preSkip := int64(r.PreSkip())
			total := int64(0)
			for {
				pkt, _, err := r.ReadPacket()
				if err != nil {
					break
				}
				n := opusPacketSamples48(pkt)
				if n <= 0 {
					t.Fatalf("packet len=%d is untimeable", len(pkt))
				}
				total += int64(n)
			}
			// The granule span is pre-skip plus the decoded samples of a straight
			// decode; the TOC sum must reach it, without counting more than one
			// final packet of granule-truncated padding.
			span := int64(r.TotalGranule()) - preSkip
			if total < span {
				t.Fatalf("TOC sum %d is below the granule span %d", total, int64(r.TotalGranule())-preSkip)
			}
			if total > span+5760 {
				t.Fatalf("TOC sum %d overshoots the granule span by more than a packet", total)
			}
		})
	}
}

// TestRepagedFixtureHasAlternatePageSpans is a guard for the budget test's
// fixture builder: the two streams must differ only in page span, so a bounded
// decode proves page-span independence rather than a different signal.
func TestRepagedFixtureHasAlternatePageSpans(t *testing.T) {
	small := repagedFixture(t, "stereo_2s.opus", 4)
	large := repagedFixture(t, "stereo_2s.opus", 50)

	smallSrc, err := os.ReadFile(small)
	if err != nil {
		t.Fatalf("read small: %v", err)
	}
	largeSrc, err := os.ReadFile(large)
	if err != nil {
		t.Fatalf("read large: %v", err)
	}
	if bytes.Equal(smallSrc, largeSrc) {
		t.Fatal("re-paged streams are identical; page span was not changed")
	}

	smallReader := mustNewOggOpusReader(t, small)
	largeReader := mustNewOggOpusReader(t, large)
	if len(smallReader.index) <= len(largeReader.index) {
		t.Fatalf("small pages produced %d indexed pages, large %d; want small pages to have more",
			len(smallReader.index), len(largeReader.index))
	}
	if smallReader.TotalGranule() != largeReader.TotalGranule() {
		t.Fatalf("re-paged totals differ: %d vs %d", smallReader.TotalGranule(), largeReader.TotalGranule())
	}
}
