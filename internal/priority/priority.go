// Package priority raises the process's scheduling priority for audio.
//
// On Windows, WASAPI shared mode has no out-of-process mixer to absorb a late
// wakeup: the render thread must refill the endpoint buffer before its next
// period, so under heavy CPU load a normal-priority thread is preempted and the
// buffer drains. Linux does not need this because PulseAudio runs the actual
// mixing in its own real-time-priority server process, so a late feeder only
// deepens a buffer. This package is the Windows correction: it raises the
// process to above-normal and asks for a 1 ms timer, so the render thread wakes
// on time. Elsewhere it is a no-op.
package priority

import "runtime"

// Report is the scheduling state a Raise leaves behind, for a startup debug
// line. Platform is always filled; Priority describes what is in effect,
// whether or not this package changed anything.
type Report struct {
	// Platform is the detected OS and architecture, GOOS/GOARCH.
	Platform string

	// Priority is a short human description of the scheduling state in force.
	Priority string
}

// Lines returns the report as the one-per-line strings a front end logs.
func (r Report) Lines() []string {
	return []string{
		"Detected running on " + r.Platform,
		"Priority: " + r.Priority,
	}
}

// platform is the detected OS and architecture, shared by every build so the
// report reads the same wherever it runs.
func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Raise lifts the process's scheduling priority for playback and reports what
// it did. It is best-effort: a host that refuses the change still plays, just
// without the extra margin, so a caller logs the returned Report and the error
// rather than treating it as fatal. The change is process-wide, which is what
// reaches the backend's own render thread, and it is left in place for the life
// of the process.
func Raise() (Report, error) { return raise() }
