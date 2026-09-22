package dsp

import "math"

// goertzel measures the magnitude of one frequency in one interleaved channel.
// It is a single-bin DFT, which is all a tone fixture needs and far cheaper
// than a full transform. The result is normalised so a pure sine of amplitude
// A at freq returns A.
func goertzel(samples []float32, ch, channel, rate int, freq float64) float64 {
	if ch < 1 || channel < 0 || channel >= ch || rate <= 0 {
		return 0
	}
	n := len(samples) / ch
	if n == 0 {
		return 0
	}

	w := 2 * math.Pi * freq / float64(rate)
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for i := 0; i < n; i++ {
		s0 := float64(samples[i*ch+channel]) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	if power < 0 {
		power = 0
	}

	return 2 * math.Sqrt(power) / float64(n)
}

// interleaveDeep copies samples so a test can keep an untouched reference
// before an effect processes the buffer in place.
func interleaveDeep(samples []float32) []float32 {
	return append([]float32(nil), samples...)
}
