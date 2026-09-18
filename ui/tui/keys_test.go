package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/player"
)

// TestHandleKeyPauseResume covers the space bar across every state. The v2 key
// string for space is "space", not " ", and the toggle is a no-op when there is
// nothing to toggle.
func TestHandleKeyPauseResume(t *testing.T) {
	cases := []struct {
		name     string
		state    player.State
		wantCall string
	}{
		{"playing pauses", player.Playing, "Pause"},
		{"paused resumes", player.Paused, "Resume"},
		{"idle ignores", player.Idle, ""},
		{"stopped ignores", player.Stopped, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakePlayer()
			f.snap.State = tc.state

			if res := handleKey(f, keyPress("space")); res.quit {
				t.Fatalf("space requested quit")
			}
			if tc.wantCall == "" {
				if f.callCount() != 0 {
					t.Fatalf("space on %v called %v", tc.state, f.calls)
				}

				return
			}
			if !f.called(tc.wantCall) {
				t.Fatalf("space on %v did not call %s (calls: %v)", tc.state, tc.wantCall, f.calls)
			}
		})
	}
}

func TestHandleKeyQuit(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		f := newFakePlayer()
		if !handleKey(f, keyPress(k)).quit {
			t.Errorf("%q did not request quit", k)
		}
	}
}

func TestHandleKeyNextPrev(t *testing.T) {
	f := newFakePlayer()
	handleKey(f, keyPress("n"))
	handleKey(f, keyPress("p"))

	if !f.called("Next") || !f.called("Prev") {
		t.Fatalf("n/p did not call Next/Prev: %v", f.calls)
	}
}

// TestHandleKeySeek pins the mapping from a seek key to a relative delta. The
// model turns the delta into one batched engine call; handleKey must never
// reach the engine itself, because one key press cannot know the batch.
func TestHandleKeySeek(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want time.Duration
	}{
		{"forward by step", "l", seekStep},
		{"right seeks forward too", "right", seekStep},
		{"back by step", "h", -seekStep},
		{"left seeks back too", "left", -seekStep},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakePlayer()

			res := handleKey(f, keyPress(tc.key))
			if !res.seeking || res.delta != tc.want {
				t.Fatalf("%q -> %+v, want delta %v", tc.key, res, tc.want)
			}
			if f.callCount() != 0 {
				t.Fatalf("%q reached the engine directly: %v", tc.key, f.calls)
			}
		})
	}
}

// TestClampSeekTarget covers the arithmetic the batch applies when it flushes.
// A positive target past a known duration clamps to the end because a decoder
// can treat a past-the-end seek as a failure; a negative target clamps to zero.
func TestClampSeekTarget(t *testing.T) {
	cases := []struct {
		name        string
		base, delta time.Duration
		dur, want   time.Duration
	}{
		{"forward by step", 10 * time.Second, 5 * time.Second, 60 * time.Second, 15 * time.Second},
		{"back by step", 10 * time.Second, -5 * time.Second, 60 * time.Second, 5 * time.Second},
		{"back past zero clamps", 2 * time.Second, -5 * time.Second, 60 * time.Second, 0},
		{"forward past known end clamps", 59 * time.Second, 5 * time.Second, 60 * time.Second, 60 * time.Second},
		{"forward with unknown duration is unclamped", 10 * time.Second, 5 * time.Second, 0, 15 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampSeekTarget(tc.base, tc.delta, tc.dur); got != tc.want {
				t.Fatalf("clampSeekTarget(%v, %v, %v) = %v, want %v", tc.base, tc.delta, tc.dur, got, tc.want)
			}
		})
	}
}

func TestHandleKeyVolume(t *testing.T) {
	cases := []struct {
		name string
		key  string
		vol  float64
		want float64
	}{
		{"up", "+", 0.5, 0.6},
		{"equals also up", "=", 0.5, 0.6},
		{"down", "-", 0.5, 0.4},
		{"underscore also down", "_", 0.5, 0.4},
		{"up clamps at one", "+", 1, 1},
		{"down clamps at zero", "-", 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakePlayer()
			f.snap.Volume = tc.vol

			handleKey(f, keyPress(tc.key))
			if !f.called("SetVolume") {
				t.Fatalf("%q did not call SetVolume", tc.key)
			}
			if got := f.Snapshot().Volume; got != tc.want {
				t.Fatalf("volume = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHandleKeyUnknownIsIgnored is the no-panic guarantee for arbitrary input:
// a key that means nothing must reach the player exactly zero times.
func TestHandleKeyUnknownIsIgnored(t *testing.T) {
	f := newFakePlayer()
	for _, k := range []string{"", "x", "z", "enter", "tab", "up", "down", "F1", "shift+a", "ctrl+alt+delete"} {
		if res := handleKey(f, keyPress(k)); res.quit || res.seeking {
			t.Fatalf("%q produced %+v", k, res)
		}
	}
	if f.callCount() != 0 {
		t.Fatalf("unknown keys reached the player: %v", f.calls)
	}
}

func TestClampVolume(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{-1, 0},
		{0, 0},
		{0.5, 0.5},
		{1, 1},
		{2, 1},
	}
	for _, tc := range cases {
		if got := clampVolume(tc.in); got != tc.want {
			t.Errorf("clampVolume(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// keyPress builds the v2 key message for a small vocabulary of test inputs.
// It constructs the Key fields directly rather than parsing bytes, because the
// model only ever sees KeyPressMsg.
func keyPress(name string) tea.KeyPressMsg {
	switch name {
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "F1":
		return tea.KeyPressMsg{Code: tea.KeyF1}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+alt+delete":
		return tea.KeyPressMsg{Code: tea.KeyDelete, Mod: tea.ModCtrl | tea.ModAlt}
	case "shift+a":
		return tea.KeyPressMsg{Code: 'a', Mod: tea.ModShift}
	case "":
		return tea.KeyPressMsg{}
	default:
		if len(name) == 1 {
			return tea.KeyPressMsg{Code: rune(name[0]), Text: name}
		}
		panic("keyPress: unhandled name " + name)
	}
}
