//go:build !windows

package priority

// raise is a no-op outside Windows. Linux already schedules the PulseAudio
// server for real-time, and macOS runs CoreAudio's own render thread, so the
// process priority this package adjusts on Windows has no equivalent gap to
// close there.
func raise() (Report, error) {
	return Report{
		Platform: platform(),
		Priority: "unchanged, the OS schedules the audio thread",
	}, nil
}
