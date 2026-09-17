package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/player"
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
		got  string
		want string
	}{
		{
			"playing known duration",
			formatProgress(player.Playing, 1500*time.Millisecond, 2*time.Second),
			"playing  0:01 / 0:02  75%",
		},
		{
			"paused",
			formatProgress(player.Paused, 0, 2*time.Second),
			"paused   0:00 / 0:02   0%",
		},
		{
			"stopped",
			formatProgress(player.Stopped, 2*time.Second, 2*time.Second),
			"stopped  0:02 / 0:02 100%",
		},
		{
			"idle",
			formatProgress(player.Idle, 0, 0),
			"idle",
		},
		{
			"position past duration clamps",
			formatProgress(player.Playing, 5*time.Second, 2*time.Second),
			"playing  0:05 / 0:02 100%",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("formatProgress() = %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// TestFormatProgressUnknownDuration is the load-bearing assertion behind the
// "never a bogus percentage" requirement: a zero duration means the probe has
// not answered, so the total is indeterminate and no percentage may appear.
func TestFormatProgressUnknownDuration(t *testing.T) {
	cases := []struct {
		name  string
		state player.State
		pos   time.Duration
		dur   time.Duration
		want  string
	}{
		{"zero duration", player.Playing, 3 * time.Second, 0, "playing  0:03 / --:--"},
		{"negative duration", player.Paused, 3 * time.Second, -time.Second, "paused   0:03 / --:--"},
		{"zero position and duration", player.Playing, 0, 0, "playing  0:00 / --:--"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatProgress(tc.state, tc.pos, tc.dur)
			if got != tc.want {
				t.Fatalf("formatProgress() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "%") {
				t.Fatalf("unknown duration produced a percentage: %q", got)
			}
		})
	}
}

func TestProgressPercent(t *testing.T) {
	cases := []struct {
		pos, dur time.Duration
		want     int
		ok       bool
	}{
		{0, 0, 0, false},
		{time.Second, 0, 0, false},
		{time.Second, -time.Second, 0, false},
		{time.Second, 2 * time.Second, 50, true},
		{2 * time.Second, 2 * time.Second, 100, true},
		{3 * time.Second, 2 * time.Second, 100, true},
		{-time.Second, 2 * time.Second, 0, true},
	}

	for _, tc := range cases {
		got, ok := progressPercent(tc.pos, tc.dur)
		if got != tc.want || ok != tc.ok {
			t.Errorf("progressPercent(%v, %v) = (%d, %v), want (%d, %v)",
				tc.pos, tc.dur, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRenderBarKnownDuration(t *testing.T) {
	cases := []struct {
		name  string
		pos   time.Duration
		dur   time.Duration
		width int
		want  string
	}{
		{"empty", 0, 2 * time.Second, 8, "[--------]"},
		{"half", time.Second, 2 * time.Second, 8, "[####----]"},
		{"full", 2 * time.Second, 2 * time.Second, 8, "[########]"},
		{"clamped past the end", 5 * time.Second, 2 * time.Second, 8, "[########]"},
		{"zero width", time.Second, 2 * time.Second, 0, "[]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderBar(tc.pos, tc.dur, tc.width, 0); got != tc.want {
				t.Errorf("renderBar() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderBarIndeterminate proves an unknown duration animates instead of
// faking a filled fraction. The marker must move with the frame and the output
// must never carry a percentage.
func TestRenderBarIndeterminate(t *testing.T) {
	a := renderBar(3*time.Second, 0, 8, 0)
	b := renderBar(3*time.Second, 0, 8, 1)

	if !strings.HasPrefix(a, "[") || !strings.HasSuffix(a, "]") {
		t.Fatalf("indeterminate bar has the wrong shape: %q", a)
	}
	if got := []rune(a); len(got) != 10 {
		t.Fatalf("indeterminate bar has %d runes, want 10: %q", len(got), a)
	}
	if a == b {
		t.Fatalf("indeterminate marker did not move between frames: %q == %q", a, b)
	}
	if strings.Contains(a, "%") {
		t.Fatalf("indeterminate bar contains a percentage: %q", a)
	}
	if got := renderBar(0, 0, 0, 5); got != "[]" {
		t.Fatalf("zero-width indeterminate bar = %q, want []", got)
	}
}

func TestVolumePercent(t *testing.T) {
	cases := []struct {
		in   float64
		want int
	}{
		{0, 0},
		{1, 100},
		{0.4, 40},
		{-0.1, 0},
		{1.5, 100},
	}

	for _, tc := range cases {
		if got := volumePercent(tc.in); got != tc.want {
			t.Errorf("volumePercent(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestStateName(t *testing.T) {
	cases := map[player.State]string{
		player.Idle:    "idle",
		player.Playing: "playing",
		player.Paused:  "paused",
		player.Stopped: "stopped",
	}
	for state, want := range cases {
		if got := stateName(state); got != want {
			t.Errorf("stateName(%v) = %q, want %q", state, got, want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	if got := displayName("Real Song", "/music/track01.opus"); got != "Real Song" {
		t.Errorf("tag title not preferred: %q", got)
	}
	if got := displayName("", "/music/track01.opus"); got != "track01.opus" {
		t.Errorf("path fallback = %q, want track01.opus", got)
	}
	if got := displayName("", ""); got != "" {
		t.Errorf("empty name = %q, want empty", got)
	}
}
