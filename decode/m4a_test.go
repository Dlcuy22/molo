package decode

import (
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/dlcuy22/molo/core"
)

// readM4aAll decodes a fixture to the end and returns interleaved stereo
// float32 exactly as the decoder delivered it.
func readM4aAll(t *testing.T, d Decoder) []float32 {
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

func openM4aFixture(t *testing.T, name string) Decoder {
	t.Helper()

	d, err := NewM4aFactory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	return d
}

// TestM4aContract pins the stream contract: canonical format, the truthful
// playable length, exactly that many delivered frames, then io.EOF.
func TestM4aContract(t *testing.T) {
	d := openM4aFixture(t, "sine_stereo_48k.m4a")

	info := d.Info()
	if info.Format != core.CanonicalFormat {
		t.Fatalf("Format = %+v, want the canonical %+v", info.Format, core.CanonicalFormat)
	}
	// The fixture is exactly 0.25 s at 48 kHz, which the edit list declares as
	// a 250 ms presentation duration. The decoder's raw output is 13*1024
	// samples; the playable count is the edit list's 12000.
	if info.TotalFrames != 12000 {
		t.Fatalf("TotalFrames = %d, want 12000", info.TotalFrames)
	}

	got := readM4aAll(t, d)
	if len(got)/2 != 12000 {
		t.Fatalf("decoded %d frames, want 12000", len(got)/2)
	}

	// End of stream is sticky: another read reports EOF rather than re-emitting
	// the interpolation tail or the final frames.
	buf := make([]float32, 512)
	if n, err := d.ReadFrames(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read after end = (%d, %v), want (0, EOF)", n, err)
	}
}

// TestM4aRegistrySelection proves the factory is wired into dispatch by
// extension and by content, and that Open resolves to this decoder.
func TestM4aRegistrySelection(t *testing.T) {
	path := fixturePath(t, "sine_stereo_48k.m4a")

	d, err := Default.Open(path)
	if err != nil {
		t.Fatalf("Default.Open: %v", err)
	}
	defer d.Close()

	if _, ok := d.(*m4aDecoder); !ok {
		t.Fatalf("Default.Open produced %T, want *m4aDecoder", d)
	}
	decoder, parser := Describe(d)
	if decoder != "go-aac" || parser != "go-m4a (mp4/aac-lc)" {
		t.Fatalf("Describe = %q/%q, want go-aac/go-m4a (mp4/aac-lc)", decoder, parser)
	}

	// The fixture's first bytes are an ISO-BMFF box size followed by "ftyp".
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	magic := make([]byte, 8)
	if _, err := io.ReadFull(f, magic); err != nil {
		t.Fatalf("read magic: %v", err)
	}
	f.Close()

	factory := NewM4aFactory()
	if !factory.Match(magic) {
		t.Fatalf("Match(% x) = false, want true", magic)
	}
	if factory.Match([]byte("OggS")) {
		t.Fatal("m4a factory claimed an Ogg stream")
	}
	if factory.Match([]byte("fLaC")) {
		t.Fatal("m4a factory claimed a native FLAC stream")
	}
	if factory.Match([]byte("ftyp")) {
		t.Fatal("m4a factory claimed a short header with ftyp at offset 0")
	}

	friendly, weight := ProfileOf(factory)
	if friendly != "M4a" || weight != 90 {
		t.Fatalf("profile = %q/%d, want M4a/90", friendly, weight)
	}
}

// TestM4aProbeReportsLength checks the prober, which the async duration lookup
// uses instead of decoding.
func TestM4aProbeReportsLength(t *testing.T) {
	info, err := NewM4aFactory().Probe(fixturePath(t, "sine_stereo_48k.m4a"), ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != 12000 {
		t.Fatalf("Probe total = %d, want 12000 canonical frames", info.TotalFrames)
	}
}

// TestM4aQualitySanity is the lossy quality check. AAC is not bit-exact, so
// the test pins what a correct decode must show: a real tone at the expected
// frequency in each channel, an in-range level, and the encoder priming gone.
func TestM4aQualitySanity(t *testing.T) {
	got := readM4aAll(t, openM4aFixture(t, "sine_stereo_48k.m4a"))
	if len(got) < 4096 {
		t.Fatalf("decoded only %d samples", len(got))
	}

	// Priming: the raw decoder emits 1024 near-silent priming samples before
	// the signal. The first 1024 delivered frames must be the tone's opening
	// cycles, not that silence. (A decode that reports 12000 but forgets to
	// skip the delay reads here as RMS 0.0025.)
	lead := got[:1024*2]
	if rms := m4aRMS(lead); rms < 0.15 {
		t.Fatalf("first 1024 frames RMS = %.4f, want > 0.15: encoder priming was not skipped", rms)
	}

	// Level: the fixture was mastered at volume 4.0, so it sits well above
	// silence and below full scale.
	if rms := m4aRMS(got); rms < 0.1 || rms > 0.6 {
		t.Fatalf("RMS = %.4f, want 0.1..0.6", rms)
	}

	// Tones and channels: 440 Hz on the left, 660 Hz on the right. A correct
	// decode concentrates energy at the assigned frequency; noise or a swapped
	// channel fails the dominance check.
	left := m4aChannel(got, 0)
	right := m4aChannel(got, 1)
	if got, want := m4aGoertzel(left, 48000, 440), m4aGoertzel(left, 48000, 660); got < 3*want {
		t.Fatalf("left channel 440 Hz magnitude %.4f vs 660 Hz %.4f: not the left tone", got, want)
	}
	if got, want := m4aGoertzel(right, 48000, 660), m4aGoertzel(right, 48000, 440); got < 3*want {
		t.Fatalf("right channel 660 Hz magnitude %.4f vs 440 Hz %.4f: not the right tone", got, want)
	}

	if peak := peakOf(got); peak < 0.2 || peak > 1.0 {
		t.Fatalf("peak = %.4f, want 0.2..1.0", peak)
	}
}

// TestM4aIsASeeker pins the native-seek capability. The container's sample table
// gives every access unit an offset, so the adapter can rebuild the reader
// cursor and land on the unit at or before the target instead of forcing the
// streamer's reopen-and-discard fallback.
func TestM4aIsASeeker(t *testing.T) {
	d := openM4aFixture(t, "sine_stereo_48k.m4a")

	if _, ok := d.(Seeker); !ok {
		t.Fatal("m4a decoder does not implement Seeker, but the sample table supports a native seek")
	}
}

// The fixture's real geometry, read from the container: 13 access units of 1024
// samples with a 1024-sample encoder delay, presented as 12000 playable frames.
const (
	m4aFixtureAUs   = 13
	m4aAUSamples    = 1024
	m4aEncoderDelay = 1024
)

// m4aLandingFrame is the first playable frame a seek to target delivers. The
// decoder lands on the access unit at or before the target and reports that
// unit's playable start, which is its source sample minus the encoder delay.
func m4aLandingFrame(target int64) int64 {
	au := (target + m4aEncoderDelay) / m4aAUSamples
	if au > m4aFixtureAUs-1 {
		au = m4aFixtureAUs - 1
	}
	if au < 0 {
		au = 0
	}
	landing := au*m4aAUSamples - m4aEncoderDelay
	if landing < 0 {
		landing = 0
	}

	return landing
}

// TestM4aSeekMatchesStraightDecode proves the seek lands on the access unit at
// or before the target and delivers what a straight decode holds there. AAC-LC
// cannot start mid-unit, so the landing may sit up to one unit before the
// target; the comparison uses the landing the decoder itself reports. The PCM
// is not bit-identical because AAC threads state across frames, so a small
// per-sample tolerance absorbs the drift, far below the signal.
func TestM4aSeekMatchesStraightDecode(t *testing.T) {
	const (
		window    = 4000
		tolerance = 0.02
	)
	full := readM4aAll(t, openM4aFixture(t, "sine_stereo_48k.m4a"))
	total := int64(len(full) / 2)
	if total != 12000 {
		t.Fatalf("straight decode delivered %d frames, want 12000", total)
	}

	for _, at := range []int64{0, 2048, 4096, 4097, 9000} {
		t.Run(itoa(at), func(t *testing.T) {
			d := openM4aFixture(t, "sine_stereo_48k.m4a")
			if err := d.(Seeker).SeekFrame(at); err != nil {
				t.Fatalf("SeekFrame(%d): %v", at, err)
			}

			landing := m4aLandingFrame(at)
			w := min(int64(window), total-landing)
			got := readExactlyFrames(t, d, int(w))
			if !windowWithin(full, got, landing, tolerance) {
				t.Fatalf("seek to %d landed at %d, but the window does not match the straight decode there",
					at, landing)
			}
		})
	}
}

// TestM4aSeekBackwardsAndToEnd walks the seek backwards, past the end, and
// negative, which is where a stale reader cursor or a missed reset would show.
func TestM4aSeekBackwardsAndToEnd(t *testing.T) {
	const (
		window    = 4000
		tolerance = 0.02
	)
	full := readM4aAll(t, openM4aFixture(t, "sine_stereo_48k.m4a"))
	total := int64(len(full) / 2)

	d := openM4aFixture(t, "sine_stereo_48k.m4a")
	seeker := d.(Seeker)

	if err := seeker.SeekFrame(9000); err != nil {
		t.Fatalf("SeekFrame(9000): %v", err)
	}
	if err := seeker.SeekFrame(2048); err != nil {
		t.Fatalf("SeekFrame(2048): %v", err)
	}
	got := readExactlyFrames(t, d, window)
	if !windowWithin(full, got, 2048, tolerance) {
		t.Fatal("backwards seek did not restore the earlier position")
	}

	// A target past the end lands on the last unit and delivers its tail.
	landing := m4aLandingFrame(total + 10*m4aAUSamples)
	if err := seeker.SeekFrame(total + 10*m4aAUSamples); err != nil {
		t.Fatalf("SeekFrame(past end): %v", err)
	}
	tail := readM4aAll(t, d)
	if int64(len(tail)/2) != total-landing {
		t.Fatalf("after seek past the end delivered %d frames, want the %d-frame tail",
			len(tail)/2, total-landing)
	}
	if !windowWithin(full, tail, landing, tolerance) {
		t.Fatal("the tail after a past-the-end seek does not match the straight decode")
	}

	if err := seeker.SeekFrame(-1); err == nil {
		t.Fatal("SeekFrame(-1) succeeded, want an error")
	}
}

// TestM4aSeekDoesNotReapplyPriming proves the encoder-priming trim is spent
// once. The landing unit is already past the priming, so a seek must not drop
// another delay worth of samples: the first frames must be the tone, not silence.
func TestM4aSeekDoesNotReapplyPriming(t *testing.T) {
	d := openM4aFixture(t, "sine_stereo_48k.m4a")
	if err := d.(Seeker).SeekFrame(0); err != nil {
		t.Fatalf("SeekFrame(0): %v", err)
	}

	lead := readExactlyFrames(t, d, m4aEncoderDelay)
	if rms := m4aRMS(lead); rms < 0.15 {
		t.Fatalf("first %d frames after seek RMS = %.4f, want > 0.15: priming was re-applied",
			m4aEncoderDelay, rms)
	}
}

// TestM4aRejectsNonMP4WithM4aExtension is the degradation case: a file with an
// .m4a name that is not an MP4 container (here a raw ADTS AAC stream) must fail
// with an error, not panic or mis-decode.
func TestM4aRejectsNonMP4WithM4aExtension(t *testing.T) {
	body, err := os.ReadFile(fixturePath(t, "sine_stereo_48k.aac"))
	if err != nil {
		t.Fatalf("read aac fixture: %v", err)
	}

	dir := t.TempDir()
	path := writeFile(t, dir, "not-really.m4a", body)

	t.Run("factory", func(t *testing.T) {
		if _, err := NewM4aFactory().Open(path); err == nil {
			t.Fatal("Open on a non-MP4 .m4a returned nil error")
		} else if !strings.Contains(err.Error(), "M4A") {
			t.Fatalf("error %q does not name the M4A opener", err)
		}
	})

	t.Run("registry", func(t *testing.T) {
		// The registry resolves by extension first, but only trusts the match
		// when the magic does not contradict it. An ADTS stream in an .m4a file
		// contradicts the M4A extension, so the extension is rejected and the
		// magic lookup runs instead, which routes it to the AAC decoder. That is
		// the registry recovering a mislabelled file, and it must work: the
		// alternative is handing an ADTS stream to the MP4 parser.
		d, err := Default.Open(path)
		if err != nil {
			t.Fatalf("registry did not recover the mislabelled ADTS stream: %v", err)
		}
		defer d.Close()

		if name := d.(Descriptor).DecoderName(); name != "go-aac" {
			t.Fatalf("recovered stream went to %q, want the AAC decoder", name)
		}
	})
}

func m4aChannel(samples []float32, ch int) []float32 {
	out := make([]float32, 0, len(samples)/2)
	for i := ch; i < len(samples); i += 2 {
		out = append(out, samples[i])
	}

	return out
}

func m4aRMS(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}

	return math.Sqrt(sum / float64(len(samples)))
}

// m4aGoertzel returns the normalized magnitude of hz in samples at rate. It is
// enough to distinguish a real tone from noise or the wrong channel.
func m4aGoertzel(samples []float32, rate int, hz float64) float64 {
	w := 2 * math.Pi * hz / float64(rate)
	c := 2 * math.Cos(w)
	var s0, s1, s2 float64
	for _, s := range samples {
		s0 = float64(s) + c*s1 - s2
		s2, s1 = s1, s0
	}
	re := s1 - s2*math.Cos(w)
	im := s2 * math.Sin(w)

	return math.Hypot(re, im) / float64(len(samples)) * 2
}
