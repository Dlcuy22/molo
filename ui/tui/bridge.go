package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/player"
)

// The messages the model reacts to. Engine events are translated into plain
// values here so Update never has to touch the sealed event interface or the
// engine at all.
type (
	// tickMsg drives the position poll.
	tickMsg time.Time

	// stateMsg carries an accepted state transition.
	stateMsg struct{ to player.State }

	// trackMsg announces that a new track became current.
	trackMsg struct {
		index int
		path  string
	}

	// seekedMsg confirms a seek after the engine applied it.
	seekedMsg struct{ position time.Duration }

	// endedMsg marks a track reaching its natural end.
	endedMsg struct{}

	// failedMsg carries a decode or device error.
	failedMsg struct{ err error }

	// eventsClosedMsg means the engine's event channel is done. The bridge
	// stops re-issuing itself on it.
	eventsClosedMsg struct{}

	// queueMsg carries a refreshed queue.
	queueMsg struct{ paths []string }

	// meterMsg carries the level of one non-blocking read of the visualizer
	// tap.
	meterMsg struct{ level float64 }

	// waveMsg carries a finished waveform pass. The sequence guards against a
	// result arriving after the user has moved to another track.
	waveMsg struct {
		seq  uint64
		wave *Wave
		err  error
	}
)

// waitForEvent reads one engine event and returns it translated. It is a
// command rather than a query because the read blocks until an event arrives,
// and blocking Update on a quiet player would freeze the whole UI.
//
// Update re-issues it after every event, so the bridge keeps draining without a
// dedicated goroutine.
func waitForEvent(ch <-chan player.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}

		return translateEvent(ev)
	}
}

// translateEvent maps the sealed event set to UI messages. The default arm is
// unreachable for the current engine, because the interface cannot be
// implemented outside internal/session.
func translateEvent(ev player.Event) tea.Msg {
	switch ev := ev.(type) {
	case player.StateChanged:
		return stateMsg{to: ev.To}
	case player.TrackChanged:
		return trackMsg{index: ev.Index, path: ev.Path}
	case player.Seeked:
		return seekedMsg{position: ev.Position}
	case player.TrackEnded:
		return endedMsg{}
	case player.Failed:
		return failedMsg{err: ev.Err}
	default:
		return nil
	}
}
