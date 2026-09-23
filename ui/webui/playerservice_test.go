package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlcuy22/player"
)

// TestExpandSkipsUnplayableFiles is the queue-building contract: a folder yields
// only the files the engine can decode, sorted, and a duplicate survives once.
func TestExpandSkipsUnplayableFiles(t *testing.T) {
	root := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		return p
	}

	// One playable file per extension the engine advertises, plus files that
	// must be skipped: cover art and a playlist are what an album folder holds.
	var want []string
	for _, ext := range player.SupportedExtensions() {
		want = append(want, write("track"+ext))
	}
	write("cover.jpg")
	write("notes.txt")

	svc := newPlayerService()
	got, err := svc.expand([]string{root})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("expand returned %d files, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d = %q, want %q (sorted)", i, got[i], want[i])
		}
	}
}

// TestExpandDeduplicates checks that naming a file and its folder does not
// queue the file twice, which is the whole point of the seen set.
func TestExpandDeduplicates(t *testing.T) {
	root := t.TempDir()
	track := filepath.Join(root, "song.flac")
	if err := os.WriteFile(track, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := newPlayerService()
	got, err := svc.expand([]string{track, root})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expand returned %d files, want 1: %v", len(got), got)
	}
}

// TestExpandRejectsMissingPath keeps a typo an error rather than a silent empty
// queue that looks like a hang.
func TestExpandRejectsMissingPath(t *testing.T) {
	svc := newPlayerService()
	if _, err := svc.expand([]string{filepath.Join(t.TempDir(), "nope.flac")}); err == nil {
		t.Fatal("expand accepted a missing path")
	}
}

// TestStateNameCoversEveryState pins the wire strings the frontend switches on.
func TestStateNameCoversEveryState(t *testing.T) {
	cases := []struct {
		state player.State
		want  string
	}{
		{player.Idle, "idle"},
		{player.Playing, "playing"},
		{player.Paused, "paused"},
		{player.Stopped, "stopped"},
	}
	for _, c := range cases {
		if got := stateName(c.state); got != c.want {
			t.Errorf("stateName(%v) = %q, want %q", c.state, got, c.want)
		}
	}
}

// TestQueueRowsMarksExactlyOne checks the active marker lands on the current
// index and no row is marked when nothing is playing.
func TestQueueRowsMarksExactlyOne(t *testing.T) {
	rows := queueRows([]string{"/a/one.flac", "/b/two.flac"}, 1)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Active || !rows[1].Active {
		t.Errorf("active flags = %v, %v; want false, true", rows[0].Active, rows[1].Active)
	}
	if rows[1].Name != "two.flac" {
		t.Errorf("name = %q, want two.flac", rows[1].Name)
	}

	none := queueRows([]string{"/a/one.flac"}, -1)
	if none[0].Active {
		t.Error("a row is active with index -1")
	}
}

// TestClamp01 pins the gain clamp, which guards the engine's range check.
func TestClamp01(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{-1, 0}, {0, 0}, {0.5, 0.5}, {1, 1}, {2, 1},
	} {
		if got := clamp01(c.in); got != c.want {
			t.Errorf("clamp01(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestPositionIntervalIsFourHz pins the snapshot poll both comments and the
// README promise: 4 Hz like the TUI. It regressed to 5000 ms once while the
// comments still said 4 Hz, which froze the progress bar to one update every
// five seconds.
func TestPositionIntervalIsFourHz(t *testing.T) {
	if positionInterval != 250*time.Millisecond {
		t.Fatalf("positionInterval = %v, want 250ms (4 Hz)", positionInterval)
	}
}

// TestSuppressSpectrumSkipsRepeats pins the bridge-side frame filter: an
// identical repeat (the runner holding its display with no new tap samples,
// e.g. paused) and steady near-silence after the drain must not each cost an
// event, while a changed frame and the transition into silence must.
func TestSuppressSpectrumSkipsRepeats(t *testing.T) {
	svc := newPlayerService()

	frame := []float64{0.5, 0.25, 0.1}
	if svc.suppressSpectrum(frame) {
		t.Fatal("first frame suppressed, nothing would ever reach the display")
	}
	if !svc.suppressSpectrum(frame) {
		t.Fatal("identical repeat emitted, a held display would push 30 identical frames a second")
	}
	if svc.suppressSpectrum([]float64{0.5, 0.25, 0.2}) {
		t.Fatal("changed frame suppressed")
	}
	if svc.suppressSpectrum([]float64{0, 0, 0}) {
		t.Fatal("transition into silence suppressed, the bars would freeze instead of draining")
	}
	if !svc.suppressSpectrum([]float64{0, 0, 0}) {
		t.Fatal("steady silence emitted, the bridge would push empty frames forever")
	}
}
