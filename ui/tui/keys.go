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

// keyResult is what one key press asks the model to do. A seek is expressed as
// a relative delta rather than an engine call: on the pure-Go Opus decoder a
// seek costs time proportional to its target position, so a burst of keys must
// become one engine call, which only the model can batch.
type keyResult struct {
	quit    bool
	seeking bool
	delta   time.Duration
}

// handleKey maps one key press to a keyResult. It is deliberately free of
// terminal and ticker concerns so every binding, including illegal input, is
// unit-testable against a fake player.
//
// An unrecognised key is ignored rather than treated as an error: keystrokes
// arrive from a human, and the UI must not fall over on the first stray press.
func handleKey(p player.Player, msg tea.KeyPressMsg) keyResult {
	switch msg.String() {
	case "space":
		togglePause(p)

		return keyResult{}

	case "q", "ctrl+c":
		return keyResult{quit: true}

	case "n":
		if p != nil {
			_ = p.Next()
		}

		return keyResult{}

	case "p":
		if p != nil {
			_ = p.Prev()
		}

		return keyResult{}

	case "h", "left":
		return keyResult{seeking: true, delta: -seekStep}

	case "l", "right":
		return keyResult{seeking: true, delta: seekStep}

	case "+", "=":
		volumeBy(p, volumeStep)

		return keyResult{}

	case "-", "_":
		volumeBy(p, -volumeStep)

		return keyResult{}
	}

	return keyResult{}
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

// clampSeekTarget moves from base by delta. A positive target past a known
// duration is clamped to the end, because a decoder can treat a past-the-end
// seek as a failure; a negative target is clamped to zero.
func clampSeekTarget(base, delta, duration time.Duration) time.Duration {
	target := base + delta
	if target < 0 {
		target = 0
	}
	if delta > 0 && duration > 0 && target > duration {
		target = duration
	}

	return target
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
