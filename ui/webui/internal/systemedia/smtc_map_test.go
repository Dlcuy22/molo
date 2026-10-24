package systemedia

import (
	"testing"
	"time"
)

// TestSMTCPlaybackStatusMapsTheThreeStates pins the MediaPlaybackStatus values:
// playing and paused pass through, and idle reads as Stopped, which is what the
// shell draws as "nothing playing".
func TestSMTCPlaybackStatusMapsTheThreeStates(t *testing.T) {
	cases := []struct {
		track TrackInfo
		want  int32
	}{
		{TrackInfo{Playing: true}, smtcStatusPlaying},
		{TrackInfo{Paused: true}, smtcStatusPaused},
		{TrackInfo{Stopped: true}, smtcStatusStopped},
		{TrackInfo{}, smtcStatusStopped},
	}
	for _, c := range cases {
		if got := smtcPlaybackStatus(c.track); got != c.want {
			t.Fatalf("smtcPlaybackStatus(%+v) = %d, want %d", c.track, got, c.want)
		}
	}
}

// TestTicksAreHundredNanosecondUnits pins the TimeSpan unit. 1 ms is 10 000
// ticks, so a sub-tick remainder is dropped rather than rounded up.
func TestTicksAreHundredNanosecondUnits(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int64
	}{
		{0, 0},
		{time.Millisecond, 10_000},
		{time.Second, 10_000_000},
		{90 * time.Second, 900_000_000},
		{150 * time.Nanosecond, 1},
		{99 * time.Nanosecond, 0},
	}
	for _, c := range cases {
		if got := ticks(c.in); got != c.want {
			t.Fatalf("ticks(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestTimelineForClampsThePosition pins the progress bar: the seek window is the
// whole track, a negative or overrun position is clamped, and an unknown
// duration leaves the bar absent instead of drawing a bogus range.
func TestTimelineForClampsThePosition(t *testing.T) {
	got := timelineFor(90*time.Second, 3*time.Second)
	want := smtcTimeline{End: 900_000_000, MaxSeek: 900_000_000, Position: 30_000_000}
	if got != want {
		t.Fatalf("timelineFor(90s, 3s) = %+v, want %+v", got, want)
	}

	if got := timelineFor(90*time.Second, 120*time.Second); got.Position != 900_000_000 {
		t.Fatalf("position past the end = %d, want the end", got.Position)
	}
	if got := timelineFor(90*time.Second, -5*time.Second); got.Position != 0 {
		t.Fatalf("negative position = %d, want 0", got.Position)
	}

	// An unknown duration means the shell has nothing to draw a bar against.
	unknown := timelineFor(0, 5*time.Second)
	if unknown.End != 0 || unknown.MaxSeek != 0 {
		t.Fatalf("timelineFor(0, 5s) = %+v, want no range", unknown)
	}
	if unknown.Position != 50_000_000 {
		t.Fatalf("position without a duration = %d, want it passed through", unknown.Position)
	}
}

// TestButtonActionMapsTheTransportKeys pins the button mapping and the refusal
// of the ones molo cannot honour: record, the channel keys and unknown values
// all become a no-op instead of a wrong command.
func TestButtonActionMapsTheTransportKeys(t *testing.T) {
	cases := map[int32]smtcAction{
		smtcButtonPlay:        smtcActionPlay,
		smtcButtonPause:       smtcActionPause,
		smtcButtonStop:        smtcActionStop,
		smtcButtonNext:        smtcActionNext,
		smtcButtonPrevious:    smtcActionPrevious,
		smtcButtonRecord:      smtcActionNone,
		smtcButtonFastForward: smtcActionNone,
		smtcButtonRewind:      smtcActionNone,
		smtcButtonChannelUp:   smtcActionNone,
		smtcButtonChannelDown: smtcActionNone,
		42:                    smtcActionNone,
	}
	for button, want := range cases {
		if got := buttonAction(button); got != want {
			t.Fatalf("buttonAction(%d) = %v, want %v", button, got, want)
		}
	}
}

// TestButtonsForOnlyOffersWiredControls pins the capability rule: a nil command
// leaves its button disabled, and PlayPause covers both halves when it is the
// only one wired.
func TestButtonsForOnlyOffersWiredControls(t *testing.T) {
	noop := func() {}

	if got := buttonsFor(Commands{}); got != (smtcButtons{}) {
		t.Fatalf("buttonsFor(empty) = %+v, want everything off", got)
	}

	got := buttonsFor(Commands{Play: noop, Next: noop})
	want := smtcButtons{Play: true, Next: true}
	if got != want {
		t.Fatalf("buttonsFor(play+next) = %+v, want %+v", got, want)
	}

	got = buttonsFor(Commands{PlayPause: noop, Previous: noop})
	want = smtcButtons{Play: true, Pause: true, Previous: true}
	if got != want {
		t.Fatalf("buttonsFor(playpause+previous) = %+v, want %+v", got, want)
	}
}

// TestArtFileNameFollowsTheBytes pins the cache file name: the content key is
// kept so the same cover is written once, and the extension follows the
// declared type because the shell decodes the file.
func TestArtFileNameFollowsTheBytes(t *testing.T) {
	if got := artFileName("abc", "image/png"); got != "smtc-abc.png" {
		t.Fatalf("png art = %q", got)
	}
	if got := artFileName("abc", "image/jpeg"); got != "smtc-abc.jpg" {
		t.Fatalf("jpeg art = %q", got)
	}
	if got := artFileName("abc", ""); got != "smtc-abc.jpg" {
		t.Fatalf("undeclared art = %q", got)
	}
}
