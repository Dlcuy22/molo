package tui

import (
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/player"
)

// TestTranslateEvent covers every member of the sealed event set: the UI must
// turn each engine event into exactly one message, and nothing may be silently
// swallowed.
func TestTranslateEvent(t *testing.T) {
	boom := errors.New("boom")

	cases := []struct {
		name string
		ev   player.Event
		want tea.Msg
	}{
		{
			"state changed",
			player.StateChanged{From: player.Playing, To: player.Paused},
			stateMsg{to: player.Paused},
		},
		{
			"track changed",
			player.TrackChanged{Index: 2, Path: "/music/c.opus"},
			trackMsg{index: 2, path: "/music/c.opus"},
		},
		{
			"seeked",
			player.Seeked{Position: 42 * time.Second},
			seekedMsg{position: 42 * time.Second},
		},
		{
			"track ended",
			player.TrackEnded{},
			endedMsg{},
		},
		{
			"failed",
			player.Failed{Err: boom},
			failedMsg{err: boom},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateEvent(tc.ev)
			if got != tc.want {
				t.Fatalf("translateEvent(%T) = %#v, want %#v", tc.ev, got, tc.want)
			}
		})
	}
}

// TestTranslateFailedKeepsError proves the failure message carries the error
// itself, so the UI can show a real reason rather than a generic string.
func TestTranslateFailedKeepsError(t *testing.T) {
	boom := errors.New("device unplugged")

	msg, ok := translateEvent(player.Failed{Err: boom}).(failedMsg)
	if !ok {
		t.Fatalf("Failed did not translate to failedMsg")
	}
	if !errors.Is(msg.err, boom) {
		t.Fatalf("failedMsg lost the error: %v", msg.err)
	}
}

// TestWaitForEvent is the bridge itself: one read must produce one translated
// message, and a closed channel must produce the sentinel that stops the drain.
func TestWaitForEvent(t *testing.T) {
	ch := make(chan player.Event, 1)
	ch <- player.TrackChanged{Index: 1, Path: "b.opus"}

	got := waitForEvent(ch)()
	if got != (trackMsg{index: 1, path: "b.opus"}) {
		t.Fatalf("waitForEvent() = %#v", got)
	}

	close(ch)
	if got := waitForEvent(ch)(); got != (eventsClosedMsg{}) {
		t.Fatalf("waitForEvent() on a closed channel = %#v, want eventsClosedMsg", got)
	}
}
