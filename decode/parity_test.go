package decode

import (
	"math"
	"testing"
)

const (
	// seekConvergeFrames is how much audio after a native seek is excluded from
	// comparison. libopusfile decodes an 80 ms pre-roll to rebuild decoder state
	// and documents that this may not match a straight-through decode, so 200 ms
	// gives it ample margin while still catching a wrong seek target.
	seekConvergeFrames = 9600
	// parityFloorDBFS is the required agreement between two independent Opus
	// implementations. Both decode the same lossy bitstream, so the only
	// remaining differences are float rounding and resampler tails.
	parityFloorDBFS = -60.0
)

func TestOpusParityFullDecode(t *testing.T) {
	requireLibopusfile(t)

	tests := []struct {
		fixture string
		frames  int64
	}{
		{"stereo_2s.opus", 96000},
		{"mono_1s.opus", 48000},
		{"short_stereo.opus", 12000},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			reference := decodeFixture(t, tt.fixture)
			native := decodeFixtureLibopusfile(t, tt.fixture)

			got := int64(len(native) / 2)
			if got < tt.frames || got > tt.frames {
				t.Fatalf("libopusfile decoded %d frames, want exactly %d", got, tt.frames)
			}
			if frames := int64(len(reference) / 2); frames < tt.frames || frames > tt.frames+5760 {
				t.Fatalf("pion decoded %d frames, want [%d, %d]", frames, tt.frames, tt.frames+5760)
			}

			common := min(len(reference), len(native))
			db := rmsDiffDB(native[:common], reference[:common])
			t.Logf("%s: %d common frames, difference %.2f dBFS", tt.fixture, common/2, db)
			if db > parityFloorDBFS {
				t.Fatalf("decoders differ by %.2f dBFS, want below %.0f", db, parityFloorDBFS)
			}
		})
	}
}

func TestOpusParityMonoDownmix(t *testing.T) {
	requireLibopusfile(t)

	// A mono stream is forced to two channels on both paths. If either wrapper
	// mishandled the channel mapping this would show up as a large difference.
	reference := decodeFixture(t, "mono_1s.opus")
	native := decodeFixtureLibopusfile(t, "mono_1s.opus")

	common := min(len(reference), len(native))
	if common < 48000*2 {
		t.Fatalf("only %d common frames, want at least 48000", common/2)
	}
	if db := rmsDiffDB(native[:common], reference[:common]); db > parityFloorDBFS {
		t.Fatalf("mono decoders differ by %.2f dBFS, want below %.0f", db, parityFloorDBFS)
	}
}

func TestOpusParityAfterSeek(t *testing.T) {
	requireLibopusfile(t)

	const (
		at     = 30000
		frames = 48000
	)

	native, err := NewLibopusfileFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("open libopusfile: %v", err)
	}
	defer native.Close()
	if err := native.(Seeker).SeekFrame(at); err != nil {
		t.Fatalf("native SeekFrame: %v", err)
	}

	nativeBuf := make([]float32, frames*2)
	if n, err := readExactly(t, native, nativeBuf); err != nil || n != frames {
		t.Fatalf("native read after seek: frames=%d err=%v", n, err)
	}

	pion, err := NewPionOpusExactFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("open pion: %v", err)
	}
	defer pion.Close()
	if err := pion.(Seeker).SeekFrame(at); err != nil {
		t.Fatalf("pion SeekFrame: %v", err)
	}

	pionBuf := make([]float32, frames*2)
	if n, err := readExactly(t, pion, pionBuf); err != nil || n != frames {
		t.Fatalf("pion read after seek: frames=%d err=%v", n, err)
	}

	db := segmentDiffDB(nativeBuf, pionBuf, seekConvergeFrames)
	t.Logf("post-seek: %d frames compared after %d frames of convergence, difference %.2f dBFS",
		frames-seekConvergeFrames, seekConvergeFrames, db)
	if db > parityFloorDBFS {
		t.Fatalf("post-seek decoders differ by %.2f dBFS after convergence, want below %.0f", db, parityFloorDBFS)
	}
}

// rmsDiffDB compares two equal-shape signals and returns the RMS of their
// difference relative to the reference RMS, in dBFS.
func rmsDiffDB(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.Inf(1)
	}
	var diff, ref float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		diff += d * d
		ref += float64(b[i]) * float64(b[i])
	}
	diff /= float64(len(a))
	ref /= float64(len(b))
	if diff == 0 {
		return math.Inf(-1)
	}
	if ref == 0 {
		return math.Inf(1)
	}

	return 10 * math.Log10(diff/ref)
}

// segmentDiffDB measures agreement only past the first skipFrames, which is
// where decoder-state pre-roll after a seek has converged.
func segmentDiffDB(a, b []float32, skipFrames int) float64 {
	skip := skipFrames * 2
	if skip >= len(a) || skip >= len(b) {
		return math.Inf(1)
	}

	return rmsDiffDB(a[skip:], b[skip:])
}

func decodeFixtureLibopusfile(t *testing.T, name string) []float32 {
	t.Helper()

	d, err := NewLibopusfileFactory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer d.Close()

	return decodeAll(t, d)
}
