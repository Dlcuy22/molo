//go:build windows

package priority

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// timerPeriodMs is the timer resolution to request. WASAPI event-driven
// rendering wakes on the system timer, so 1 ms is what keeps the render thread
// from being quantized to the default ~15.6 ms tick under load. On Windows 10
// 2004 and later this is a per-process request, so it does not drag the whole
// system's timer down the way the old global call did.
const timerPeriodMs = 1

// raise sets the process to above-normal priority and raises the timer
// resolution. Above-normal, not high: the render thread needs to win against
// ordinary background load, not against the UI and the rest of the system,
// which high or realtime priority would start to starve.
//
// timeBeginPeriod is deliberately not paired with a timeEndPeriod: the process
// lives for the whole playback session, so the resolution is held for that
// lifetime and released by the OS on exit.
func raise() (Report, error) {
	rep := Report{Platform: platform()}

	// A class change can be partially applied: SetPriorityClass can fail while
	// the timer call still matters, so the class error is recorded but the timer
	// is still attempted. The report describes what is actually in effect, read
	// back rather than assumed, so a refused change is visible in the log.
	classErr := windows.SetPriorityClass(windows.CurrentProcess(), windows.ABOVE_NORMAL_PRIORITY_CLASS)
	rep.Priority = priorityClassName() + fmt.Sprintf(" (timer %d ms)", timerPeriodMs)

	if err := windows.TimeBeginPeriod(timerPeriodMs); err != nil {
		return rep, fmt.Errorf("priority: timer period: %w", err)
	}
	if classErr != nil {
		return rep, fmt.Errorf("priority: set class: %w", classErr)
	}

	return rep, nil
}

// priorityClassName reads back the process priority class so the report states
// what is in force, not what was requested. An unreadable class is named
// rather than guessed at.
func priorityClassName() string {
	class, err := windows.GetPriorityClass(windows.CurrentProcess())
	if err != nil {
		return "unknown"
	}

	switch class {
	case windows.IDLE_PRIORITY_CLASS:
		return "idle"
	case windows.BELOW_NORMAL_PRIORITY_CLASS:
		return "below normal"
	case windows.NORMAL_PRIORITY_CLASS:
		return "normal"
	case windows.ABOVE_NORMAL_PRIORITY_CLASS:
		return "above normal"
	case windows.HIGH_PRIORITY_CLASS:
		return "high"
	case windows.REALTIME_PRIORITY_CLASS:
		return "realtime"
	default:
		return fmt.Sprintf("class 0x%x", class)
	}
}
