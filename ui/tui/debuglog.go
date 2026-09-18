package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/dlcuy22/player"
)

// maxDebugLines bounds the in-memory log. A session can run for hours, and the
// panel only ever shows the tail, so keeping more than a few screens would be
// memory spent on lines nobody can see.
const maxDebugLines = 200

// debugLog is a bounded ring of preformatted diagnostic lines. It is a value on
// the model and has no lock because Update owns it; pushing past the bound
// discards the oldest line, which is the one furthest from view.
type debugLog struct {
	lines []string
}

func (l *debugLog) push(line string) {
	if line == "" {
		return
	}

	l.lines = append(l.lines, line)
	if len(l.lines) > maxDebugLines {
		// Copy the tail to the front of the same array: the length stays at the
		// bound and no new allocation happens on the steady-state push.
		l.lines = append(l.lines[:0], l.lines[len(l.lines)-maxDebugLines:]...)
	}
}

// The formatters below are the one place a line's shape is decided, so the
// panel's text is unit-testable without a terminal and every caller agrees on
// the layout.

func formatDecoderLine(decoder, parser string) string {
	return fmt.Sprintf("decoder  %s  parser  %s", decoder, parser)
}

func formatMetaLine(source string) string {
	return "meta     " + source
}

func formatStateLine(state player.State) string {
	return "state    " + stateName(state)
}

func formatEOSLine(path string) string {
	return "eos      " + displayName("", path)
}

func formatSeekLine(from, to, elapsed time.Duration) string {
	return fmt.Sprintf("seek     %s -> %s  took %dms", formatClock(from), formatClock(to), elapsed.Milliseconds())
}

func formatErrorLine(err error) string {
	if err == nil {
		return "error    (nil)"
	}

	return "error    " + err.Error()
}

func formatTrackLine(path string) string {
	return "track    " + displayName("", path)
}

// formatCodecLine records an accepted decoder change. It names the resolved
// choice, with "auto" spelled out for the empty preference so the log never
// shows a puzzling blank.
func formatCodecLine(name string) string {
	if name == "" {
		return "codec    auto"
	}

	return "codec    " + name
}

// renderDebugPanel draws the header and the newest lines that fit. The panel is
// allowed only the height the main panel and the queue leave behind: when the
// terminal is short it shows fewer, older lines, and it disappears entirely
// before it can push the now-playing panel off screen.
func (m model) renderDebugPanel(width, budget int) string {
	total := len(m.debug.lines)
	if total == 0 {
		return ""
	}
	if budget > total {
		budget = total
	}
	if budget <= 0 {
		return ""
	}
	// The log is oldest-first; keep its tail so the most recent facts survive.
	lines := m.debug.lines[total-budget:]

	var b strings.Builder
	b.WriteString(dimStyle.Render("debug"))
	for _, line := range lines {
		b.WriteString("\n")
		b.WriteString(renderDebugLine(line, width))
	}

	return b.String()
}

// debugLinesBudget is the number of log lines that fit under a body of the given
// height. The body's line count includes the line the final newline does not
// terminate; the panel then adds a blank separator row and the header row, so
// those three are what the budget subtracts.
func (m model) debugLinesBudget(bodyLines int) int {
	height := m.height
	if height <= 0 {
		height = defaultHeight
	}

	return height - bodyLines - 3
}

// renderDebugLine styles one line. Errors are the one kind worth a colour of
// their own; every other line is dim, with its label accented so the eye can
// scan the left column.
func renderDebugLine(line string, width int) string {
	line = clip(line, width)
	if strings.HasPrefix(line, "error") {
		return errStyle.Render(line)
	}
	if i := strings.Index(line, "  "); i >= 0 {
		return accentStyle.Render(line[:i]) + dimStyle.Render(line[i:])
	}

	return dimStyle.Render(line)
}
