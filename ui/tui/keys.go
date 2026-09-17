package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/player"
)

// seekStep and volumeStep are the granularity of the two continuous controls.
// They are constants rather than settings because a keyboard is a coarse input;
// a user who needs precision is not the audience of a prototype TUI.
const (
	seekStep   = 5 * time.Second
	volumeStep = 0.1
)

// handleKey applies one key press to the player and reports whether the UI
// should quit. It is deliberately free of terminal and ticker concerns so every
// binding, including illegal input, is unit-testable against a fake player.
//
// An unrecognised key is ignored rather than treated as an error: keystrokes
// arrive from a human, and the UI must not fall over on the first stray press.
func handleKey(p player.Player, msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "space":
		togglePause(p)

		return false

	case "q", "ctrl+c":
		return true

	case "n":
		if p != nil {
			_ = p.Next()
		}

		return false

	case "p":
		if p != nil {
			_ = p.Prev()
		}

		return false

	case "h", "left":
		seekBy(p, -seekStep)

		return false

	case "l", "right":
		seekBy(p, seekStep)

		return false

	case "+", "=":
		volumeBy(p, volumeStep)

		return false

	case "-", "_":
		volumeBy(p, -volumeStep)

		return false
	}

	return false
}

// togglePause is the space bar. It only ever acts on a state it can act on, so
// pressing space on an idle player is a no-op rather than an error.
func togglePause(p player.Player) {
	if p == nil {
		return
	}

	switch p.Snapshot().State {
	case player.Playing:
		_ = p.Pause()
	case player.Paused:
		_ = p.Resume()
	}
}

// seekBy moves relative to the current position. A positive target past a known
// duration is clamped to the end, because a decoder can treat a past-the-end
// seek as a failure; a negative target is clamped to zero.
func seekBy(p player.Player, delta time.Duration) {
	if p == nil {
		return
	}

	snap := p.Snapshot()
	target := snap.Position + delta
	if target < 0 {
		target = 0
	}
	if delta > 0 && snap.Duration > 0 && target > snap.Duration {
		target = snap.Duration
	}

	_ = p.Seek(target)
}

func volumeBy(p player.Player, delta float64) {
	if p == nil {
		return
	}

	p.SetVolume(clampVolume(p.Snapshot().Volume + delta))
}

func clampVolume(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}

	return v
}
