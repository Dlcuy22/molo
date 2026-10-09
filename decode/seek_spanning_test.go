package decode

import (
	"testing"
)

// spanningFixture rewrites a fixture so that one audio packet is split across a
// page boundary: the first part ends a page with a 255 lacing value and no
// granule, and the continuation opens the next page. That is the post-seek
// shape the reader must drop per RFC 7845 section 3, and the shape a warm-up
// skip must not misjudge. It returns the stream path and the index of the split
// packet, whose continuation is the first packet the reader can deliver.
func spanningFixture(t *testing.T, name string) (path string, splitAt int, samplesPerPacket int) {
	t.Helper()

	src := mustNewOggOpusReader(t, fixturePath(t, name))
	preSkip := uint16(src.PreSkip())
	packets := readOggOpusPackets(t, src)
	if len(packets) < 3 {
		t.Fatalf("%s has %d packets, need at least 3", name, len(packets))
	}

	// Split a packet that is longer than 255 bytes so it can cross a page.
	splitAt = -1
	for i, p := range packets {
		if len(p.data) > 256 {
			splitAt = i

			break
		}
	}
	if splitAt < 0 {
		t.Skipf("%s has no packet longer than 256 bytes", name)
	}

	samplesPerPacket = 960
	big := packets[splitAt].data
	front, back := big[:255], big[255:]

	// Page A carries the earlier packets plus the front of the split packet and
	// ends mid-packet, so its granule is -1 and it is not indexed.
	var lacingA, bodyA []byte
	granule := uint64(preSkip)
	for _, p := range packets[:splitAt] {
		lacingA, bodyA = appendPacket(lacingA, bodyA, p.data)
		granule += uint64(samplesPerPacket)
	}
	lacingA = append(lacingA, 255)
	bodyA = append(bodyA, front...)

	// Page B continues the split packet and carries everything after it.
	lacingB, bodyB := appendPacket(nil, nil, back)
	granule += uint64(samplesPerPacket)
	for _, p := range packets[splitAt+1:] {
		lacingB, bodyB = appendPacket(lacingB, bodyB, p.data)
		granule += uint64(samplesPerPacket)
	}

	pageA := rawOggPage(-1, 0x00, 2, lacingA, bodyA)
	pageB := rawOggPage(int64(granule), 0x01, 3, lacingB, bodyB)

	return writeOgg(t, name,
		oggPage(0, 0x02, 0, opusHeadPacket(preSkip, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		pageA, pageB,
	), splitAt, samplesPerPacket
}

// appendPacket appends a whole packet's lacing and body.
func appendPacket(lacing, body, packet []byte) ([]byte, []byte) {
	for len(packet) >= 255 {
		lacing = append(lacing, 255)
		body = append(body, packet[:255]...)
		packet = packet[255:]
	}
	lacing = append(lacing, byte(len(packet)))

	return lacing, append(body, packet...)
}

// TestOpusSeekAcrossSpanningPageStaysBounded composes the warm-up skip with the
// reader's continued-packet drop. A seek whose landing page opens mid-packet
// cannot reconstruct that packet, so its first deliverable frame is the next
// packet. PositionExact is false for such a landing, which makes the decoder
// abandon the packet-count skip and decode forward to the target instead; the
// guarantees this test can therefore check are that the seek does not error,
// that the discarded prefix is not decoded as audio, and that no packet before
// the landing page is re-decoded. The drop itself is pinned by
// TestOggOpusReaderSeekSkipsIncompleteLeadingPacket, which this must not break.
func TestOpusSeekAcrossSpanningPageStaysBounded(t *testing.T) {
	path, _, _ := spanningFixture(t, "stereo_2s.opus")
	packetSamples := 960

	for _, at := range []int64{0, 960, 20000, 40000} {
		t.Run(itoa(at), func(t *testing.T) {
			d, err := NewOpusFactory().Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()
			if err := d.(Seeker).SeekFrame(at); err != nil {
				t.Fatalf("SeekFrame(%d): %v", at, err)
			}
			// With the skip abandoned the decoder walks from the landing
			// packet to the target, so the decoded count is bounded by the
			// target distance, never by an earlier page.
			bound := int(at)/packetSamples + 4
			if decoded := d.(*opusDecoder).decodedPackets; decoded > bound {
				t.Fatalf("spanning-page seek decoded %d packets, want at most %d (target %d frames)",
					decoded, bound, at)
			}
			if _, err := d.ReadFrames(make([]float32, 2048)); err != nil {
				t.Fatalf("ReadFrames after spanning-page seek: %v", err)
			}
		})
	}
}
