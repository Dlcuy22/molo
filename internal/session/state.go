// Package session is the player's controller: the only component that owns
// mutable engine state. It drives the decoder, streamer, gain and device,
// keeps the queue, and reports changes as events so a UI can be built on top
// without ever touching the audio path.
//
// Everything public on Session is safe to call from any goroutine and returns
// without waiting for audio work. A UI polls Snapshot for the cheap things
// (position, state, queue) and listens on Events for the rare ones.
package session

// State is the lifecycle of the controller. The legal transitions are few and
// deliberate; an illegal one is ignored rather than allowed to corrupt the
// device or streamer.
type State uint8

const (
	// StateIdle is the state before any track has been started.
	StateIdle State = iota
	// StatePlaying means a track is loaded and the device is consuming it.
	StatePlaying
	// StatePaused means the current track is loaded but the device is paused.
	StatePaused
	// StateStopped means playback ended, was stopped, or failed. The queue may
	// still hold tracks, so Play or Next can start again.
	StateStopped
)

// canTransition encodes the legal state machine. A transition to the same
// state is not an edge: the controller treats it as a no-op and emits no
// StateChanged, which is why self pairs return false here.
func canTransition(from, to State) bool {
	switch from {
	case StateIdle:
		// A failed first start is the only way Idle reaches Stopped.
		return to == StatePlaying || to == StateStopped
	case StatePlaying:
		return to == StatePaused || to == StateStopped
	case StatePaused:
		return to == StatePlaying || to == StateStopped
	case StateStopped:
		return to == StatePlaying
	default:
		return false
	}
}
