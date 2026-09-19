// Opus packet duration read from the table-of-contents header.
//
// A seek must know how far each packet advances the stream, but the Ogg reader
// reports a granule only per page: every packet completing on a page shares that
// page's granule, so within a page the container says nothing about individual
// packet positions. The packet itself does, through the configuration and frame
// count in its first one or two bytes. This mirrors libopusfile's
// op_get_packet_duration, which is opus_packet_get_nb_frames times
// opus_packet_get_samples_per_frame with the same 120 ms ceiling; the reference
// is ref/analysis/opus/src/opus.c and opus_decoder.c.

package decode

// opusPacketSamples48 returns a packet's duration in 48 kHz samples, or -1 when
// it is empty or malformed. A caller treats -1 as "cannot be skipped" and
// decodes the packet instead.
func opusPacketSamples48(packet []byte) int {
	if len(packet) < 1 {
		return -1
	}
	second := byte(0)
	hasSecond := len(packet) >= 2
	if hasSecond {
		second = packet[1]
	}

	return opusSamplesFromTOC(packet[0], second, hasSecond)
}

// opusSamplesFromTOC decodes a duration from the first two packet bytes. It is
// separate from opusPacketSamples48 so a reader can time a packet while walking
// its lacing without assembling the body. hasSecond reports whether second is
// actually the packet's second byte; a code-3 packet without it cannot be timed.
func opusSamplesFromTOC(toc, second byte, hasSecond bool) int {
	var perFrame int
	switch {
	case toc&0x80 != 0:
		// CELT-only configurations 16..31: 2.5, 5, 10, or 20 ms.
		perFrame = (48000 << ((toc >> 3) & 0x3)) / 400
	case toc&0x60 == 0x60:
		// Hybrid configurations 12..15: 10 or 20 ms.
		if toc&0x08 != 0 {
			perFrame = 48000 / 50
		} else {
			perFrame = 48000 / 100
		}
	default:
		// SILK-only configurations 0..11: 10, 20, 40, or 60 ms.
		frame := (toc >> 3) & 0x3
		if frame == 3 {
			perFrame = 48000 * 60 / 1000
		} else {
			perFrame = (48000 << frame) / 100
		}
	}

	var frames int
	switch toc & 0x3 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	default:
		if !hasSecond {
			return -1
		}
		frames = int(second & 0x3F)
	}

	samples := frames * perFrame
	// RFC 6716 section 3.1 caps a packet at 120 ms.
	if samples <= 0 || samples > 120*48 {
		return -1
	}

	return samples
}
