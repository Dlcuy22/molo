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

	"github.com/dlcuy22/molo/core"
)

// The WAV fixtures are the FLAC fixtures re-wrapped without compression, so the
// decoded PCM is bit-identical to what TestFlacIsBitExact expects. 48 kHz 16-bit
// is the native path, 24-bit exercises a wide stored sample, and 44.1 kHz
// exercises rate conversion.
const (
	wav48 = "sine_stereo_48k.wav"
	wav24 = "sine_stereo_24bit.wav"
	wav44 = "sine_stereo_44k.wav"
)

func openWavFixture(t *testing.T, name string) Decoder {
	t.Helper()

	d, err := NewWavFactory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	return d
}

// readWavAll decodes a fixture to the end and returns interleaved stereo
// float32 exactly as the decoder delivered it.
func readWavAll(t *testing.T, d Decoder) []float32 {
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

// readWavFrames reads exactly len(dst)/2 frames.
func readWavFrames(t *testing.T, d Decoder, dst []float32) {
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

// TestWavHeaderAndLength pins the stream shape: the canonical format the device
// requires and the exact frame count the header declares, rather than a count
// learned by decoding to the end.
func TestWavHeaderAndLength(t *testing.T) {
	d := openWavFixture(t, wav48)

	info := d.Info()
	if info.Format != core.CanonicalFormat {
		t.Fatalf("Format = %+v, want the canonical %+v", info.Format, core.CanonicalFormat)
	}
	// The fixture is exactly 0.25 s at 48 kHz.
	if info.TotalFrames != 12000 {
		t.Fatalf("TotalFrames = %d, want 12000", info.TotalFrames)
	}
	if got := readWavAll(t, d); len(got)/2 != 12000 {
		t.Fatalf("decoded %d frames, want 12000", len(got)/2)
	}
}

// TestWavIsBitExact is the load-bearing correctness test: WAV is uncompressed
// PCM, so the decoder must reproduce the stored samples exactly. The expected
// bits are the reference decode of the same fixture (see also
// TestWavBitDepthsAgree), so any change to scaling or channel handling shows up
// here rather than passing on RMS alone.
func TestWavIsBitExact(t *testing.T) {
	got := readWavAll(t, openWavFixture(t, wav48))

	// The first frames are silence (the sine starts at zero), then a rising
	// ramp. These are the same bits the FLAC fixture decodes to, because the two
	// files carry the same samples.
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

// TestWavBitDepthsAgree verifies that a 24-bit source produces the same PCM as
// the 16-bit source of the same signal. The fixtures are the same tones encoded
// at the two depths, so the normalized samples must match bit for bit: bit depth
// is a storage detail, not a level change. This is what catches a decoder that
// reads a 24-bit sample at the wrong width.
func TestWavBitDepthsAgree(t *testing.T) {
	got16 := readWavAll(t, openWavFixture(t, wav48))
	got24 := readWavAll(t, openWavFixture(t, wav24))

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

// TestWavRegistrySelection proves the factory is wired into dispatch: a .wav
// path resolves to this decoder, the magic is claimed, and a RIFF container that
// is not WAVE is not.
func TestWavRegistrySelection(t *testing.T) {
	path := fixturePath(t, wav48)

	d, err := Default.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := d.(*wavDecoder); !ok {
		t.Fatalf("Default.Open produced %T, want *wavDecoder", d)
	}
	d.Close()

	names := map[string]bool{}
	for _, c := range Default.Codecs() {
		names[c.Name] = true
		if c.Name == "wav" {
			if c.FriendlyName != "Wav" || c.Weight != 90 || !containsExt(c.Exts, ".wav") || !c.Default {
				t.Fatalf("wav codec row is malformed: %+v", c)
			}
		}
	}
	if !names["wav"] {
		t.Fatal("wav factory is not registered")
	}

	header, err := readHeader(path)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if !NewWavFactory().Match(header) {
		t.Fatal("wav factory did not claim the WAVE fixture")
	}
	for _, magic := range []string{"RIFF", "RF64", "BW64"} {
		if !NewWavFactory().Match([]byte(magic + "\x00\x00\x00\x00WAVE")) {
			t.Fatalf("wav factory did not claim a %s WAVE header", magic)
		}
	}
	if NewWavFactory().Match([]byte("OggS")) {
		t.Fatal("wav factory claimed an Ogg stream")
	}
	// A RIFF file that is not WAVE (here an AVI) must not be claimed: Match must
	// look past the container magic to the form type.
	if NewWavFactory().Match([]byte("RIFFxxxxAVI ")) {
		t.Fatal("wav factory claimed a RIFF AVI container")
	}
}

// TestWavSeekLandsExactly verifies the seek contract: after SeekFrame the next
// delivered frame is the requested one, matching a straight decode of the same
// position, and the remaining count runs to the declared total. A prefix is read
// first so the seek has to discard live converter and delivery state rather than
// starting from a fresh decoder.
func TestWavSeekLandsExactly(t *testing.T) {
	full := readWavAll(t, openWavFixture(t, wav48))
	const total = 12000

	for _, at := range []int64{0, 1, 960, 5000, 6000, 9000, 11000} {
		t.Run(itoa(at), func(t *testing.T) {
			d := openWavFixture(t, wav48)
			seeker, ok := d.(Seeker)
			if !ok {
				t.Fatal("wav decoder does not implement Seeker")
			}

			pre := make([]float32, 1000*2)
			readWavFrames(t, d, pre)

			if err := seeker.SeekFrame(at); err != nil {
				t.Fatalf("SeekFrame(%d): %v", at, err)
			}

			// Read every frame the position promises. A stale pos after the
			// seek would clamp this to the wrong count and hit EOF early, so
			// the read also pins the bookkeeping, not just the first samples.
			remaining := total - at
			got := make([]float32, remaining*2)
			readWavFrames(t, d, got)

			// The delivery must equal the straight decode from the same frame,
			// and only the bounded prefix is compared because the request can
			// land on the very end.
			n := min(int64(500), remaining)
			if !bytes.Equal(float32sToBytes(got[:n*2]), float32sToBytes(full[at*2:(at+n)*2])) {
				t.Fatalf("seek to %d differs from the straight decode", at)
			}

			if frames, err := d.ReadFrames(make([]float32, 2)); frames != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("after %d frames read = (%d, %v), want (0, EOF)", remaining, frames, err)
			}
		})
	}
}

// TestWavResamplesToCanonical covers the rate conversion: a 44.1 kHz file must
// come out at 48000 frames per source second, because the device rejects any
// other rate. The tone must survive the conversion at the same amplitude, which
// is what a broken interpolator would fail.
func TestWavResamplesToCanonical(t *testing.T) {
	d := openWavFixture(t, wav44)

	info := d.Info()
	if info.Format.Rate != core.CanonicalFormat.Rate {
		t.Fatalf("format rate = %d, want %d", info.Format.Rate, core.CanonicalFormat.Rate)
	}
	// 0.25 s at 44.1 kHz is 0.25 s of output: 12000 canonical frames, even
	// though the source holds 11025.
	if info.TotalFrames != 12000 {
		t.Fatalf("TotalFrames = %d, want 12000", info.TotalFrames)
	}

	got := readWavAll(t, d)
	if len(got)/2 != 12000 {
		t.Fatalf("decoded %d frames, want 12000", len(got)/2)
	}

	// The interpolator must not attenuate. The two fixtures are the same tones
	// at the same level, so the settled peaks must agree within the difference
	// the discrete sampling grid alone can explain (a few percent); a broken
	// interpolator loses far more than that.
	peak48 := peakOf(readWavAll(t, openWavFixture(t, wav48)))
	peak44 := peakOf(got)
	if peak44 < peak48*0.9 || peak44 > peak48*1.1 {
		t.Fatalf("resampled peak = %.4f against 48 kHz peak %.4f: the interpolator is attenuating", peak44, peak48)
	}
}

// TestWavSeekPastEndIsSafe pins the boundary the engine can hit when a UI scrubs
// to the very end: the seek must not error or panic, and the read must report end
// of stream.
func TestWavSeekPastEndIsSafe(t *testing.T) {
	for _, at := range []int64{12000, 20000} {
		t.Run(itoa(at), func(t *testing.T) {
			d := openWavFixture(t, wav48)

			if err := d.(Seeker).SeekFrame(at); err != nil {
				t.Fatalf("SeekFrame(%d) = %v, want a clean end-of-stream position", at, err)
			}
			buf := make([]float32, 512)
			if n, err := d.ReadFrames(buf); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("read after end = (%d, %v), want (0, EOF)", n, err)
			}
		})
	}

	d := openWavFixture(t, wav48)
	if err := d.(Seeker).SeekFrame(-1); err == nil {
		t.Fatal("SeekFrame(-1) returned nil, want an error")
	}
}

// TestWavProbeReportsLength checks the prober, which the async duration lookup
// uses instead of decoding.
func TestWavProbeReportsLength(t *testing.T) {
	info, err := NewWavFactory().Probe(fixturePath(t, wav44), ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != 12000 {
		t.Fatalf("Probe total = %d, want 12000 canonical frames", info.TotalFrames)
	}
}

// TestWavDescribe pins the diagnostics labels to the library that does the work.
func TestWavDescribe(t *testing.T) {
	d := openWavFixture(t, wav48)

	decoder, parser := Describe(d)
	if decoder != "go-wav" {
		t.Fatalf("DecoderName = %q, want %q", decoder, "go-wav")
	}
	if parser != "go-wav (riff)" {
		t.Fatalf("ParserName = %q, want %q", parser, "go-wav (riff)")
	}
}

// TestWavFloatIsConverted covers the one genuinely different branch in the
// decoder: a float source is not integer PCM, so the shared converter would
// misread its bits and the library has to turn it into integers first. Float
// fixtures are not checked in, so the file is built here; the expected output is
// the library's documented conversion (full-scale scaling with clamping, then
// the converter's 2^-15 scale).
func TestWavFloatIsConverted(t *testing.T) {
	// Two frames of interleaved float32; 0.5 and -0.25 are exactly
	// representable, and the two full-scale values exercise the clamp.
	in := []float32{0.5, -0.25, 0, 1, -1, -0.125, 0.75, 0.25}
	path := writeFloatWav(t, 48000, 2, in)

	d, err := NewWavFactory().Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if d.Info().Format != core.CanonicalFormat {
		t.Fatalf("Format = %+v, want canonical", d.Info().Format)
	}
	if d.Info().TotalFrames != 4 {
		t.Fatalf("TotalFrames = %d, want 4", d.Info().TotalFrames)
	}

	got := readWavAll(t, d)
	if len(got) != len(in) {
		t.Fatalf("decoded %d samples, want %d", len(got), len(in))
	}
	// +1.0 scales to full scale, which the signed range cannot hold, so it
	// clamps one step below; everything else round-trips within a bit.
	want := []float32{0.5, -0.25, 0, 32767.0 / 32768.0, -1, -0.125, 0.75, 0.25}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1.0/32768.0 {
			t.Fatalf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// writeFloatWav writes a minimal IEEE-float RIFF WAVE file and returns its path.
// It exists so the float branch has a source without a checked-in binary.
func writeFloatWav(t *testing.T, rate, channels int, samples []float32) string {
	t.Helper()

	var data bytes.Buffer
	for _, s := range samples {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(s))
		data.Write(b[:])
	}

	blockAlign := channels * 4
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	// 4 (WAVE) + 8 + 16 (fmt) + 8 + data, in the RIFF 32-bit size field.
	binary.Write(&buf, binary.LittleEndian, uint32(4+8+16+8+data.Len()))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(3)) // WAVE_FORMAT_IEEE_FLOAT
	binary.Write(&buf, binary.LittleEndian, uint16(channels))
	binary.Write(&buf, binary.LittleEndian, uint32(rate))
	binary.Write(&buf, binary.LittleEndian, uint32(rate*blockAlign))
	binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	binary.Write(&buf, binary.LittleEndian, uint16(32))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(data.Len()))
	buf.Write(data.Bytes())

	path := filepath.Join(t.TempDir(), "float.wav")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write float wav: %v", err)
	}

	return path
}
