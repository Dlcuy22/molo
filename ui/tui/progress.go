package tui

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/dlcuy22/molo"
)

// unknownClock is what an unresolved duration shows. Rendering "0:00" would
// claim a zero-length track, which is the bug this display exists to avoid.
const unknownClock = "--:--"

// formatClock renders a duration as m:ss, or h:mm:ss once it crosses an hour.
// Non-positive input is "0:00" so callers never print a negative time.
func formatClock(d time.Duration) string {
	if d <= 0 {
		return "0:00"
	}

	total := int64(d / time.Second)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60

	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}

	return fmt.Sprintf("%d:%02d", m, s)
}

// formatDuration renders a snapshot duration, using the indeterminate marker
// for the zero value that means "the probe has not answered yet".
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return unknownClock
	}

	return formatClock(d)
}

// formatProgress renders the state, the two clocks and, only when the duration
// is known, a percentage. The percentage is omitted rather than zeroed for an
// unknown duration: a "0%" is indistinguishable from a real one.
func formatProgress(state molo.State, pos, dur time.Duration) string {
	if state == molo.Idle {
		return "idle"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-8s %s / %s", stateName(state), formatClock(pos), formatDuration(dur))

	if pct, ok := progressPercent(pos, dur); ok {
		fmt.Fprintf(&b, " %3d%%", pct)
	}

	return b.String()
}

// progressPercent clamps to [0, 100] and reports false for an unknown duration,
// so a caller can never divide by zero or print "105%".
func progressPercent(pos, dur time.Duration) (int, bool) {
	if dur <= 0 {
		return 0, false
	}

	pct := int(math.Round(float64(pos) / float64(dur) * 100))
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	return pct, true
}

// renderBar draws the progress bar. A known duration fills a fraction; an
// unknown one animates a marker whose position advances with frame, which is
// the honest way to show "still probing" without a number.
func renderBar(pos, dur time.Duration, width, frame int) string {
	if width <= 0 {
		return "[]"
	}

	chars := make([]rune, width)
	if dur <= 0 {
		for i := range chars {
			chars[i] = '-'
		}
		chars[((frame%width)+width)%width] = '='

		return "[" + string(chars) + "]"
	}

	filled := width
	if pct, ok := progressPercent(pos, dur); ok {
		filled = int(math.Round(float64(pct) / 100 * float64(width)))
	}
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}

	for i := range chars {
		if i < filled {
			chars[i] = '#'
		} else {
			chars[i] = '-'
		}
	}

	return "[" + string(chars) + "]"
}

func volumePercent(v float64) int {
	pct := int(math.Round(v * 100))
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	return pct
}

// displayName prefers a resolved tag and falls back to the file name, never the
// full path, so a long queue does not become a filesystem dump.
func displayName(title, path string) string {
	if title != "" {
		return title
	}
	if path == "" {
		return ""
	}

	return filepath.Base(path)
}

func stateName(s molo.State) string {
	switch s {
	case molo.Playing:
		return "playing"
	case molo.Paused:
		return "paused"
	case molo.Stopped:
		return "stopped"
	default:
		return "idle"
	}
}

// clip truncates a plain string to w display cells, with an ellipsis so a
// truncated name is visibly truncated rather than silently wrong.
func clip(s string, w int) string {
	if w <= 0 {
		return s
	}

	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return string(r[:1])
	}

	return string(r[:w-1]) + "…"
}
