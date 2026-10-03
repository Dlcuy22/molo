package decode

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
)

// readMp3All decodes a fixture to the end and returns interleaved stereo
// float32 exactly as the decoder delivered it.
func readMp3All(t *testing.T, d Decoder) []float32 {
	t.Helper()

	buf := make([]float32, 4096)
	var out []float32
	for {
		n, err := d.ReadFrames(buf)
		out = append(out, buf[:n*2]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}

			t.Fatalf("ReadFrames: %v", err)
		}
	}
}

// readMp3Frames reads exactly len(dst)/2 frames.
func readMp3Frames(t *testing.T, d Decoder, dst []float32) {
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

func openMp3Fixture(t *testing.T, name string) Decoder {
	t.Helper()

	d, err := NewMp3Factory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	return d
}

// mp3RMS is the root mean square over every interleaved sample.
func mp3RMS(samples []float32) float64 {
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	if len(samples) == 0 {
		return 0
	}

	return math.Sqrt(sum / float64(len(samples)))
}

// mp3GoertzelAmplitude estimates the amplitude of one frequency in one channel
// over the whole buffer. It is the lossy-domain replacement for an exact FFT:
// enough to tell a real tone from silence or noise, without a dependency.
func mp3GoertzelAmplitude(samples []float32, channel, freq, rate int) float64 {
	frames := len(samples) / 2
	if frames == 0 {
		return 0
	}
	w := 2 * math.Pi * float64(freq) / float64(rate)
	c := 2 * math.Cos(w)

	var s0, s1, s2 float64
	for i := range frames {
		s0 = float64(samples[i*2+channel]) + c*s1 - s2
		s2 = s1
		s1 = s0
	}

	return math.Sqrt(s1*s1+s2*s2-c*s1*s2) / float64(frames)
}

// TestMp3HeaderAndLength pins the stream shape and the declared length. The
// fixture is 0.25 s of stereo at 48 kHz, so the ideal playable count is 12000;
// the decoder reports 13824 because the ffmpeg Info tag that accompanies this
// file carries no LAME delay/padding fields, so the library has no gapless
// window to trim. The encoder's algorithmic delay plus its padding therefore
// stay in the output. 13824 is exactly 12 Layer III frames of 1152 samples, the
// tag's own frame count, so the number is the real decodable length rather than
// an error, and the tolerance below covers any future re-encode's padding.
func TestMp3HeaderAndLength(t *testing.T) {
	d := openMp3Fixture(t, "sine_stereo_48k.mp3")

	info := d.Info()
	if info.Format != core.CanonicalFormat {
		t.Fatalf("Format = %+v, want the canonical %+v", info.Format, core.CanonicalFormat)
	}
	const ideal = 12000
	if info.TotalFrames < ideal || info.TotalFrames > ideal*6/5 {
		t.Fatalf("TotalFrames = %d, want within 20%% of %d", info.TotalFrames, ideal)
	}

	got := readMp3All(t, d)
	if int64(len(got)/2) != info.TotalFrames {
		t.Fatalf("decoded %d frames, Info promised %d", len(got)/2, info.TotalFrames)
	}

	// A second read after the stream is exhausted must remain EOF, not restart.
	buf := make([]float32, 64)
	if n, err := d.ReadFrames(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read after end = (%d, %v), want (0, EOF)", n, err)
	}
}

// TestMp3RegistrySelection proves the factory is wired into dispatch and that
// the magic check accepts both an MPEG frame sync and an ID3 prefix. The ID3
// case matters when a tagged file has no usable extension: a tag starts with
// "ID3", so without claiming it content sniffing would find no frame sync at
// the start and reject the file.
func TestMp3RegistrySelection(t *testing.T) {
	path := fixturePath(t, "sine_stereo_48k.mp3")

	d, err := Default.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := d.(*mp3Decoder); !ok {
		t.Fatalf("Default.Open produced %T, want the *mp3Decoder", d)
	}
	d.Close()

	names := map[string]bool{}
	for _, c := range Default.Codecs() {
		names[c.Name] = true
		if c.Name == "mp3" && (c.FriendlyName == "" || !containsExt(c.Exts, ".mp3")) {
			t.Fatalf("mp3 codec row is malformed: %+v", c)
		}
	}
	if !names["mp3"] {
		t.Fatal("mp3 factory is not registered")
	}

	f := NewMp3Factory()
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !f.Match(header) {
		t.Fatal("mp3 factory did not claim the ID3-prefixed fixture")
	}
	if !f.Match([]byte("ID3\x04\x00\x00\x00\x00\x00\x00")) {
		t.Fatal("mp3 factory did not claim a synthetic ID3 tag")
	}
	if !f.Match([]byte{0xFF, 0xFB, 0x90, 0x00}) {
		t.Fatal("mp3 factory did not claim a bare MPEG-1 Layer III sync")
	}
	if f.Match([]byte("OggS")) {
		t.Fatal("mp3 factory claimed an Ogg stream")
	}
	// Layer II sync: the layer bits are 10, so this is not an MP3 we decode.
	if f.Match([]byte{0xFF, 0xF5, 0x90, 0x00}) {
		t.Fatal("mp3 factory claimed a non-Layer-III frame")
	}

	// A tagged file with no mp3 extension must resolve by magic alone, which is
	// the case the ID3 branch exists for.
	dir := t.TempDir()
	sniff := filepath.Join(dir, "tagged.bin")
	if err := os.WriteFile(sniff, header, 0o644); err != nil {
		t.Fatalf("write sniff fixture: %v", err)
	}
	sniffed, err := Default.Open(sniff)
	if err != nil {
		t.Fatalf("Open by ID3 magic: %v", err)
	}
	sniffed.Close()
}

// TestMp3QualityTone checks the decode is real audio: a settled RMS in a
// plausible range and the 440/660 Hz tones on their own channels. Lossy coding
// leaves plenty of slop, so this measures dominance rather than exact samples.
func TestMp3QualityTone(t *testing.T) {
	got := readMp3All(t, openMp3Fixture(t, "sine_stereo_48k.mp3"))
	if len(got) == 0 {
		t.Fatal("no audio decoded")
	}

	rms := mp3RMS(got)
	if rms < 0.1 || rms > 0.6 {
		t.Fatalf("RMS = %.4f, want roughly 0.1..0.6", rms)
	}

	left440 := mp3GoertzelAmplitude(got, 0, 440, 48000)
	left660 := mp3GoertzelAmplitude(got, 0, 660, 48000)
	right660 := mp3GoertzelAmplitude(got, 1, 660, 48000)
	right440 := mp3GoertzelAmplitude(got, 1, 440, 48000)

	const floor = 0.02
	if left440 < floor || right660 < floor {
		t.Fatalf("expected tones missing: L440=%.4f R660=%.4f", left440, right660)
	}
	// The tones are isolated: each channel's own tone must dominate the other
	// channel's frequency, which a channel swap or a noise-only decode fails.
	if left440 < 5*left660 {
		t.Fatalf("left channel is not 440 Hz dominant: 440=%.4f 660=%.4f", left440, left660)
	}
	if right660 < 5*right440 {
		t.Fatalf("right channel is not 660 Hz dominant: 660=%.4f 440=%.4f", right660, right440)
	}
}

// TestMp3SeekMatchesStraightDecode verifies the seek contract. go-mp3's
// SeekToSample walks frame headers and primes the bit reservoir and MDCT
// overlap with sixteen lead-in frames before dropping the intra-frame samples,
// so a seek lands on the exact requested sample rather than merely the frame
// that contains it. At 48 kHz a canonical frame is a source sample, so the
// comparison against a straight decode is byte-exact.
func TestMp3SeekMatchesStraightDecode(t *testing.T) {
	full := readMp3All(t, openMp3Fixture(t, "sine_stereo_48k.mp3"))

	for _, at := range []int64{0, 1, 960, 5000, 9000, 12000, 13000} {
		t.Run(itoa(at), func(t *testing.T) {
			d := openMp3Fixture(t, "sine_stereo_48k.mp3")
			seeker, ok := d.(Seeker)
			if !ok {
				t.Fatal("mp3 decoder does not implement Seeker")
			}
			if err := seeker.SeekFrame(at); err != nil {
				t.Fatalf("SeekFrame(%d): %v", at, err)
			}

			frames := min(int64(500), int64(len(full)/2)-at)
			got := make([]float32, frames*2)
			readMp3Frames(t, d, got)

			want := full[at*2 : (at+frames)*2]
			for i := range got {
				if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
					t.Fatalf("seek to %d differs at sample %d: got %v want %v", at, i, got[i], want[i])
				}
			}
		})
	}
}

// TestMp3SeekPastEndIsSafe pins the boundary a scrub to the end can hit: the
// seek must not error or panic and the read must report end of stream.
func TestMp3SeekPastEndIsSafe(t *testing.T) {
	d := openMp3Fixture(t, "sine_stereo_48k.mp3")
	total := d.Info().TotalFrames

	if err := d.(Seeker).SeekFrame(total); err != nil {
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

// TestMp3ResamplesToCanonical covers the 44.1 kHz path, the rate every MP3
// encoder defaults to. The shared converter must lift 12672 source frames to
// the canonical 48 kHz domain and keep the tones intact. 12672 source samples
// at 44100 is not an integer number of 48000 frames, so canonicalFrames cannot
// state a total exactly and Info must report -1 rather than a rounded one; the
// decoded count is what the resampler actually emits.
func TestMp3ResamplesToCanonical(t *testing.T) {
	d := openMp3Fixture(t, "sine_stereo_44k.mp3")

	info := d.Info()
	if info.Format.Rate != core.CanonicalFormat.Rate {
		t.Fatalf("format rate = %d, want %d", info.Format.Rate, core.CanonicalFormat.Rate)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1 for a total that does not divide exactly", info.TotalFrames)
	}
	// 12672 samples at 44100 is the exact source-domain length, and it is what
	// gives the stream a duration when TotalFrames cannot.
	if info.SourceSamples != 12672 || info.SourceRate != 44100 {
		t.Fatalf("source length = %d/%d, want 12672/44100", info.SourceSamples, info.SourceRate)
	}
	if info.Duration() <= 0 {
		t.Fatalf("Duration() = %v, want a positive source-domain duration", info.Duration())
	}

	got := readMp3All(t, d)
	// 12672 * 48000 / 44100 = 13792.65, so 13792 or 13793 frames are expected.
	if len(got)/2 < 13790 || len(got)/2 > 13795 {
		t.Fatalf("decoded %d frames, want about 13792", len(got)/2)
	}

	left440 := mp3GoertzelAmplitude(got, 0, 440, 48000)
	right660 := mp3GoertzelAmplitude(got, 1, 660, 48000)
	left660 := mp3GoertzelAmplitude(got, 0, 660, 48000)
	right440 := mp3GoertzelAmplitude(got, 1, 440, 48000)
	const floor = 0.02
	if left440 < floor || right660 < floor {
		t.Fatalf("resampled tones missing: L440=%.4f R660=%.4f", left440, right660)
	}
	if left440 < 5*left660 || right660 < 5*right440 {
		t.Fatalf("resampled channels are not tone-dominant: L440=%.4f L660=%.4f R660=%.4f R440=%.4f",
			left440, left660, right660, right440)
	}
}

// TestMp3ProbeReportsLength checks the prober, which the async duration lookup
// uses instead of decoding. The 48 kHz fixture's Info tag carries an exact
// frame count, so the total is exact.
func TestMp3ProbeReportsLength(t *testing.T) {
	info, err := NewMp3Factory().Probe(fixturePath(t, "sine_stereo_48k.mp3"), ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Format != core.CanonicalFormat {
		t.Fatalf("Format = %+v, want the canonical %+v", info.Format, core.CanonicalFormat)
	}
	// 12 audio frames of 1152 samples at 48 kHz.
	if info.TotalFrames != 13824 {
		t.Fatalf("Probe total = %d, want 13824 canonical frames", info.TotalFrames)
	}
}

// TestMp3ProbeReportsSourceDuration checks the 44.1 kHz probe, where the tag
// states 12672 source samples. That count does not divide evenly into 48 kHz
// frames, so TotalFrames stays -1 while the source-domain duration is exact.
func TestMp3ProbeReportsSourceDuration(t *testing.T) {
	info, err := NewMp3Factory().Probe(fixturePath(t, "sine_stereo_44k.mp3"), ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1 for a total that does not divide exactly", info.TotalFrames)
	}
	if info.SourceSamples != 12672 || info.SourceRate != 44100 {
		t.Fatalf("source length = %d/%d, want 12672/44100", info.SourceSamples, info.SourceRate)
	}
	if want := time.Duration(12672) * time.Second / 44100; info.Duration() != want {
		t.Fatalf("Duration() = %v, want %v", info.Duration(), want)
	}
}

// TestMp3DescriptorNames checks the diagnostic labels a UI reads.
func TestMp3DescriptorNames(t *testing.T) {
	d := openMp3Fixture(t, "sine_stereo_48k.mp3")
	decoder, parser := Describe(d)
	if decoder != "go-mp3" {
		t.Fatalf("DecoderName = %q, want go-mp3", decoder)
	}
	if parser != "go-mp3 (mpeg audio)" {
		t.Fatalf("ParserName = %q, want go-mp3 (mpeg audio)", parser)
	}
}
