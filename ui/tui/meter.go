package tui

import (
	"math"
	"strings"
)

// meterDecay is how much of the previous level survives one frame of silence.
// A meter that snapped to zero on every quiet buffer would flicker on the
// attack of the next note, so the display smooths the fall only.
const meterDecay = 0.75

// meter turns sample levels into a display level with a slow release.
type meter struct {
	level float64
}

// push folds one measured level in. A level louder than the held one replaces
// it immediately, so an attack is never missed.
func (m *meter) push(level float64) {
	if level > m.level {
		m.level = level
	} else {
		m.level *= meterDecay
	}
	if m.level > 1 {
		m.level = 1
	}
	if m.level < 0 {
		m.level = 0
	}
}

func (m *meter) reset() { m.level = 0 }

// rms is the root mean square of the buffer, in [0, 1] for full-scale float
// samples. An empty buffer is silence, not a division by zero.
func rms(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}

	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}

	return math.Sqrt(sum / float64(len(samples)))
}

// meterFull and meterEmpty are the two glyphs of the level meter: block
// characters, which a terminal renders as a continuous bar without a font that
// carries a linear scale.
const (
	meterFull  = "█"
	meterEmpty = "░"
)

// meterBars draws width cells filled to the level. The level is scaled with a
// square root so a quiet passage still moves the meter instead of hugging zero.
func meterBars(level float64, width int) string {
	if width <= 0 {
		return ""
	}
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}

	filled := int(math.Round(math.Sqrt(level) * float64(width)))
	if filled > width {
		filled = width
	}

	return strings.Repeat(meterFull, filled) + strings.Repeat(meterEmpty, width-filled)
}
