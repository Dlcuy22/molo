package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/molo/meta"
)

// ansiRE matches SGR colour sequences so a test can assert on the text a
// terminal would show rather than on the escape codes around it.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// TestRenderQueueMarksActiveIndex is the load-bearing queue assertion: exactly
// one row carries the active marker, and it is the row at the active index.
func TestRenderQueueMarksActiveIndex(t *testing.T) {
	queue := []string{
		"/music/a.opus",
		"/music/b.opus",
		"/music/c.opus",
	}

	out := stripANSI(renderQueue(queue, 1, 40))
	lines := strings.Split(out, "\n")
	if len(lines) != len(queue) {
		t.Fatalf("renderQueue produced %d lines for %d tracks: %q", len(lines), len(queue), out)
	}

	for i, line := range lines {
		marked := strings.HasPrefix(strings.TrimSpace(line), activeMarker)
		if want := i == 1; marked != want {
			t.Fatalf("line %d marked=%v, want %v (line %q)", i, marked, want, line)
		}
	}
	if !strings.Contains(lines[1], "b.opus") {
		t.Fatalf("active row does not name the track: %q", lines[1])
	}
}

func TestRenderQueueFallsBackToFileName(t *testing.T) {
	out := renderQueue([]string{"/deep/path/track01.opus"}, 0, 40)
	if strings.Contains(out, "/deep/path") {
		t.Fatalf("queue rendered a full path instead of the file name: %q", out)
	}
	if !strings.Contains(out, "track01.opus") {
		t.Fatalf("queue lost the file name: %q", out)
	}
}

// TestRenderQueueNoActiveMarkerWhenStopped proves a queue with no current track
// (-1) marks nothing rather than marking the first row by accident.
func TestRenderQueueNoActiveMarkerWhenStopped(t *testing.T) {
	out := renderQueue([]string{"a.opus", "b.opus"}, -1, 40)
	if strings.Contains(out, activeMarker) {
		t.Fatalf("queue with index -1 has an active marker: %q", out)
	}
}

func TestRenderQueueEmptyIsEmpty(t *testing.T) {
	if got := renderQueue(nil, -1, 40); got != "" {
		t.Fatalf("empty queue rendered %q, want empty", got)
	}
}

func TestQueueTitlesUseMetaTags(t *testing.T) {
	m := newModel(newFakePlayer())
	m.queue = []string{"/music/a.opus", "/music/b.opus"}
	m.titles = []string{"Real A", ""}

	out := m.renderQueueView(40)
	if !strings.Contains(out, "Real A") {
		t.Fatalf("resolved title missing from queue: %q", out)
	}
	if !strings.Contains(out, "b.opus") {
		t.Fatalf("unresolved track missing from queue: %q", out)
	}
}

// TestTrackChangeResetsUIState is the load-bearing reset assertion: a new track
// must clear the previous track's title, clock, error and meter instead of
// showing stale data for the new one. Titles learned for other queue rows are
// per-track cache and deliberately survive.
func TestTrackChangeResetsUIState(t *testing.T) {
	m := newModel(newFakePlayer())
	m.queue = []string{"/music/a.opus", "/music/b.opus"}
	m.titles = []string{"Old Title", "Stale B"}
	m.snap = playerSnapshot(
		"/music/a.opus",
		90*time.Second,
		120*time.Second,
		meta.Meta{Tags: meta.Tags{Title: "Old Title", Artist: "Old Artist", Album: "Old Album"}},
	)
	m.err = errFailed
	m.meter.push(1, time.Time{})

	next, _ := m.Update(trackMsg{index: 1, path: "/music/b.opus"})
	got := next.(model)

	if got.snap.Meta.Tags.Title != "" || got.snap.Meta.Tags.Artist != "" || got.snap.Meta.Tags.Album != "" {
		t.Fatalf("tags survived a track change: %+v", got.snap.Meta.Tags)
	}
	if got.snap.Position != 0 {
		t.Fatalf("position survived a track change: %v", got.snap.Position)
	}
	if got.snap.Duration != 0 {
		t.Fatalf("duration survived a track change: %v", got.snap.Duration)
	}
	if got.snap.QueueIndex != 1 || got.snap.Path != "/music/b.opus" {
		t.Fatalf("track change not applied: %+v", got.snap)
	}
	if got.err != nil {
		t.Fatalf("previous error survived a track change: %v", got.err)
	}
	if got.meter.level != 0 {
		t.Fatalf("meter level survived a track change: %v", got.meter.level)
	}
	if got.titles[1] != "" {
		t.Fatalf("the new track's cached title was not cleared: %q", got.titles[1])
	}
	if got.titles[0] != "Old Title" {
		t.Fatalf("another row's learned title was lost: %q", got.titles[0])
	}
}
