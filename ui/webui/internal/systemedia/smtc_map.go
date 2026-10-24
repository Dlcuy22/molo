package systemedia

import (
	"strings"
	"time"
)

// The Windows Media enums and shapes the SMTC backend speaks. They live
// untagged so the mapping can be tested on any host; the values are the ones in
// Windows.Media.winmd.

// MediaPlaybackStatus.
const (
	smtcStatusStopped int32 = 2
	smtcStatusPlaying int32 = 3
	smtcStatusPaused  int32 = 4
)

// MediaPlaybackType, the kind of media the display updater describes.
const smtcTypeMusic int32 = 1

// SystemMediaTransportControlsButton.
const (
	smtcButtonPlay        int32 = 0
	smtcButtonPause       int32 = 1
	smtcButtonStop        int32 = 2
	smtcButtonRecord      int32 = 3
	smtcButtonFastForward int32 = 4
	smtcButtonRewind      int32 = 5
	smtcButtonNext        int32 = 6
	smtcButtonPrevious    int32 = 7
	smtcButtonChannelUp   int32 = 8
	smtcButtonChannelDown int32 = 9
)

// smtcAction is a pressed shell button reduced to something the player can do.
type smtcAction int

const (
	smtcActionNone smtcAction = iota
	smtcActionPlay
	smtcActionPause
	smtcActionStop
	smtcActionNext
	smtcActionPrevious
)

// buttonAction maps a pressed button onto an action. Record, the channel keys
// and anything outside the enum have no answer here, so they are dropped rather
// than guessed at.
func buttonAction(button int32) smtcAction {
	switch button {
	case smtcButtonPlay:
		return smtcActionPlay
	case smtcButtonPause:
		return smtcActionPause
	case smtcButtonStop:
		return smtcActionStop
	case smtcButtonNext:
		return smtcActionNext
	case smtcButtonPrevious:
		return smtcActionPrevious
	default:
		return smtcActionNone
	}
}

// smtcPlaybackStatus is the shell's status for the engine's state. SMTC also
// has Closed and Changing, which describe a media source loading; molo's engine
// has no equivalent, and idle reads as Stopped.
func smtcPlaybackStatus(t TrackInfo) int32 {
	switch {
	case t.Playing:
		return smtcStatusPlaying
	case t.Paused:
		return smtcStatusPaused
	default:
		return smtcStatusStopped
	}
}

// ticks is a duration in the 100 ns units a WinRT TimeSpan carries.
func ticks(d time.Duration) int64 {
	return int64(d / (100 * time.Nanosecond))
}

// smtcTimeline is the progress bar's five values, all in 100 ns ticks.
type smtcTimeline struct {
	Start    int64
	End      int64
	MinSeek  int64
	MaxSeek  int64
	Position int64
}

// timelineFor maps a track onto the progress bar. The seek window is the whole
// track because SMTC has no separate seekable range, and the position is
// clamped so a stale tick cannot put the bar past the end.
func timelineFor(duration, position time.Duration) smtcTimeline {
	end := ticks(duration)
	if end < 0 {
		end = 0
	}
	pos := ticks(position)
	if pos < 0 {
		pos = 0
	}
	if end > 0 && pos > end {
		pos = end
	}

	return smtcTimeline{End: end, MaxSeek: end, Position: pos}
}

// smtcButtons is which shell buttons to offer.
type smtcButtons struct {
	Play     bool
	Pause    bool
	Stop     bool
	Next     bool
	Previous bool
}

// buttonsFor derives the offered buttons from the wired commands. A button with
// no command behind it stays disabled, so the shell shows no control that would
// do nothing; PlayPause stands in for either half when the caller wired only it.
func buttonsFor(cmd Commands) smtcButtons {
	return smtcButtons{
		Play:     cmd.Play != nil || cmd.PlayPause != nil,
		Pause:    cmd.Pause != nil || cmd.PlayPause != nil,
		Stop:     cmd.Stop != nil,
		Next:     cmd.Next != nil,
		Previous: cmd.Previous != nil,
	}
}

// artFileName is the cache file for one cover. The extension follows the bytes
// because the shell decodes the file, not the MIME the tag declared.
func artFileName(key, mime string) string {
	if strings.Contains(mime, "png") {
		return "smtc-" + key + ".png"
	}

	return "smtc-" + key + ".jpg"
}
