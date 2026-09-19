package decode

import (
	"bytes"
	"testing"
)

// repagedFixture rewrites a fixture into a synthetic stream that carries the
// same audio packets grouped into pages of packetsPerPage. It exists so the
// seek budget can be measured against two page spans of one signal without
// external tooling: only the page boundaries and granules change, so PCM and
// packet count are identical and any difference in decoded packets is page span.
func repagedFixture(t *testing.T, name string, packetsPerPage int) string {
	t.Helper()

	src := mustNewOggOpusReader(t, fixturePath(t, name))
	preSkip := uint16(src.PreSkip())

	packets := readOggOpusPackets(t, src)
	const samplesPerPacket = 960

	var pages [][]byte
	pages = append(pages, oggPage(0, 0x02, 0, opusHeadPacket(preSkip, 2)))
	pages = append(pages, oggPage(0, 0x00, 1, []byte("OpusTags")))

	seq := uint32(2)
	granule := uint64(preSkip)
	for i := 0; i < len(packets); i += packetsPerPage {
		end := min(i+packetsPerPage, len(packets))
		batch := make([][]byte, 0, end-i)
		for _, p := range packets[i:end] {
			batch = append(batch, p.data)
			granule += samplesPerPacket
		}
		flags := byte(0x00)
		if end == len(packets) {
			flags = 0x04
		}
		pages = append(pages, oggPage(granule, flags, seq, batch...))
		seq++
	}

	return writeOgg(t, name, pages...)
}

// TestPionSeekDecodedPacketsBounded is the load-bearing budget test: a seek may
// not decode more than the variant's warm-up window, the count must not grow
// with the page span, and the fast variant must decode strictly fewer packets
// than the exact one. Without the skip the decoder decodes every packet from the
// landing page to the target, which for a 1 s page is roughly twice the window.
func TestPionSeekDecodedPacketsBounded(t *testing.T) {
	const (
		frame         = 72000 // 1.5 s
		packetSamples = 960
	)
	variants := []struct {
		name   string
		build  func() Factory
		warmup int64
	}{
		{"fast", func() Factory { return NewPionOpusFactory() }, pionWarmupFast},
		{"exact", func() Factory { return NewPionOpusExactFactory() }, pionWarmupExact},
	}

	spans := []int{4, 50}
	decoded := map[string]map[int]int{}
	for _, v := range variants {
		decoded[v.name] = map[int]int{}
		for _, span := range spans {
			t.Run(v.name+"/packets_per_page_"+itoa(int64(span)), func(t *testing.T) {
				path := repagedFixture(t, "stereo_2s.opus", span)

				d, err := v.build().Open(path)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				defer d.Close()

				if err := d.(Seeker).SeekFrame(frame); err != nil {
					t.Fatalf("SeekFrame: %v", err)
				}
				got := d.(*pionOpusDecoder).decodedPackets
				decoded[v.name][span] = got

				// The warm-up window in packets, plus a small allowance for the
				// packet that straddles the boundary.
				bound := int(v.warmup/packetSamples) + 2
				if got > bound {
					t.Fatalf("seek decoded %d packets, want at most %d (warm-up %d, page span %d)",
						got, bound, v.warmup, span*packetSamples)
				}
				if got == 0 {
					t.Fatal("seek decoded nothing, so the warm-up requirement was not met")
				}
				t.Logf("%s, page span %d packets: decoded %d during seek", v.name, span, got)
			})
		}
	}

	// The exact window is ten times the fast one, so every span must show the
	// fast variant decoding strictly fewer packets.
	for _, span := range spans {
		if fast, exact := decoded["fast"][span], decoded["exact"][span]; fast >= exact {
			t.Fatalf("page span %d: fast decoded %d, exact decoded %d; fast must be strictly fewer",
				span, fast, exact)
		}
	}
}

// TestPionSeekDecodedPacketsDoNotScaleWithPageSpan contrasts two streams whose
// pages both span more than the warm-up window. Neither can be decoded from its
// landing page cheaply, so if the count tracked the page span they would differ;
// with the skip both are capped by the window and decode the same amount.
func TestPionSeekDecodedPacketsDoNotScaleWithPageSpan(t *testing.T) {
	smallPath := repagedFixture(t, "stereo_2s.opus", 40)
	largePath := repagedFixture(t, "stereo_2s.opus", 50)

	count := func(path string) int {
		d, err := NewPionOpusFactory().Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer d.Close()
		if err := d.(Seeker).SeekFrame(72000); err != nil {
			t.Fatalf("SeekFrame: %v", err)
		}

		return d.(*pionOpusDecoder).decodedPackets
	}

	small := count(smallPath)
	large := count(largePath)
	t.Logf("decoded during seek: 40-packet pages %d, 50-packet pages %d", small, large)
	if large > small+2 || small > large+2 {
		t.Fatalf("page span changed the decoded packet count: %d vs %d; the skip must cap it at the warm-up window", small, large)
	}
}

// TestPionSeekMatchesStraightDecodeEverywhere is the correctness proof: after a
// seek the first delivered frame must be exactly the requested frame and its
// PCM must equal a straight decode that discarded to the same frame. It covers
// the start, a target inside the warm-up window, the middle, and the end. It
// uses the exact variant because only it promises byte identity.
func TestPionSeekMatchesStraightDecodeEverywhere(t *testing.T) {
	factory := NewPionOpusExactFactory()
	full := decodeFixture(t, "stereo_2s.opus")

	targets := []int64{0, 1, 5000, pionWarmupExact - 1000, pionWarmupExact, 30000, 48000, 90000, 96000}
	for _, at := range targets {
		if at >= int64(len(full)/2) {
			continue
		}
		t.Run(itoa(at), func(t *testing.T) {
			d, err := factory.Open(fixturePath(t, "stereo_2s.opus"))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()
			if err := d.(Seeker).SeekFrame(at); err != nil {
				t.Fatalf("SeekFrame(%d): %v", at, err)
			}

			frames := min(int64(20000), int64(len(full)/2)-at)
			got := readExactlyFrames(t, d, int(frames))
			want := full[at*2 : (at+frames)*2]
			if !bytes.Equal(float32sToBytes(got), float32sToBytes(want)) {
				t.Fatalf("seek to %d differs from the straight decode", at)
			}
		})
	}
}

// TestPionSeekBackToStartAfterSkip ensures a skip-heavy seek does not corrupt
// the reader for a subsequent seek to the head, where the skip is a no-op. It
// uses the exact variant so the comparison can stay byte-identical.
func TestPionSeekBackToStartAfterSkip(t *testing.T) {
	full := decodeFixture(t, "stereo_2s.opus")

	d, err := NewPionOpusExactFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := d.(Seeker).SeekFrame(90000); err != nil {
		t.Fatalf("SeekFrame(90000): %v", err)
	}
	if err := d.(Seeker).SeekFrame(0); err != nil {
		t.Fatalf("SeekFrame(0): %v", err)
	}
	got := readExactlyFrames(t, d, 5000)
	if !bytes.Equal(float32sToBytes(got), float32sToBytes(full[:5000*2])) {
		t.Fatal("seek back to the start after a skip did not restore the head")
	}
}

// itoa is a tiny base-10 formatter for subtest names, avoiding an import for a
// single call site.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}
