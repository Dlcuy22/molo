package main

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/dlcuy22/player"
)

// unknownClock is what an unresolved duration shows. A UI that printed "0:00"
// would be claiming a zero-length track, which is the exact bug this display
// exists to avoid.
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

// formatProgress renders one line of the progress display. It is a pure
// function of a Snapshot so every state is testable without a player: the
// caller decides when to paint it and where.
//
// The percentage is printed only when the duration is known. An indeterminate
// track shows "--:--" and omits the percentage field entirely rather than
// inventing one, which is the difference between "still probing" and a wrong
// number.
func formatProgress(s player.Snapshot) string {
	if s.State == player.Idle {
		return "idle"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-8s %s / %s", stateName(s.State), formatClock(s.Position), formatDuration(s.Duration))

	// The percentage is omitted, not zeroed, when the duration is unknown: a
	// zero-looking figure is indistinguishable from a real 0%.
	if s.Duration > 0 {
		fmt.Fprintf(&b, " %3d%%", progressPercent(s.Position, s.Duration))
	}

	// The volume column is always present and always padded to three digits so
	// a repainting line does not jitter as the value changes width.
	fmt.Fprintf(&b, "  vol %3d%%", volumePercent(s.Volume))

	hasIndex := s.QueueLen > 0 && s.QueueIndex >= 0
	if hasIndex {
		fmt.Fprintf(&b, "  [%d/%d]", s.QueueIndex+1, s.QueueLen)
	}

	if name := displayName(s.Meta.Tags.Title, s.Path); name != "" {
		if hasIndex {
			b.WriteString(" ")
		} else {
			b.WriteString("  ")
		}
		b.WriteString(name)
	}

	return b.String()
}

// formatDuration renders a snapshot duration, using the indeterminate marker
// for the zero value that means "the probe has not answered yet".
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return unknownClock
	}

	return formatClock(d)
}

// progressPercent clamps to [0, 100] so a position that overran a stale
// duration cannot render as "105%" or go negative after a seek reset.
func progressPercent(pos, dur time.Duration) int {
	if dur <= 0 {
		return 0
	}

	pct := int(math.Round(float64(pos) / float64(dur) * 100))
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}

	return pct
}

func volumePercent(v float64) int {
	pct := int(math.Round(v * 100))
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}

	return pct
}

// displayName prefers a resolved tag and falls back to the file name, never
// the full path, so a long queue does not turn the progress line into a
// filesystem dump.
func displayName(title, path string) string {
	if title != "" {
		return title
	}
	if path == "" {
		return ""
	}

	return filepath.Base(path)
}

func stateName(s player.State) string {
	switch s {
	case player.Playing:
		return "playing"
	case player.Paused:
		return "paused"
	case player.Stopped:
		return "stopped"
	default:
		return "idle"
	}
}
