package decode

import (
	"encoding/json"
	"math"
	"os/exec"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
)

// maxDurationSkew is the tolerance between a probe-derived duration and
// ffprobe's. Both come from the same granule position, so the only expected
// gap is each tool's rounding.
const maxDurationSkew = 50 * time.Millisecond

func TestProbeDurationMatchesFFprobe(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}

	fixtures := []string{"stereo_2s.opus", "mono_1s.opus", "short_stereo.opus"}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := fixturePath(t, name)
			want := ffprobeDuration(t, ffprobe, path)

			for _, prober := range []struct {
				name   string
				prober Prober
			}{
				{"pion", NewPionOpusFactory()},
				{"libopusfile", NewLibopusfileFactory()},
			} {
				if prober.name == "libopusfile" && loadLibopusfile() != nil {
					continue
				}
				info, err := prober.prober.Probe(path, ProbeOptions{Duration: core.DurationProbe})
				if err != nil {
					t.Fatalf("%s Probe: %v", prober.name, err)
				}
				if info.TotalFrames < 0 {
					t.Fatalf("%s probe left TotalFrames unknown", prober.name)
				}

				got := info.Duration()
				if skew := absDuration(got - want); skew > maxDurationSkew {
					t.Fatalf("%s duration %v differs from ffprobe %v by %v (max %v)",
						prober.name, got, want, skew, maxDurationSkew)
				}
			}
		})
	}
}

func ffprobeDuration(t *testing.T, ffprobe, path string) time.Duration {
	t.Helper()

	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}

	var parsed struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse ffprobe output: %v", err)
	}

	seconds, err := time.ParseDuration(parsed.Format.Duration + "s")
	if err != nil {
		t.Fatalf("parse ffprobe duration %q: %v", parsed.Format.Duration, err)
	}

	return seconds
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}

	return d
}

// TestProbeDurationIsNotZero guards against a probe that reports success with
// an obviously wrong zero length.
func TestProbeDurationIsNotZero(t *testing.T) {
	info, err := NewPionOpusFactory().Probe(fixturePath(t, "stereo_2s.opus"), ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := info.Duration(); math.Abs(float64(got-time.Second*2)) > float64(maxDurationSkew) {
		t.Fatalf("duration = %v, want about 2s", got)
	}
}
