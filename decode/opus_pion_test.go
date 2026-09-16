package decode

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"testing"

	"github.com/dlcuy22/player/core"
)

// decodeAll drains a decoder into interleaved float32 frames.
func decodeAll(t *testing.T, d Decoder) []float32 {
	t.Helper()

	buf := make([]float32, 4096)
	var out []float32
	for {
		n, err := d.ReadFrames(buf)
		if n > 0 {
			out = append(out, buf[:n*2]...)
		}
		if errorsIsEOF(err) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadFrames: %v", err)
		}
		if n == 0 {
			t.Fatalf("ReadFrames returned no frames and no error")
		}
	}
}

func errorsIsEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

func TestPionOpenReportsUnknownTotal(t *testing.T) {
	d, err := NewPionOpusFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	info := d.Info()
	if info.TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1 (a forward-only decoder cannot know)", info.TotalFrames)
	}
	if want := (core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}); info.Format != want {
		t.Fatalf("Format = %+v, want %+v", info.Format, want)
	}
}

func TestPionDecodeFrameCountsForAllFixtures(t *testing.T) {
	tests := []struct {
		fixture    string
		wantFrames int64
	}{
		// Pre-skip is discarded, so the decoded length matches the granule math.
		{"stereo_2s.opus", 96000},
		{"mono_1s.opus", 48000},
		{"short_stereo.opus", 12000},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			d, err := NewPionOpusFactory().Open(fixturePath(t, tt.fixture))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()

			got := decodeAll(t, d)
			frames := int64(len(got) / 2)
			// The final packet may extend past the granule: at most one 120 ms
			// packet of padding, never a short stream.
			if frames < tt.wantFrames || frames > tt.wantFrames+5760 {
				t.Fatalf("decoded %d frames, want within [%d, %d]", frames, tt.wantFrames, tt.wantFrames+5760)
			}
		})
	}
}

func TestPionDecodeIsNotSilentOrClipped(t *testing.T) {
	d, err := NewPionOpusFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	pcm := decodeAll(t, d)
	// Forward-only decoding cannot know where the granule-truncated end is, so
	// the last packet's padding may survive: at most one 120 ms packet.
	if frames := len(pcm) / 2; frames < 96000 || frames > 96000+5760 {
		t.Fatalf("decoded %d frames, want within [96000, %d]", frames, 96000+5760)
	}
	var peak float64
	for _, s := range pcm {
		if a := abs64(float64(s)); a > peak {
			peak = a
		}
	}
	// A full-scale 440 Hz + 660 Hz sine pair. Silence, gain errors, and clipping
	// all show up here.
	if peak < 0.3 || peak > 0.9 {
		t.Fatalf("peak sample = %.4f, want a healthy non-clipped signal", peak)
	}
}

func TestPionDecodeIsDeterministicBytes(t *testing.T) {
	first := decodeFixture(t, "stereo_2s.opus")
	second := decodeFixture(t, "stereo_2s.opus")
	if !bytes.Equal(float32sToBytes(first), float32sToBytes(second)) {
		t.Fatal("two decodes of the same file differ")
	}
}

func TestPionStereoAndMonoShareLayout(t *testing.T) {
	// The engine currency is always 48k/2ch; a mono source must be duplicated
	// into both channels rather than dropped or interleaved wrongly.
	factory := NewPionOpusFactory()
	d, err := factory.Open(fixturePath(t, "mono_1s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if d.Info().Format.Ch != 2 {
		t.Fatalf("mono source opened as %d channels, want 2", d.Info().Format.Ch)
	}
	pcm := decodeAll(t, d)
	for i := 0; i+1 < len(pcm); i += 2 {
		if pcm[i] != pcm[i+1] {
			t.Fatalf("frame %d is not duplicated: L=%v R=%v", i/2, pcm[i], pcm[i+1])
		}
	}
}

func TestPionOpenReaderMatchesPath(t *testing.T) {
	fromPath := decodeFixture(t, "stereo_2s.opus")

	f, err := os.Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	d, err := NewPionOpusFactory().OpenReader(f)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer d.Close()

	fromReader := decodeAll(t, d)
	if !bytes.Equal(float32sToBytes(fromPath), float32sToBytes(fromReader)) {
		t.Fatal("OpenReader and Open disagree on the same file")
	}
}

// oneShotReader hides io.Seeker, standing in for a network or pipe source.
type oneShotReader struct{ r io.Reader }

func (o oneShotReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func TestPionOpenReaderAcceptsNonSeekable(t *testing.T) {
	f, err := os.Open(fixturePath(t, "short_stereo.opus"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	d, err := NewPionOpusFactory().OpenReader(oneShotReader{f})
	if err != nil {
		t.Fatalf("OpenReader on a non-seekable stream: %v", err)
	}
	defer d.Close()

	if d.Info().TotalFrames != -1 {
		t.Fatalf("TotalFrames = %d, want -1", d.Info().TotalFrames)
	}
	if frames := len(decodeAll(t, d)) / 2; frames < 12000 {
		t.Fatalf("decoded %d frames from a non-seekable stream, want at least 12000", frames)
	}
}

func TestPionSeekOnNonSeekableReaderFails(t *testing.T) {
	f, err := os.Open(fixturePath(t, "short_stereo.opus"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	d, err := NewPionOpusFactory().OpenReader(oneShotReader{f})
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer d.Close()

	if err := d.(Seeker).SeekFrame(1000); err == nil {
		t.Fatal("SeekFrame succeeded on a non-seekable stream")
	}
}

func TestPionSeekMatchesPrefixOfFullDecode(t *testing.T) {
	factory := NewPionOpusFactory()
	full := decodeFixture(t, "stereo_2s.opus")

	d, err := factory.Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	seeker, ok := d.(Seeker)
	if !ok {
		t.Fatal("pion decoder does not implement Seeker")
	}
	const at = 30000
	if err := seeker.SeekFrame(at); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}
	got := decodeAll(t, d)[:48000*2]
	want := full[at*2 : (at+48000)*2]
	if !bytes.Equal(float32sToBytes(got), float32sToBytes(want)) {
		t.Fatal("seek output differs from the same window of a straight-through decode")
	}

	if err := seeker.SeekFrame(0); err != nil {
		t.Fatalf("SeekFrame back to zero: %v", err)
	}
	if got := decodeAll(t, d); !bytes.Equal(float32sToBytes(got[:1000*2]), float32sToBytes(full[:1000*2])) {
		t.Fatal("seeking back to the start did not restore the stream head")
	}
}

func TestPionSeekPastEndReportsError(t *testing.T) {
	d, err := NewPionOpusFactory().Open(fixturePath(t, "short_stereo.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	seeker := d.(Seeker)
	if err := seeker.SeekFrame(-1); err == nil {
		t.Fatal("SeekFrame(-1) returned nil")
	}
	if err := seeker.SeekFrame(1 << 30); err == nil {
		t.Fatal("SeekFrame past the end returned nil")
	}
}

func TestPionCloseIsIdempotentAndStopsReads(t *testing.T) {
	d, err := NewPionOpusFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := d.ReadFrames(make([]float32, 128)); err == nil {
		t.Fatal("ReadFrames after Close returned nil error")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPionAppliesHeaderOutputGain(t *testing.T) {
	// A -6 dB header gain must scale the decoded PCM by 10^(-6/20), so a file
	// with the field patched decodes to exactly that fraction of the original.
	const gainQ78 = -6 * 256
	gain := float32(math.Pow(10, float64(gainQ78)/(20*256)))

	base := decodeFixture(t, "stereo_2s.opus")
	path := patchOpusHeadGain(t, fixturePath(t, "stereo_2s.opus"), "gain.opus", gainQ78)

	d, err := NewPionOpusFactory().Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	gained := decodeAll(t, d)
	if len(gained) < len(base) {
		t.Fatalf("gained decode produced %d samples, want at least %d", len(gained), len(base))
	}

	var maxAbs, maxScale float64
	for i := range base {
		if base[i] == 0 {
			continue
		}
		if a := abs64(float64(gained[i] - base[i]*gain)); a > maxAbs {
			maxAbs = a
		}
		if s := abs64(float64(gained[i] / base[i])); abs64(s-float64(gain)) > maxScale {
			maxScale = abs64(s - float64(gain))
		}
	}
	if maxScale > 1e-3 {
		t.Fatalf("gain factor is off by %.5f, want %.5f", maxScale, gain)
	}
	if maxAbs > 1e-5 {
		t.Fatalf("gain-scaled samples differ by %.2e from the exact factor", maxAbs)
	}
}

func TestPionRejectsNonOpus(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"plain.opus":  []byte("this is not an ogg file at all, not even close"),
		"vorbis.opus": func() []byte { b := oggPage(0, 0x02, 0, []byte("\x01vorbis")); return b }(),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, dir, name, body)
			if _, err := NewPionOpusFactory().Open(path); err == nil {
				t.Fatal("Open accepted a non-Opus file")
			}
		})
	}
}

func TestPionFactoryContract(t *testing.T) {
	f := NewPionOpusFactory()
	if f.Name() == "" {
		t.Fatal("Name() is empty")
	}
	if !slices.Contains(f.Exts(), ".opus") {
		t.Fatalf("Exts() = %v, want .opus", f.Exts())
	}
	if !slices.Contains(f.Exts(), ".ogg") {
		t.Fatalf("Exts() = %v, want .ogg", f.Exts())
	}
	if !f.Match([]byte("OggS\x00\x02")) {
		t.Fatal("Match rejected an OggS header")
	}
	if f.Match([]byte("RIFF....WAVE")) {
		t.Fatal("Match accepted a WAVE header")
	}
	if f.Match([]byte("Ogg")) {
		t.Fatal("Match accepted a truncated header")
	}
}

func decodeFixture(t *testing.T, name string) []float32 {
	t.Helper()

	d, err := NewPionOpusFactory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	defer d.Close()

	return decodeAll(t, d)
}

func float32sToBytes(in []float32) []byte {
	out := make([]byte, 4*len(in))
	for i, v := range in {
		bits := float32bits(v)
		out[4*i] = byte(bits)
		out[4*i+1] = byte(bits >> 8)
		out[4*i+2] = byte(bits >> 16)
		out[4*i+3] = byte(bits >> 24)
	}

	return out
}
