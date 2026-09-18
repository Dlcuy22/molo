package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/meta"
)

// TestDebugLogIsBounded is the load-bearing memory assertion: the log drops the
// oldest line rather than growing with the session.
func TestDebugLogIsBounded(t *testing.T) {
	var log debugLog
	for i := 0; i < maxDebugLines+50; i++ {
		log.push("line " + strconv.Itoa(i))
	}

	if len(log.lines) != maxDebugLines {
		t.Fatalf("log holds %d lines, want the bound %d", len(log.lines), maxDebugLines)
	}
	if log.lines[0] != "line 50" {
		t.Fatalf("oldest kept line = %q, want the 51st pushed", log.lines[0])
	}
	if want := "line " + strconv.Itoa(maxDebugLines+49); log.lines[len(log.lines)-1] != want {
		t.Fatalf("newest line = %q, want %q", log.lines[len(log.lines)-1], want)
	}
}

// TestDebugFormatters pins the documented line shapes so a UI change cannot
// silently reshape the log.
func TestDebugFormatters(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"decoder", formatDecoderLine("pion/opus", "pion/opus/pkg/oggreader"), "decoder  pion/opus  parser  pion/opus/pkg/oggreader"},
		{"meta", formatMetaLine("embedded-tags"), "meta     embedded-tags"},
		{"state", formatStateLine(player.Playing), "state    playing"},
		{"eos", formatEOSLine("/music/stereo_2s.opus"), "eos      stereo_2s.opus"},
		{"seek", formatSeekLine(12*time.Second, 42*time.Second, 3*time.Millisecond), "seek     0:12 -> 0:42  took 3ms"},
		{"error", formatErrorLine(errFailed), "error    test failure"},
		{"track", formatTrackLine("/music/a.opus"), "track    a.opus"},
		{"codec", formatCodecLine("opus-pion"), "codec    opus-pion"},
		{"codec auto", formatCodecLine(""), "codec    auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// TestUpdateLogsEveryTrigger drives one message per documented trigger and
// proves each produces a log line.
func TestUpdateLogsEveryTrigger(t *testing.T) {
	f := newFakePlayer()
	f.snap = playerSnapshot("/music/a.opus", 10*time.Second, time.Minute, meta.Meta{})
	f.snap.Decoder = "pion/opus"
	f.snap.Parser = "pion/opus/pkg/oggreader"
	f.snap.Meta.Source = "embedded-tags"

	m := newModel(f)
	m.height = 40

	next, _ := m.Update(trackMsg{index: 0, path: "/music/a.opus"})
	m = next.(model)

	next, _ = m.Update(tickMsg(time.Now()))
	m = next.(model)

	next, _ = m.Update(stateMsg{to: player.Paused})
	m = next.(model)

	next, _ = m.Update(seekedMsg{position: 42 * time.Second, from: 12 * time.Second, elapsed: 3 * time.Millisecond})
	m = next.(model)

	next, _ = m.Update(endedMsg{})
	m = next.(model)

	next, _ = m.Update(failedMsg{err: errFailed})
	m = next.(model)

	joined := strings.Join(m.debug.lines, "\n")
	for _, want := range []string{
		"track    a.opus",
		"decoder  pion/opus  parser  pion/opus/pkg/oggreader",
		"meta     embedded-tags",
		"state    paused",
		"seek     0:12 -> 0:42  took 3ms",
		"eos      a.opus",
		"error    test failure",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("log is missing %q:\n%s", want, joined)
		}
	}
}

// TestUpdateLogsDecoderAndMetaOnce proves a per-tick poll does not repeat the
// identity lines: they belong to the track, not the frame.
func TestUpdateLogsDecoderAndMetaOnce(t *testing.T) {
	f := newFakePlayer()
	f.snap = playerSnapshot("/music/a.opus", 0, time.Minute, meta.Meta{})
	f.snap.Decoder = "pion/opus"
	f.snap.Parser = "pion/opus/pkg/oggreader"
	f.snap.Meta.Source = "embedded-tags"

	m := newModel(f)
	m.height = 40

	next, _ := m.Update(trackMsg{index: 0, path: "/music/a.opus"})
	m = next.(model)

	for i := 0; i < 4; i++ {
		next, _ = m.Update(tickMsg(time.Now()))
		m = next.(model)
	}

	joined := strings.Join(m.debug.lines, "\n")
	if got := strings.Count(joined, "decoder  pion/opus"); got != 1 {
		t.Fatalf("decoder line logged %d times, want exactly 1:\n%s", got, joined)
	}
	if got := strings.Count(joined, "meta     embedded-tags"); got != 1 {
		t.Fatalf("meta line logged %d times, want exactly 1:\n%s", got, joined)
	}
}

// TestTrackChangeRearmsIdentityLogging proves the once-per-track guard is reset
// by a track change, so the next track logs its own labels.
func TestTrackChangeRearmsIdentityLogging(t *testing.T) {
	f := newFakePlayer()
	f.snap = playerSnapshot("/music/a.opus", 0, time.Minute, meta.Meta{})
	f.snap.Decoder = "codec-a"
	f.snap.Meta.Source = "source-a"

	m := newModel(f)
	m.height = 40

	next, _ := m.Update(trackMsg{index: 0, path: "/music/a.opus"})
	m = next.(model)
	next, _ = m.Update(tickMsg(time.Now()))
	m = next.(model)

	f.setSnapshot(func(s *player.Snapshot) {
		s.Path = "/music/b.opus"
		s.Decoder = "codec-b"
		s.Parser = "parser-b"
		s.Meta.Source = "source-b"
	})
	next, _ = m.Update(trackMsg{index: 1, path: "/music/b.opus"})
	m = next.(model)
	next, _ = m.Update(tickMsg(time.Now()))
	m = next.(model)

	joined := strings.Join(m.debug.lines, "\n")
	if got := strings.Count(joined, "decoder  codec-b"); got != 1 {
		t.Fatalf("new track decoder logged %d times, want 1:\n%s", got, joined)
	}
	if got := strings.Count(joined, "meta     source-b"); got != 1 {
		t.Fatalf("new track meta logged %d times, want 1:\n%s", got, joined)
	}
}

// TestRenderDebugPanelShowsSeek proves the seek line reaches the painted frame
// with its origin, target and duration.
func TestRenderDebugPanelShowsSeek(t *testing.T) {
	m := newModel(newFakePlayer())
	m.width, m.height = 80, 40
	m.snap = playerSnapshot("/music/a.opus", 42*time.Second, time.Minute, meta.Meta{})
	m.debug.push(formatSeekLine(12*time.Second, 42*time.Second, 3*time.Millisecond))

	frame := stripANSI(m.render())
	if !strings.Contains(frame, "debug") {
		t.Fatalf("panel header missing:\n%s", frame)
	}
	if !strings.Contains(frame, "seek     0:12 -> 0:42  took 3ms") {
		t.Fatalf("seek line missing from the frame:\n%s", frame)
	}
}

// TestDebugPanelOmittedWhenEmpty proves a model with nothing to report paints
// no panel at all, so a clean session does not grow a stray header.
func TestDebugPanelOmittedWhenEmpty(t *testing.T) {
	m := newModel(newFakePlayer())
	m.width, m.height = 80, 40
	m.snap = playerSnapshot("/music/a.opus", 0, time.Minute, meta.Meta{})

	if frame := stripANSI(m.render()); strings.Contains(frame, "debug") {
		t.Fatalf("empty log rendered a panel:\n%s", frame)
	}
}

// TestShortTerminalKeepsMainPanel is the layout guard: a cramped window with a
// full log must still show the now-playing panel and the progress line, because
// the panel only gets the space left over.
func TestShortTerminalKeepsMainPanel(t *testing.T) {
	m := newModel(newFakePlayer())
	m.width, m.height = 60, 8
	m.snap = playerSnapshot("/music/a.opus", 31*time.Second, 2*time.Minute, meta.Meta{Tags: meta.Tags{Title: "Real Track"}})
	m.queue = []string{"/music/a.opus", "/music/b.opus"}
	m.titles = []string{"Real Track", ""}

	for i := 0; i < maxDebugLines; i++ {
		m.debug.push("line " + strconv.Itoa(i))
	}

	frame := stripANSI(m.render())
	if !strings.Contains(frame, "Real Track") {
		t.Fatalf("main panel title missing on a short terminal:\n%s", frame)
	}
	if !strings.Contains(frame, "0:31") {
		t.Fatalf("progress line missing on a short terminal:\n%s", frame)
	}
}

// TestDebugPanelPrefersNewest proves the tail is kept when the height budget
// cannot fit every line.
func TestDebugPanelPrefersNewest(t *testing.T) {
	m := newModel(newFakePlayer())
	m.width, m.height = 80, 14
	m.snap = playerSnapshot("/music/a.opus", 0, time.Minute, meta.Meta{})

	for i := 0; i < maxDebugLines; i++ {
		m.debug.push("line " + strconv.Itoa(i))
	}

	frame := stripANSI(m.render())
	if !strings.Contains(frame, "line 199") {
		t.Fatalf("newest line missing from a cramped panel:\n%s", frame)
	}
	if strings.Contains(frame, "line 0") {
		t.Fatalf("oldest line shown despite a tight budget:\n%s", frame)
	}
}
