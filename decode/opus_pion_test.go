package decode

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
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

// TestPionOpenReportsTotalFromGranuleIndex pins the decision to report the real
// total now that the page index is built at open. The index walk costs about a
// millisecond on the 139 s reference file, which the decoder already paid to
// enable seeking, so the total is free rather than an extra pass.
func TestPionOpenReportsTotalFromGranuleIndex(t *testing.T) {
	d, err := NewPionOpusFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	info := d.Info()
	// Granule 96312 minus the 312-sample pre-skip. It is the playable length,
	// not the number of frames ReadFrames will deliver: the final packet may
	// extend past the granule, exactly as the probe reports.
	if info.TotalFrames != 96000 {
		t.Fatalf("TotalFrames = %d, want 96000 from the granule index", info.TotalFrames)
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

// TestPionOpenReaderAcceptsNonSeekable proves a stream without io.Seeker still
// plays forward, and that it decodes to exactly the same PCM as the seekable
// path. The forward-only reader is a second container implementation, so byte
// equality with the indexed reader is the check that it parses packets the same.
func TestPionOpenReaderAcceptsNonSeekable(t *testing.T) {
	fromPath := decodeFixture(t, "short_stereo.opus")

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
		t.Fatalf("TotalFrames = %d, want -1 (a forward-only source cannot know)", d.Info().TotalFrames)
	}
	fromReader := decodeAll(t, d)
	if len(fromReader) < 12000*2 {
		t.Fatalf("decoded %d frames from a non-seekable stream, want at least 12000", len(fromReader)/2)
	}
	if !bytes.Equal(float32sToBytes(fromPath), float32sToBytes(fromReader)) {
		t.Fatal("non-seekable OpenReader and Open disagree on the same file")
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

// TestForwardOggOpusMatchesSeekableOnFixtures exercises every fixture on the
// forward-only reader and requires byte-identical PCM to the indexed reader.
// Mono and the short file cover the different page counts and layouts; a
// container bug in the fallback would show up as a divergence.
func TestForwardOggOpusMatchesSeekableOnFixtures(t *testing.T) {
	for _, name := range []string{"stereo_2s.opus", "mono_1s.opus", "short_stereo.opus"} {
		t.Run(name, func(t *testing.T) {
			want := decodeFixture(t, name)

			f, err := os.Open(fixturePath(t, name))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()

			d, err := NewPionOpusFactory().OpenReader(oneShotReader{f})
			if err != nil {
				t.Fatalf("OpenReader: %v", err)
			}
			defer d.Close()

			got := decodeAll(t, d)
			if !bytes.Equal(float32sToBytes(want), float32sToBytes(got)) {
				t.Fatalf("forward-only decode of %s differs from the seekable decode", name)
			}
		})
	}
}

// TestForwardOggOpusReadsMissingCommentHeader pins the non-conformant fallback:
// a stream whose second packet is audio rather than OpusTags must play.
func TestForwardOggOpusReadsMissingCommentHeader(t *testing.T) {
	audio := []byte("audio-without-tags")
	path := writeOgg(t, "notags.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(960, 0x04, 1, audio),
	)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	r, err := newForwardOggOpus(oneShotReader{f})
	if err != nil {
		t.Fatalf("newForwardOggOpus: %v", err)
	}
	if r.PreSkip() != 312 {
		t.Fatalf("PreSkip = %d, want 312", r.PreSkip())
	}
	if r.TotalGranule() != -1 {
		t.Fatalf("TotalGranule = %d, want -1", r.TotalGranule())
	}
	if err := r.SeekGranule(0, 0); !errors.Is(err, ErrNotSeekable) {
		t.Fatalf("SeekGranule on a forward-only reader = %v, want ErrNotSeekable", err)
	}

	pkt, granule, err := r.ReadPacket()
	if err != nil || !bytes.Equal(pkt, audio) || granule != 960 {
		t.Fatalf("first packet = %q granule=%d err=%v, want the audio packet", pkt, granule, err)
	}
	if _, _, err := r.ReadPacket(); !errors.Is(err, io.EOF) {
		t.Fatalf("end err = %v, want io.EOF", err)
	}
}

// TestForwardOggOpusReportsChecksumMismatch checks that the forward reader
// verifies page checksums, matching the seekable reader's integrity contract.
func TestForwardOggOpusReportsChecksumMismatch(t *testing.T) {
	page := oggPage(960, 0x00, 2, bytes.Repeat([]byte{7}, 300))
	page[len(page)-1] ^= 0xff
	path := writeOgg(t, "corrupt.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		page,
	)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	r, err := newForwardOggOpus(oneShotReader{f})
	if err != nil {
		t.Fatalf("newForwardOggOpus: %v", err)
	}
	if _, _, err := r.ReadPacket(); !errors.Is(err, errOggOpusChecksum) {
		t.Fatalf("err = %v, want errOggOpusChecksum", err)
	}
}

// TestForwardOggOpusReassemblesContinuedPacket covers the case the fixtures
// never exercise: a packet split across a page boundary. The reference file has
// 278 such pages, so the forward reader must join them exactly like the
// seekable reader rather than treat each page as a packet boundary.
func TestForwardOggOpusReassemblesContinuedPacket(t *testing.T) {
	head := bytes.Repeat([]byte{0x11}, 510)
	tail := bytes.Repeat([]byte{0x22}, 90)
	pageA := rawOggPage(-1, 0x00, 2, []byte{255, 255}, head)
	pageB := rawOggPage(960, 0x01, 3, []byte{90, 5}, append(append([]byte(nil), tail...), []byte("extra")...))
	path := writeOgg(t, "continued.opus",
		oggPage(0, 0x02, 0, opusHeadPacket(312, 2)),
		oggPage(0, 0x00, 1, []byte("OpusTags")),
		pageA, pageB,
	)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	r, err := newForwardOggOpus(oneShotReader{f})
	if err != nil {
		t.Fatalf("newForwardOggOpus: %v", err)
	}

	pkt, granule, err := r.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if want := append(append([]byte(nil), head...), tail...); !bytes.Equal(pkt, want) {
		t.Fatalf("continued packet = %d bytes, want %d", len(pkt), len(want))
	}
	if granule != 960 {
		t.Fatalf("continued packet granule = %d, want 960", granule)
	}

	pkt, granule, err = r.ReadPacket()
	if err != nil || string(pkt) != "extra" || granule != 960 {
		t.Fatalf("second packet = %q granule=%d err=%v", pkt, granule, err)
	}
	if _, _, err := r.ReadPacket(); !errors.Is(err, io.EOF) {
		t.Fatalf("end err = %v, want io.EOF", err)
	}
}

// TestPionSeekMatchesStraightDecodeBytes is the load-bearing seek proof: after a
// seek the decoder must emit exactly the PCM a straight decode produced from
// that frame, byte for byte. A granule seek starts mid-stream, so this would
// catch an off-by-pre-skip conversion, a missed pre-roll discard, or a stale
// position that only showed up as a small phase shift. Only the exact variant
// makes this guarantee; the fast default is bounded but not bit-exact, which
// TestPionFastSeekTransientIsBoundedAndConfined pins instead.
func TestPionSeekMatchesStraightDecodeBytes(t *testing.T) {
	factory := NewPionOpusExactFactory()
	full := decodeFixture(t, "stereo_2s.opus")

	targets := []int64{0, 1, 1000, 12345, 30000, 48000, 90000, 96000}
	for _, at := range targets {
		d, err := factory.Open(fixturePath(t, "stereo_2s.opus"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		seeker, ok := d.(Seeker)
		if !ok {
			t.Fatal("pion decoder does not implement Seeker")
		}
		if err := seeker.SeekFrame(at); err != nil {
			d.Close()
			t.Fatalf("SeekFrame(%d): %v", at, err)
		}

		// A window that stays inside the straight decode, long enough to span
		// the pre-roll and several audio pages.
		frames := min(int64(48000), int64(len(full)/2)-at)
		got := readExactlyFrames(t, d, int(frames))
		d.Close()

		want := full[at*2 : (at+frames)*2]
		if !bytes.Equal(float32sToBytes(got), float32sToBytes(want)) {
			t.Fatalf("seek to %d differs from the straight decode window", at)
		}
	}
}

// TestPionSeekMatchesStraightDecodeOnReference repeats the byte-identity check
// on the 139 s file, where audio pages are large and each seek crosses several
// of them, so a seek that only worked within one page would fail here. It uses
// the exact variant, the only one that promises byte identity.
func TestPionSeekMatchesStraightDecodeOnReference(t *testing.T) {
	path := realOpusPath(t)
	full := decodeFixturePath(t, path)

	targets := []int64{5000, 48000, 240000, 1440000, 4800000}
	for _, at := range targets {
		d, err := NewPionOpusExactFactory().Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := d.(Seeker).SeekFrame(at); err != nil {
			d.Close()
			t.Fatalf("SeekFrame(%d): %v", at, err)
		}
		got := readExactlyFrames(t, d, 96000)
		d.Close()

		want := full[at*2 : (at+96000)*2]
		if !bytes.Equal(float32sToBytes(got), float32sToBytes(want)) {
			t.Fatalf("reference seek to %d differs from the straight decode window", at)
		}
	}
}

// readExactlyFrames reads exactly frames interleaved frames or fails.
func readExactlyFrames(t *testing.T, d Decoder, frames int) []float32 {
	t.Helper()

	out := make([]float32, 0, frames*2)
	buf := make([]float32, 4096)
	for len(out) < frames*2 {
		want := min(len(buf), frames*2-len(out))
		n, err := d.ReadFrames(buf[:want])
		if n > 0 {
			out = append(out, buf[:n*2]...)
		}
		if err != nil {
			t.Fatalf("ReadFrames after seek: %v", err)
		}
		if n == 0 {
			t.Fatal("ReadFrames made no progress")
		}
	}

	return out
}

// decodeFixturePath decodes a fixture given a full path rather than a testdata
// name, for files outside the fixtures directory.
func decodeFixturePath(t *testing.T, path string) []float32 {
	t.Helper()

	d, err := NewPionOpusFactory().Open(path)
	if err != nil {
		t.Fatalf("Open %s: %v", path, err)
	}
	defer d.Close()

	return decodeAll(t, d)
}

// TestPionSeekCost measures the native seek at several targets. The old path
// reopened and decoded from frame zero (about 6.6 ms per audio-second on the
// reference file); the granule index makes the cost independent of the target.
func TestPionSeekCost(t *testing.T) {
	cases := []struct {
		name string
		path string
		// targets are absolute 48 kHz frames.
		targets []int64
	}{
		{"reference_139s", realOpusPath(t), []int64{10 * 48000, 30 * 48000, 60 * 48000, 120 * 48000}},
		{"stereo_2s", fixturePath(t, "stereo_2s.opus"), []int64{0, 12000, 24000, 48000}},
		{"mono_1s", fixturePath(t, "mono_1s.opus"), []int64{0, 12000, 24000}},
		{"short_stereo", fixturePath(t, "short_stereo.opus"), []int64{0, 6000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewPionOpusFactory().Open(tc.path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()
			seeker := d.(Seeker)

			// The whole point of the rewire: cost must be flat, not linear in
			// the target. Decode-and-discard took 76 ms to 832 ms at these
			// targets and the farthest was over ten times the nearest; the
			// index seek plus its short decode remainder is a near-constant
			// ~13 ms regardless of distance. The comparison is a ratio so it
			// survives a slow or -race run where every seek costs more.
			first := time.Duration(0)
			var last, slowest time.Duration
			for i, frame := range tc.targets {
				start := time.Now()
				if err := seeker.SeekFrame(frame); err != nil {
					t.Fatalf("SeekFrame(%d): %v", frame, err)
				}
				elapsed := time.Since(start)
				if i == 0 {
					first = elapsed
				}
				last = elapsed
				slowest = max(slowest, elapsed)
				t.Logf("seek to %d frames (%.1fs): %v", frame, float64(frame)/48000, elapsed)
			}
			t.Logf("%s: %d seeks, first %v, last %v, slowest %v", tc.name, len(tc.targets), first, last, slowest)

			// The farthest target must not cost several times the nearest, as
			// decode-and-discard would; a flat path stays within a small
			// constant of it whatever the absolute speed of the machine.
			if tc.name == "reference_139s" && last > 4*first+50*time.Millisecond {
				t.Fatalf("far seek costs %v against near %v; the index seek should be flat", last, first)
			}
		})
	}
}

// TestPionOpenStaysCheap guards the open path: the index is one page-header
// pass, so opening the 139 s file must stay well below one frame's worth of
// audio (20 ms) and nowhere near the old linear seek cost.
func TestPionOpenStaysCheap(t *testing.T) {
	path := realOpusPath(t)

	start := time.Now()
	d, err := NewPionOpusFactory().Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	elapsed := time.Since(start)
	t.Logf("Open(%s): %v, TotalFrames=%d", path, elapsed, d.Info().TotalFrames)

	if elapsed > 20*time.Millisecond {
		t.Fatalf("Open took %v, want well under one 20 ms frame of audio", elapsed)
	}
}

// TestSeekGranuleForAddsPreSkip pins the frame-to-granule conversion. Granules
// count decoded samples including the pre-skip, so a seek target must be
// frame+preSkip. Dropping the pre-skip still yields correct PCM because
// discardTo closes the gap, but it asks the reader for the wrong granule and
// can decode an extra page, so this pins the conversion directly.
func TestSeekGranuleForAddsPreSkip(t *testing.T) {
	cases := []struct {
		frame, preSkip, want int64
	}{
		{0, 312, 312},
		{30000, 312, 30312},
		{48000, 0, 48000},
		{48000, 312, 48312},
		{1000000, 3840, 1003840},
	}
	for _, tt := range cases {
		if got := seekGranuleFor(tt.frame, tt.preSkip); got != tt.want {
			t.Fatalf("seekGranuleFor(%d, %d) = %d, want %d (granules include pre-skip)",
				tt.frame, tt.preSkip, got, tt.want)
		}
	}
}

func TestPionSeekMatchesPrefixOfFullDecode(t *testing.T) {
	factory := NewPionOpusExactFactory()
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
