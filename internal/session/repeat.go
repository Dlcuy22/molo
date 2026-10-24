package session

// RepeatMode is what the engine does when a track reaches its natural end.
//
// It lives in [runtimeSettings] rather than in the view because the control loop
// reads it at end-of-track to pick the next track, while the view is only what
// Snapshot reports. Keeping it with the other runtime settings means a setter
// and the control loop share one guarded copy.
type RepeatMode uint8

const (
	// RepeatOff stops at the end of the queue, which is the behaviour before
	// repeat existed.
	RepeatOff RepeatMode = iota
	// RepeatAll wraps from the last track back to the first.
	RepeatAll
	// RepeatOne restarts the current track instead of advancing.
	RepeatOne
)

// ValidateRepeat maps any value outside the known set to RepeatOff, so a stale
// or hand-set mode can never wedge the queue.
func ValidateRepeat(m RepeatMode) RepeatMode {
	switch m {
	case RepeatOff, RepeatAll, RepeatOne:
		return m
	default:
		return RepeatOff
	}
}
