package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/dlcuy22/player/core"
	"github.com/tphakala/go-aac/pcm"
)

// readAacAll drains a decoder into interleaved stereo float32 exactly as it
// delivered it.
func readAacAll(t *testing.T, d Decoder) []float32 {
	t.Helper()

	buf := make([]float32, 4096)
	var out []float32
	for {
		n, err := d.ReadFrames(buf)
		out = append(out, buf[:n*2]...)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("ReadFrames: %v", err)
			}

			return out
		}
	}
}

func openAacFixture(t *testing.T, name string) Decoder {
	t.Helper()

	d, err := NewAacFactory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	return d
}

// TestAacHeaderAndLength pins the stream shape and the exact end-of-stream
// contract. ADTS has no field that states the total, but the header scan turns
// the frame count into an exact one, so Info can promise it and delivery clamps
// to it.
func TestAacHeaderAndLength(t *testing.T) {
	d := openAacFixture(t, "sine_stereo_48k.aac")

	info := d.Info()
	if info.Format != core.CanonicalFormat {
		t.Fatalf("Format = %+v, want the canonical %+v", info.Format, core.CanonicalFormat)
	}
	// The fixture is 13 ADTS frames of 1024: 13312 source samples at 48 kHz
	// divide evenly into the canonical domain.
	if info.TotalFrames != 13312 {
		t.Fatalf("TotalFrames = %d, want 13312", info.TotalFrames)
	}
	if info.SourceSamples != 13312 || info.SourceRate != 48000 {
		t.Fatalf("source domain = %d @ %d Hz, want 13312 @ 48000 Hz", info.SourceSamples, info.SourceRate)
	}

	got := readAacAll(t, d)
	if len(got) == 0 {
		t.Fatal("no audio decoded")
	}
	// 0.25 s of input, but the encoder's priming and padding make the decoded
	// stream 13 frames of 1024: 13312 canonical frames here.
	if len(got)/2 != 13312 {
		t.Fatalf("decoded %d frames, want 13312", len(got)/2)
	}

	// A second drain must report end of stream without delivering more audio.
	buf := make([]float32, 512)
	if n, err := d.ReadFrames(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read after end = (%d, %v), want (0, EOF)", n, err)
	}
}

// TestAacReservedSampleRateIsRejected guards the sampling_frequency_index table:
// a 4-bit index can be 0..15 but only 0..12 are defined, so a reserved index
// must be rejected rather than read past the table. A malformed file must not
// panic the process.
func TestAacReservedSampleRateIsRejected(t *testing.T) {
	for _, idx := range []byte{13, 14, 15} {
		t.Run(itoa(int64(idx)), func(t *testing.T) {
			// A 7-byte ADTS header with the reserved index, LC profile, stereo
			// channel config, and a minimal frame length of 7, so the sync,
			// channel and length checks pass and only the rate table can reject
			// it. Padded past 10 bytes so the header read does not fail first.
			hdr := []byte{
				0xFF, 0xF1, 0x40 | idx<<2, 0x80, 0x00, 0xE0, 0x00,
				0, 0, 0, 0, 0, 0, 0, 0, 0,
			}
			path := writeFile(t, t.TempDir(), "reserved.aac", hdr)

			if _, err := NewAacFactory().Open(path); err == nil {
				t.Fatal("Open accepted a reserved sampling_frequency_index")
			}
			if _, err := NewAacFactory().Probe(path, ProbeOptions{Duration: core.DurationProbe}); err == nil {
				t.Fatal("Probe accepted a reserved sampling_frequency_index")
			}
		})
	}
}

// TestAacRegistrySelection proves the factory is wired into dispatch: the
// extension resolves to it, and the magic check claims a real ADTS header
// without over-claiming unrelated bytes.
func TestAacRegistrySelection(t *testing.T) {
	path := fixturePath(t, "sine_stereo_48k.aac")

	d, err := Default.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d.Close()

	if _, ok := d.(*aacDecoder); !ok {
		t.Fatalf("Default.Open produced %T, want *aacDecoder", d)
	}

	names := map[string]bool{}
	for _, c := range Default.Codecs() {
		names[c.Name] = true
		if c.Name == "aac" {
			if c.FriendlyName != "Aac" || c.Weight != 90 || !containsExt(c.Exts, ".aac") {
				t.Fatalf("aac codec row is malformed: %+v", c)
			}
		}
	}
	if !names["aac"] {
		t.Fatal("aac factory is not registered")
	}

	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f := NewAacFactory()
	if !f.Match(header[:2]) {
		t.Fatal("aac factory did not claim a real ADTS header")
	}
	// A lone 0xFF is not a syncword: the second byte must complete the 12-bit
	// sync and carry the ADTS layer bits.
	if f.Match([]byte{0xFF}) {
		t.Fatal("aac factory claimed a lone 0xFF")
	}
	if f.Match([]byte("OggS")) {
		t.Fatal("aac factory claimed an Ogg stream")
	}
	if f.Match([]byte{0xFF, 0xFB}) {
		t.Fatal("aac factory claimed an MPEG-1 Layer III frame (0xFF 0xFB)")
	}
}

// TestAacDecodesTheTone is the signature check. The fixture is a stereo pair
// of a 440 Hz left tone and a 660 Hz right tone; the decoded output must carry
// that energy, not silence and not noise. AAC is lossy, so this asserts shape
// and level rather than bit-exactness.
func TestAacDecodesTheTone(t *testing.T) {
	d := openAacFixture(t, "sine_stereo_48k.aac")
	got := readAacAll(t, d)

	rms := rmsOf(got)
	if rms < 0.1 || rms > 0.6 {
		t.Fatalf("RMS = %.4f, want a real tone inside 0.1..0.6", rms)
	}

	rate := float64(core.CanonicalFormat.Rate)
	left440 := tonePower(got, 0, 2, 440, rate)
	left660 := tonePower(got, 0, 2, 660, rate)
	right660 := tonePower(got, 1, 2, 660, rate)
	right440 := tonePower(got, 1, 2, 440, rate)

	if left440 < left660*100 {
		t.Fatalf("left channel is not dominated by 440 Hz: 440=%.0f 660=%.0f", left440, left660)
	}
	if right660 < right440*100 {
		t.Fatalf("right channel is not dominated by 660 Hz: 660=%.0f 440=%.0f", right660, right440)
	}
}

// TestAacIsASeeker pins the native-seek capability. The ADTS header scan builds
// a byte index, so the decoder reparses its own framing and jumps natively
// instead of forcing the streamer's reopen-and-discard fallback.
func TestAacIsASeeker(t *testing.T) {
	d := openAacFixture(t, "sine_stereo_48k.aac")
	if _, ok := d.(Seeker); !ok {
		t.Fatal("aac decoder does not implement Seeker, but the ADTS index supports a native seek")
	}
}

// TestAacProbeReportsCanonicalShape checks the prober: DurationProbe scans the
// 7-byte headers for the exact frame count, so TotalFrames is now exact rather
// than -1, and DurationUnknown stays cheap and leaves it unknown.
func TestAacProbeReportsCanonicalShape(t *testing.T) {
	factory := NewAacFactory()
	path := fixturePath(t, "sine_stereo_48k.aac")

	info, err := factory.Probe(path, ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Format != core.CanonicalFormat {
		t.Fatalf("Format = %+v, want the canonical %+v", info.Format, core.CanonicalFormat)
	}
	// The fixture is 13 ADTS frames of 1024 source samples.
	if info.TotalFrames != 13312 {
		t.Fatalf("TotalFrames = %d, want the exact 13312", info.TotalFrames)
	}
	if info.SourceSamples != 13312 || info.SourceRate != 48000 {
		t.Fatalf("source domain = %d @ %d Hz, want 13312 @ 48000 Hz", info.SourceSamples, info.SourceRate)
	}

	unknown, err := factory.Probe(path, ProbeOptions{Duration: core.DurationUnknown})
	if err != nil {
		t.Fatalf("Probe (unknown): %v", err)
	}
	if unknown.TotalFrames != -1 || unknown.SourceSamples != 0 {
		t.Fatalf("DurationUnknown total = %d, source = %d, want -1 and 0",
			unknown.TotalFrames, unknown.SourceSamples)
	}
}

// TestAacResamplesToCanonical covers the rate conversion end to end. ADTS has
// no 44.1 kHz fixture in testdata, so one is encoded in a temp directory with
// go-aac's own encoder and decoded back through the factory. A skipped
// conversion would report the source's 12288 frames instead of the canonical
// count, which is what the lower bound rules out.
func TestAacResamplesToCanonical(t *testing.T) {
	path := writeAac44100(t)

	d, err := NewAacFactory().Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	info := d.Info()
	if info.Format.Rate != core.CanonicalFormat.Rate {
		t.Fatalf("format rate = %d, want %d", info.Format.Rate, core.CanonicalFormat.Rate)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1", info.TotalFrames)
	}

	got := readAacAll(t, d)
	frames := len(got) / 2
	// 12288 source frames at 44.1 kHz is 13374.7 canonical frames; the
	// converter finishes the final interval, so accept its one-frame tail.
	if frames < 13374 || frames > 13376 {
		t.Fatalf("decoded %d canonical frames, want 13374..13376", frames)
	}

	rate := float64(core.CanonicalFormat.Rate)
	if p := tonePower(got, 0, 2, 440, rate); p <= tonePower(got, 0, 2, 660, rate)*100 {
		t.Fatalf("resampled left channel lost its 440 Hz tone: %v", p)
	}
}

// TestAacSeekMatchesStraightDecode proves the native seek lands on an ADTS frame
// boundary and delivers what a straight decode holds from that point. An ADTS
// frame cannot start mid-frame, so the seek lands at the frame at or before the
// target; the comparison allows a shift of up to one frame. The PCM is not
// bit-identical because AAC reconstruction threads state (the PNS generator)
// across frames, so a fresh decoder primes it from the frame before the landing
// one only; the per-sample tolerance absorbs that small drift, which stays far
// below the signal and would be near the full amplitude for a wrong landing.
func TestAacSeekMatchesStraightDecode(t *testing.T) {
	const (
		auFrame   = 1024
		window    = 4000
		tolerance = 0.01
	)
	full := readAacAll(t, openAacFixture(t, "sine_stereo_48k.aac"))
	total := int64(len(full) / 2)

	targets := []int64{0, 2048, 4096, 4097, 9000, total - window - auFrame}
	for _, at := range targets {
		t.Run(itoa(at), func(t *testing.T) {
			d := openAacFixture(t, "sine_stereo_48k.aac")
			if err := d.(Seeker).SeekFrame(at); err != nil {
				t.Fatalf("SeekFrame(%d): %v", at, err)
			}

			got := readExactlyFrames(t, d, window)
			if !matchesWithin(full, got, at, auFrame, tolerance) {
				t.Fatalf("seek to %d differs from the straight decode by more than %g per sample",
					at, tolerance)
			}
		})
	}
}

// TestAacSeekBackwardsAndToEnd walks the seek backwards, to the last frame, and
// to a past-the-end target, which is where a stale index or a missed reset would
// show.
func TestAacSeekBackwardsAndToEnd(t *testing.T) {
	const (
		window    = 4000
		tolerance = 0.01
	)
	full := readAacAll(t, openAacFixture(t, "sine_stereo_48k.aac"))
	total := int64(len(full) / 2)

	d := openAacFixture(t, "sine_stereo_48k.aac")
	seeker := d.(Seeker)

	if err := seeker.SeekFrame(9000); err != nil {
		t.Fatalf("SeekFrame(9000): %v", err)
	}
	if err := seeker.SeekFrame(2048); err != nil {
		t.Fatalf("SeekFrame(2048): %v", err)
	}
	got := readExactlyFrames(t, d, window)
	if !matchesWithin(full, got, 2048, 1024, tolerance) {
		t.Fatal("backwards seek did not restore the earlier position")
	}

	// A target past the end lands on the last frame and delivers its tail.
	lastFrame := (total - 1) / 1024 * 1024
	if err := seeker.SeekFrame(total + 10*1024); err != nil {
		t.Fatalf("SeekFrame(past end): %v", err)
	}
	tail := readAacAll(t, d)
	if int64(len(tail)/2) != total-lastFrame {
		t.Fatalf("after seek past the end delivered %d frames, want the %d-frame tail",
			len(tail)/2, total-lastFrame)
	}
	if !matchesWithin(full, tail, lastFrame, 0, tolerance) {
		t.Fatal("the tail after a past-the-end seek does not match the straight decode")
	}

	if err := seeker.SeekFrame(-1); err == nil {
		t.Fatal("SeekFrame(-1) succeeded, want an error")
	}
}

// matchesWithin reports whether got matches full at a start of at or earlier,
// within shift frames, each sample within slop. The window can only shift
// backward because an ADTS seek lands on the frame at or before the target; a
// late landing would not be accepted. The per-sample slop absorbs the AAC
// decoder's cross-frame state.
func matchesWithin(full, got []float32, at int64, shift int, slop float64) bool {
	for s := 0; s <= shift; s++ {
		if windowWithin(full, got, at-int64(s), slop) {
			return true
		}
	}

	return false
}

// windowWithin reports whether got matches full starting at start, sample by
// sample within slop.
func windowWithin(full, got []float32, start int64, slop float64) bool {
	if start < 0 || start*2+int64(len(got)) > int64(len(full)) {
		return false
	}
	off := start * 2
	for i := range got {
		if diff := float64(full[off+int64(i)] - got[i]); diff > slop || diff < -slop {
			return false
		}
	}

	return true
}

// TestAacConverterResamples is the direct converter check the fixture route
// above doubles: newAacDecoder must build a non-stereo converter for a 44.1 kHz
// source and emit 48000 frames per source second.
func TestAacConverterResamples(t *testing.T) {
	c := newPCMConverter(44100, 2, 2)
	if c.stereo() {
		t.Fatal("44.1 kHz converter claims to be at the canonical rate")
	}
	if c.ratio == 1 {
		t.Fatalf("ratio = %v, want 44100/48000", c.ratio)
	}
}

// TestAacS16Scale pins the conversion the adapter configures: go-aac emits
// interleaved little-endian S16, and a full-scale negative sample must map to
// exactly -1.0 and a full-scale positive one to just under +1.0. This is the
// scale the whole decoded level hangs on, so an off-by-one bit in bytesPS or
// the divisor is caught here rather than hidden by a wide RMS band.
func TestAacS16Scale(t *testing.T) {
	raw := []byte{0x00, 0x80, 0xFF, 0x7F} // left = -32768, right = +32767
	c := newPCMConverter(48000, 2, 2)
	if n := c.ingest(raw); n != 1 {
		t.Fatalf("ingest consumed %d frames, want 1", n)
	}
	if !c.stereo() {
		t.Fatal("48 kHz converter is not the identity-rate path")
	}
	if got := c.src[0]; got != -1 {
		t.Fatalf("full-scale negative sample = %v, want -1", got)
	}
	// 32767/32768 rounds within float32; require it to sit at the top, not at
	// half scale as a divisor of 2^16 would put it.
	if got := c.src[1]; got < 0.999 || got > 1 {
		t.Fatalf("full-scale positive sample = %v, want just under 1", got)
	}
}

// writeAac44100 encodes a 0.25 s 440/660 Hz stereo pair at 44.1 kHz with
// go-aac's encoder, so the rate-conversion path can be tested without a
// committed fixture.
func writeAac44100(t *testing.T) string {
	t.Helper()

	const rate = 44100
	frames := rate / 4
	raw := make([]byte, 0, frames*4)
	for i := range frames {
		left := int16(0.6 * 32767 * math.Sin(2*math.Pi*440*float64(i)/rate))
		right := int16(0.6 * 32767 * math.Sin(2*math.Pi*660*float64(i)/rate))
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(left))
		raw = append(raw, b[:]...)
		binary.LittleEndian.PutUint16(b[:], uint16(right))
		raw = append(raw, b[:]...)
	}

	var buf bytes.Buffer
	enc, err := pcm.NewEncoder(&buf, pcm.Config{SampleRate: rate, BitDepth: 16, Channels: 2, Bitrate: 128000})
	if err != nil {
		t.Fatalf("go-aac encoder: %v", err)
	}
	if _, err := enc.Write(raw); err != nil {
		t.Fatalf("go-aac Write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("go-aac Close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "tone44.aac")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// rmsOf returns the root-mean-square of interleaved samples.
func rmsOf(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += float64(v) * float64(v)
	}

	return math.Sqrt(sum / float64(len(samples)))
}

// tonePower is the Goertzel power of one channel of interleaved samples at
// freq, used to tell a real tone from broadband noise.
func tonePower(samples []float32, channel, channels int, freq, rate float64) float64 {
	w := 2 * math.Pi * freq / rate
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for i := channel; i < len(samples); i += channels {
		s0 := float64(samples[i]) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}

	return s1*s1 + s2*s2 - coeff*s1*s2
}
