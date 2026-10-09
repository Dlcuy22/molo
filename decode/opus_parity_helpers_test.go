package decode

import "math"

// seekConvergeFrames is how much audio after a seek is excluded from a
// comparison against a straight decode. A granule seek starts without the
// codec's inter-frame state, so the first frames differ by a bounded transient
// that converges well within this window.
const seekConvergeFrames = 9600

// parityFloorDBFS is the required agreement when the same codec decodes the
// same Opus packets through two different containers (WebM vs Ogg). The packets
// are identical, so the only difference is container framing; anything above
// this floor means a container is handing the codec the wrong bytes.
const parityFloorDBFS = -60.0

// rmsDiffDB compares two equal-shape signals and returns the RMS of their
// difference relative to the reference RMS, in dBFS. Lower is closer; -inf is
// an exact match.
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

// segmentDiffDB measures agreement only past the first skipFrames, where the
// decoder-state transient after a seek has converged.
func segmentDiffDB(a, b []float32, skipFrames int) float64 {
	skip := skipFrames * 2
	if skip >= len(a) || skip >= len(b) {
		return math.Inf(1)
	}

	return rmsDiffDB(a[skip:], b[skip:])
}
