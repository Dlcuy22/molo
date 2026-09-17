package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/meta"
)

func TestFormatClock(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0:00"},
		{500 * time.Millisecond, "0:00"},
		{time.Second, "0:01"},
		{61 * time.Second, "1:01"},
		{10*time.Minute + 5*time.Second, "10:05"},
		{time.Hour, "1:00:00"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1:02:03"},
		{-time.Second, "0:00"},
	}

	for _, tc := range cases {
		if got := formatClock(tc.in); got != tc.want {
			t.Errorf("formatClock(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatProgressStates(t *testing.T) {
	cases := []struct {
		name string
		snap player.Snapshot
		want string
	}{
		{
			name: "idle",
			snap: player.Snapshot{State: player.Idle},
			want: "idle",
		},
		{
			name: "playing known duration",
			snap: player.Snapshot{
				State:      player.Playing,
				Path:       "/music/short_stereo.opus",
				Position:   1500 * time.Millisecond,
				Duration:   2 * time.Second,
				Volume:     1,
				QueueIndex: 0,
				QueueLen:   3,
			},
			want: "playing  0:01 / 0:02  75%  vol 100%  [1/3] short_stereo.opus",
		},
		{
			name: "playing unknown duration is indeterminate",
			snap: player.Snapshot{
				State:      player.Playing,
				Path:       "/music/a.opus",
				Position:   1500 * time.Millisecond,
				Volume:     1,
				QueueIndex: 0,
				QueueLen:   1,
			},
			want: "playing  0:01 / --:--  vol 100%  [1/1] a.opus",
		},
		{
			name: "paused",
			snap: player.Snapshot{
				State:      player.Paused,
				Path:       "/music/a.opus",
				Position:   0,
				Duration:   2 * time.Second,
				Volume:     0.4,
				QueueIndex: 1,
				QueueLen:   3,
			},
			want: "paused   0:00 / 0:02   0%  vol  40%  [2/3] a.opus",
		},
		{
			name: "stopped",
			snap: player.Snapshot{
				State:      player.Stopped,
				Path:       "/music/a.opus",
				Position:   2 * time.Second,
				Duration:   2 * time.Second,
				Volume:     1,
				QueueIndex: 0,
				QueueLen:   1,
			},
			want: "stopped  0:02 / 0:02 100%  vol 100%  [1/1] a.opus",
		},
		{
			name: "title beats file name",
			snap: player.Snapshot{
				State:    player.Playing,
				Path:     "/music/track01.opus",
				Meta:     meta.Meta{Tags: meta.Tags{Title: "Real Song"}},
				Duration: time.Minute,
				Volume:   1,
			},
			want: "playing  0:00 / 1:00   0%  vol 100%  Real Song",
		},
		{
			name: "position past duration clamps",
			snap: player.Snapshot{
				State:    player.Playing,
				Path:     "/music/a.opus",
				Position: 5 * time.Second,
				Duration: 2 * time.Second,
				Volume:   1,
			},
			want: "playing  0:05 / 0:02 100%  vol 100%  a.opus",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatProgress(tc.snap); got != tc.want {
				t.Errorf("formatProgress() =\n %q\nwant\n %q", got, tc.want)
			}
		})
	}
}

// TestFormatProgressUnknownDurationHasNoPercent is the load-bearing assertion
// behind requirement 3: an unknown duration must never be shown as a bogus
// percentage or a zero-looking total.
func TestFormatProgressUnknownDurationHasNoPercent(t *testing.T) {
	const want = "playing  0:03 / --:--  vol 100%  a.opus"

	line := formatProgress(player.Snapshot{
		State:    player.Playing,
		Path:     "/music/a.opus",
		Position: 3 * time.Second,
		Volume:   1,
	})

	if line != want {
		t.Fatalf("formatProgress() = %q, want %q", line, want)
	}

	// Guard the percentage field specifically. A loose "0%" check would also
	// match the volume column's "100%".
	head, _, ok := strings.Cut(line, "  vol ")
	if !ok {
		t.Fatalf("no volume column in %q", line)
	}
	if strings.Contains(head, "%") {
		t.Fatalf("unknown duration produced a percentage before the volume column: %q", line)
	}
}

func TestFormatProgressZeroVolume(t *testing.T) {
	line := formatProgress(player.Snapshot{State: player.Playing, Path: "a.opus", Volume: 0})
	if !strings.Contains(line, "vol   0%") {
		t.Fatalf("zero volume not padded: %q", line)
	}
}
