package decode

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/dlcuy22/player/core"
	"github.com/tphakala/go-flac/pcm"
)

// readFlacAll decodes a fixture to the end and returns interleaved stereo
// float32 exactly as the decoder delivered it.
func readFlacAll(t *testing.T, d Decoder) []float32 {
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

func openFlacFixture(t *testing.T, name string) Decoder {
	t.Helper()

	d, err := NewFlacFactory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	return d
}

// TestFlacHeaderAndLength pins the stream shape: the canonical format the
// device requires, and the exact frame count from STREAMINFO rather than a
// count learned by decoding to the end.
func TestFlacHeaderAndLength(t *testing.T) {
	d := openFlacFixture(t, "sine_stereo_48k.flac")

	info := d.Info()
	if info.Format != core.CanonicalFormat {
		t.Fatalf("Format = %+v, want the canonical %+v", info.Format, core.CanonicalFormat)
	}
	// The fixture is exactly 0.25 s at 48 kHz.
	if info.TotalFrames != 12000 {
		t.Fatalf("TotalFrames = %d, want 12000", info.TotalFrames)
	}
	if got := readFlacAll(t, d); len(got)/2 != 12000 {
		t.Fatalf("decoded %d frames, want 12000", len(got)/2)
	}
}

// TestFlacIsBitExact is the load-bearing correctness test: for a native 48 kHz
// file the decoder must reproduce the encoded samples exactly. The expected
// bits come from the reference decode of the same fixture, so any change to the
// scaling or channel handling shows up here rather than passing on RMS alone.
func TestFlacIsBitExact(t *testing.T) {
	d := openFlacFixture(t, "sine_stereo_48k.flac")
	got := readFlacAll(t, d)

	// The first frames are silence (the sine starts at zero), then a rising
	// ramp. The exact bit patterns are from the fixture, verified against the
	// reference decoder.
	want := []uint32{
		0x0, 0x0,
		0x3ceb0000, 0x3d308000,
		0x3d6b0000, 0x3db00000,
		0x3db00000, 0x3e032000,
		0x3de9c000, 0x3e2d6000,
	}
	if len(got) < len(want) {
		t.Fatalf("decoded %d samples, want at least %d", len(got), len(want))
	}
	for i, w := range want {
		if b := math.Float32bits(got[i]); b != w {
			t.Fatalf("sample %d = %#x, want %#x", i, b, w)
		}
	}
}

// TestFlacBitDepthsAgree verifies that a 24-bit source produces the same PCM as
// the 16-bit source of the same signal. The fixtures are the same tones encoded
// at the two depths, so the normalized samples must match bit for bit: bit depth
// is a storage detail, not a level change.
func TestFlacBitDepthsAgree(t *testing.T) {
	got16 := readFlacAll(t, openFlacFixture(t, "sine_stereo_48k.flac"))
	got24 := readFlacAll(t, openFlacFixture(t, "sine_stereo_24bit.flac"))

	if len(got16) != len(got24) {
		t.Fatalf("frame counts differ: 16-bit %d, 24-bit %d", len(got16)/2, len(got24)/2)
	}
	for i := range got16 {
		if math.Float32bits(got16[i]) != math.Float32bits(got24[i]) {
			t.Fatalf("sample %d differs: 16-bit %#x, 24-bit %#x",
				i, math.Float32bits(got16[i]), math.Float32bits(got24[i]))
		}
	}
}

// TestFlacMonoIsDuplicated checks the channel contract: a mono file must fill
// both output channels, because the streamer and the device carry stereo.
func TestFlacMonoIsDuplicated(t *testing.T) {
	got := readFlacAll(t, openFlacFixture(t, "sine_mono_48k.flac"))
	if len(got) == 0 {
		t.Fatal("no audio decoded")
	}
	for i := 0; i < len(got); i += 2 {
		if got[i] != got[i+1] {
			t.Fatalf("frame %d is not duplicated: left %v right %v", i/2, got[i], got[i+1])
		}
	}
}

// TestFlacResamplesToCanonical covers the rate conversion: a 44.1 kHz file must
// come out at 48000 frames per source second, because the device rejects any
// other rate. The tone must survive the conversion at the same amplitude and
// frequency, which is what a broken interpolator would fail.
func TestFlacResamplesToCanonical(t *testing.T) {
	d := openFlacFixture(t, "sine_stereo_44k.flac")

	info := d.Info()
	if info.Format.Rate != core.CanonicalFormat.Rate {
		t.Fatalf("format rate = %d, want %d", info.Format.Rate, core.CanonicalFormat.Rate)
	}
	// 0.25 s at 44.1 kHz is 0.25 s of output: 12000 canonical frames, even
	// though the source holds 11025.
	if info.TotalFrames != 12000 {
		t.Fatalf("TotalFrames = %d, want 12000", info.TotalFrames)
	}

	got := readFlacAll(t, d)
	if len(got)/2 != 12000 {
		t.Fatalf("decoded %d frames, want 12000", len(got)/2)
	}

	// The interpolator must not attenuate. The two fixtures are the same tones
	// at the same level, so the settled peaks must agree within the difference
	// the discrete sampling grid alone can explain (a few percent); a broken
	// interpolator loses far more than that.
	peak48 := peakOf(readFlacAll(t, openFlacFixture(t, "sine_stereo_48k.flac")))
	peak44 := peakOf(got)
	if peak44 < peak48*0.9 || peak44 > peak48*1.1 {
		t.Fatalf("resampled peak = %.4f against 48 kHz peak %.4f: the interpolator is attenuating", peak44, peak48)
	}
}

// peakOf returns the largest absolute sample in a settled central window, which
// avoids the ramp at the start and the tail.
func peakOf(samples []float32) float64 {
	if len(samples) < 4000 {
		return 0
	}
	var peak float64
	for i := 2000; i < len(samples)-2000; i++ {
		if a := math.Abs(float64(samples[i])); a > peak {
			peak = a
		}
	}

	return peak
}

// TestFlacSeekLandsExactly verifies the seek contract: after SeekFrame the next
// delivered frame is the requested one, matching a straight decode of the same
// position.
func TestFlacSeekLandsExactly(t *testing.T) {
	full := readFlacAll(t, openFlacFixture(t, "sine_stereo_48k.flac"))

	for _, at := range []int64{0, 1, 960, 5000, 6000, 9000, 11000} {
		t.Run(itoa(at), func(t *testing.T) {
			d := openFlacFixture(t, "sine_stereo_48k.flac")
			seeker, ok := d.(Seeker)
			if !ok {
				t.Fatal("flac decoder does not implement Seeker")
			}
			if err := seeker.SeekFrame(at); err != nil {
				t.Fatalf("SeekFrame(%d): %v", at, err)
			}

			frames := min(int64(500), int64(len(full)/2)-at)
			got := make([]float32, frames*2)
			readFlacFrames(t, d, got)

			want := full[at*2 : (at+frames)*2]
			if !bytes.Equal(float32sToBytes(got), float32sToBytes(want)) {
				t.Fatalf("seek to %d differs from the straight decode", at)
			}
		})
	}
}

// readFlacFrames reads exactly len(dst)/2 frames.
func readFlacFrames(t *testing.T, d Decoder, dst []float32) {
	t.Helper()

	got := 0
	for got < len(dst)/2 {
		n, err := d.ReadFrames(dst[got*2:])
		got += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatalf("reached EOF after %d of %d frames", got, len(dst)/2)
			}
			t.Fatalf("ReadFrames: %v", err)
		}
	}
}

// TestFlacSeekPastEndIsSafe pins the boundary the engine can hit when a UI
// scrubs to the very end: the seek must not error or panic, and the read must
// report end of stream.
func TestFlacSeekPastEndIsSafe(t *testing.T) {
	d := openFlacFixture(t, "sine_stereo_48k.flac")

	if err := d.(Seeker).SeekFrame(12000); err != nil {
		t.Fatalf("SeekFrame(total) = %v, want a clean end-of-stream position", err)
	}
	buf := make([]float32, 512)
	if n, err := d.ReadFrames(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read after end = (%d, %v), want (0, EOF)", n, err)
	}
	if err := d.(Seeker).SeekFrame(-1); err == nil {
		t.Fatal("SeekFrame(-1) returned nil, want an error")
	}
}

// TestFlacInexactLengthStillDecodes is the regression for a silent truncation:
// a 44.1 kHz source almost never holds a sample count divisible by 147, the
// exactness condition of canonicalFrames. Discarding that function's ok bool
// defaulted the total to 0, and readCanonical clamps delivery to the declared
// total, so every real 44.1 kHz file ended at frame zero. The fix reports an
// unknown total (-1), matching the MP3 and WAV decoders, and the resampler
// delivers the whole stream.
func TestFlacInexactLengthStillDecodes(t *testing.T) {
	// 0.25 s of 44.1 kHz is 11025 samples, which happens to be exact; one more
	// sample makes the canonical total non-integral, the common real-file case.
	const samples = 11026

	pcmBytes := make([]byte, 0, samples*2*2)
	for i := range samples {
		// A small non-silent ramp, little-endian stereo int16.
		v := int16((i*97)%3000 - 1500)
		for range 2 {
			pcmBytes = append(pcmBytes, byte(v), byte(v>>8))
		}
	}

	path := filepath.Join(t.TempDir(), "inexact_44k.flac")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cfg := pcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 2}
	if err := pcm.EncodeInterleaved(f, cfg, pcmBytes); err != nil {
		f.Close()
		t.Fatalf("encode: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d, err := NewFlacFactory().Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if got := d.Info().TotalFrames; got != -1 {
		t.Fatalf("TotalFrames = %d, want -1 for an inexact canonical length", got)
	}
	if got := readFlacAll(t, d); len(got)/2 == 0 {
		t.Fatal("decoded 0 frames from a valid 44.1 kHz FLAC: delivery was clamped to a zero total")
	}
}

// TestFlacProbeReportsLength checks the prober, which the async duration
// lookup uses instead of decoding.
func TestFlacProbeReportsLength(t *testing.T) {
	info, err := NewFlacFactory().Probe(fixturePath(t, "sine_stereo_44k.flac"), ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != 12000 {
		t.Fatalf("Probe total = %d, want 12000 canonical frames", info.TotalFrames)
	}
}

// TestFlacRegistrySelection proves the factory is wired into dispatch: the
// extension resolves to it, and the magic is claimed.
func TestFlacRegistrySelection(t *testing.T) {
	path := fixturePath(t, "sine_stereo_48k.flac")

	d, err := Default.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d.Close()

	names := map[string]bool{}
	for _, c := range Default.Codecs() {
		names[c.Name] = true
		if c.Name == "flac" && (c.FriendlyName == "" || !containsExt(c.Exts, ".flac")) {
			t.Fatalf("flac codec row is malformed: %+v", c)
		}
	}
	if !names["flac"] {
		t.Fatal("flac factory is not registered")
	}

	// A file that is not FLAC must not be claimed by magic.
	if NewFlacFactory().Match([]byte("OggS")) {
		t.Fatal("flac factory claimed an Ogg stream")
	}
}

func containsExt(exts []string, want string) bool {
	for _, e := range exts {
		if e == want {
			return true
		}
	}

	return false
}

// TestFlacOpusUnaffected guards the shared decode package: adding a codec must
// not change how the Opus path resolves.
func TestFlacOpusUnaffected(t *testing.T) {
	info, err := probeOggOpus(fixturePath(t, "stereo_2s.opus"), core.DurationProbe, oggTailWindow)
	if err != nil {
		t.Fatalf("ogg opus probe: %v", err)
	}
	if info.Format.Rate != 48000 {
		t.Fatalf("opus probe rate = %d, want 48000", info.Format.Rate)
	}
}
