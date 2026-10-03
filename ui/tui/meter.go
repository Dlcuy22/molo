package tui

import (
	"math"
	"strings"
	"time"

	"github.com/dlcuy22/molo"
)

// meterHalfLife is the time for the held level to fall to half when the audio
// goes quiet. Expressing the release as a time constant rather than a per-frame
// factor is what lets the meter poll at its own rate: a fixed factor per push
// would make the release speed depend on how often the UI happens to read.
const meterHalfLife = 600 * time.Millisecond

// meter turns sample levels into a display level with a fast attack and a slow,
// time-based release.
type meter struct {
	level float64
	last  time.Time
}

// push folds one measured level in at time now. A level louder than the held
// one replaces it immediately, so an attack is never missed; a quieter one lets
// the held level decay toward it. now is the instant the level was measured, so
// the decay is correct even when pushes are unevenly spaced; a zero now means
// "no elapsed time", which keeps a caller that has no clock from dividing by it.
func (m *meter) push(level float64, now time.Time) {
	if !now.IsZero() && !m.last.IsZero() {
		if dt := now.Sub(m.last); dt > 0 {
			m.level *= math.Exp2(-dt.Seconds() / meterHalfLife.Seconds())
		}
	}
	if !now.IsZero() {
		m.last = now
	}

	if level > m.level {
		m.level = level
	}
	if m.level > 1 {
		m.level = 1
	}
	if m.level < 0 {
		m.level = 0
	}
}

func (m *meter) reset() {
	m.level = 0
	m.last = time.Time{}
}

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

// drainTap reads every block currently buffered and returns the RMS of all of
// it. It reads in bounded blocks so one call never allocates a ring-sized
// buffer, and it keeps reading until the feed is empty so no audio is skipped
// between meter samples: the tap drops frames when it is not read, so a reader
// that took one block would silently miss the rest. It stops at meterMaxBlocks
// so a stalled UI cannot spin; the tap's own overwrite policy bounds the rest.
func drainTap(t molo.Tap, block int) (float64, int) {
	if t == nil || block <= 0 {
		return 0, 0
	}

	buf := make([]float32, block)

	var sum float64
	var frames int
	for range meterMaxBlocks {
		n := t.Read(buf)
		if n <= 0 {
			break
		}
		for _, s := range buf[:n] {
			sum += float64(s) * float64(s)
		}
		frames += n
	}

	if frames == 0 {
		return 0, 0
	}

	return math.Sqrt(sum / float64(frames)), frames
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
