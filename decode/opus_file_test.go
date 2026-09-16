package decode

import (
	"slices"
	"testing"

	"github.com/dlcuy22/player/core"
)

// requireLibopusfile skips a test when the native library is unavailable, so a
// machine without libopusfile still runs a green suite.
func requireLibopusfile(t *testing.T) {
	t.Helper()

	if err := loadLibopusfile(); err != nil {
		t.Skipf("libopusfile unavailable: %v", err)
	}
}

func TestLibopusfileFactoryContract(t *testing.T) {
	f := NewLibopusfileFactory()
	if f.Name() == "" {
		t.Fatal("Name() is empty")
	}
	if !slices.Contains(f.Exts(), ".opus") {
		t.Fatalf("Exts() = %v, want .opus", f.Exts())
	}
	if !f.Match([]byte("OggS\x00\x02")) {
		t.Fatal("Match rejected an OggS header")
	}
	if f.Match([]byte("fLaC")) {
		t.Fatal("Match accepted a FLAC header")
	}
}

func TestLibopusfileMissingLibraryIsGraceful(t *testing.T) {
	// The suite must stay green on a machine without libopusfile, so every
	// entry point reports an error instead of panicking or failing to build.
	t.Setenv("PLAYER_LIBOPUSFILE", "libopusfile-does-not-exist.so.0")

	if err := loadLibopusfile(); err == nil {
		t.Fatal("loadLibopusfile succeeded for a nonexistent library")
	}

	f := NewLibopusfileFactory()
	if _, err := f.Open(fixturePath(t, "short_stereo.opus")); err == nil {
		t.Fatal("Open succeeded without libopusfile")
	}
	if _, err := f.Probe(fixturePath(t, "short_stereo.opus"), ProbeOptions{Duration: core.DurationProbe}); err == nil {
		t.Fatal("Probe succeeded without libopusfile")
	}
}

func TestLibopusfileOpenReportsRealTotal(t *testing.T) {
	requireLibopusfile(t)

	d, err := NewLibopusfileFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	info := d.Info()
	if want := int64(96000); info.TotalFrames != want {
		t.Fatalf("TotalFrames = %d, want %d", info.TotalFrames, want)
	}
	if want := (core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}); info.Format != want {
		t.Fatalf("Format = %+v, want %+v", info.Format, want)
	}
}

func TestLibopusfileDecodeExactFrameCounts(t *testing.T) {
	requireLibopusfile(t)

	tests := []struct {
		fixture string
		want    int64
	}{
		{"stereo_2s.opus", 96000},
		{"mono_1s.opus", 48000},
		{"short_stereo.opus", 12000},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			d, err := NewLibopusfileFactory().Open(fixturePath(t, tt.fixture))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()

			pcm := decodeAll(t, d)
			// libopusfile stops exactly at the granule, so there is no padding
			// slack here unlike the forward-only decoder.
			if got := int64(len(pcm) / 2); got != tt.want {
				t.Fatalf("decoded %d frames, want %d", got, tt.want)
			}
		})
	}
}

func TestLibopusfileSeekReadsTheRequestedWindow(t *testing.T) {
	requireLibopusfile(t)

	factory := NewLibopusfileFactory()
	full, err := factory.Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	reference := decodeAll(t, full)
	full.Close()

	d, err := factory.Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	seeker, ok := d.(Seeker)
	if !ok {
		t.Fatal("libopusfile decoder does not implement Seeker")
	}
	const at = 30000
	if err := seeker.SeekFrame(at); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}

	got := make([]float32, 48000*2)
	frames, err := readExactly(t, d, got)
	if err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if frames != 48000 {
		t.Fatalf("read %d frames after seek, want 48000", frames)
	}

	// Native seek resets the decoder state, so the first packets differ from a
	// straight-through decode while the pre-roll converges. Beyond that window
	// the output must line up with the reference.
	want := reference[at*2 : (at+48000)*2]
	if db := segmentDiffDB(got, want, seekConvergeFrames); db > -60 {
		t.Fatalf("post-convergence seek difference %.2f dBFS, want below -60", db)
	}
}

func TestLibopusfileSeekRejectsInvalidTargets(t *testing.T) {
	requireLibopusfile(t)

	d, err := NewLibopusfileFactory().Open(fixturePath(t, "short_stereo.opus"))
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

func TestLibopusfileSeekBackToStart(t *testing.T) {
	requireLibopusfile(t)

	d, err := NewLibopusfileFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	seeker := d.(Seeker)
	if err := seeker.SeekFrame(48000); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}
	if err := seeker.SeekFrame(0); err != nil {
		t.Fatalf("SeekFrame back to zero: %v", err)
	}

	pcm := make([]float32, 96000*2)
	frames, err := readExactly(t, d, pcm)
	if err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if frames != 96000 {
		t.Fatalf("read %d frames, want 96000", frames)
	}
	if pcm[0] == 0 && pcm[1] == 0 {
		t.Fatal("stream head is silent after seeking back to zero")
	}
}

func TestLibopusfileCloseIsIdempotentAndStopsReads(t *testing.T) {
	requireLibopusfile(t)

	d, err := NewLibopusfileFactory().Open(fixturePath(t, "stereo_2s.opus"))
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

func TestLibopusfileOpenRejectsNonOpus(t *testing.T) {
	requireLibopusfile(t)

	dir := t.TempDir()
	path := writeFile(t, dir, "noise.opus", []byte("definitely not an ogg opus stream"))
	if _, err := NewLibopusfileFactory().Open(path); err == nil {
		t.Fatal("Open accepted a non-Opus file")
	}
	if _, err := NewLibopusfileFactory().Open(dir + "/absent.opus"); err == nil {
		t.Fatal("Open accepted a missing file")
	}
}

func TestLibopusfileProbeTotalFrames(t *testing.T) {
	requireLibopusfile(t)

	tests := []struct {
		fixture string
		want    int64
	}{
		{"stereo_2s.opus", 96000},
		{"mono_1s.opus", 48000},
		{"short_stereo.opus", 12000},
	}
	f := NewLibopusfileFactory()
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			info, err := f.Probe(fixturePath(t, tt.fixture), ProbeOptions{Duration: core.DurationProbe})
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if info.TotalFrames != tt.want {
				t.Fatalf("TotalFrames = %d, want %d", info.TotalFrames, tt.want)
			}
		})
	}

	// The cheapest mode must not pretend to know a length.
	info, err := f.Probe(fixturePath(t, "stereo_2s.opus"), ProbeOptions{Duration: core.DurationUnknown})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != -1 {
		t.Fatalf("DurationUnknown TotalFrames = %d, want -1", info.TotalFrames)
	}
}

func TestLibopusfileProbeRejectsNonOpus(t *testing.T) {
	requireLibopusfile(t)

	dir := t.TempDir()
	path := writeFile(t, dir, "noise.opus", []byte("definitely not an ogg opus stream"))
	if _, err := NewLibopusfileFactory().Probe(path, ProbeOptions{Duration: core.DurationProbe}); err == nil {
		t.Fatal("Probe accepted a non-Opus file")
	}
}

// readExactly fills dst entirely, failing only when the stream ends early.
func readExactly(t *testing.T, d Decoder, dst []float32) (int, error) {
	t.Helper()

	frames := 0
	for frames*2 < len(dst) {
		n, err := d.ReadFrames(dst[frames*2:])
		if err != nil {
			return frames, err
		}
		if n == 0 {
			return frames, nil
		}
		frames += n
	}

	return frames, nil
}
