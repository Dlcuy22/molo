package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/ui/webui/internal/cover"
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
	for _, ext := range molo.SupportedExtensions() {
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
		state molo.State
		want  string
	}{
		{molo.Idle, "idle"},
		{molo.Playing, "playing"},
		{molo.Paused, "paused"},
		{molo.Stopped, "stopped"},
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
	svc := newPlayerService()
	rows := svc.queueRows([]string{"/a/one.flac", "/b/two.flac"}, 1)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Active || !rows[1].Active {
		t.Errorf("active flags = %v, %v; want false, true", rows[0].Active, rows[1].Active)
	}
	if rows[1].Name != "two.flac" {
		t.Errorf("name = %q, want two.flac", rows[1].Name)
	}

	none := svc.queueRows([]string{"/a/one.flac"}, -1)
	if none[0].Active {
		t.Error("a row is active with index -1")
	}
}

// TestLoadPathsAppendsWhenAQueueExists is the add-folder bug: with a track
// already loaded, adding more files must extend the queue rather than replace
// it and cut the track off. With nothing queued it still replaces and starts.
func TestLoadPathsAppendsWhenAQueueExists(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		return p
	}
	a, b := write("a.flac"), write("b.flac")

	// Empty queue: the first load replaces and starts.
	fp := newFakePlayer()
	svc := newPlayerService()
	svc.player = fp
	if err := svc.LoadPaths([]string{a}); err != nil {
		t.Fatalf("LoadPaths(first): %v", err)
	}
	if got := fp.Queue(); len(got) != 1 || got[0] != a {
		t.Fatalf("queue after first load = %q, want [%q]", got, a)
	}
	if !fp.called("playQueue:1") {
		t.Fatalf("calls = %v, want a PlayQueue for the first load", fp.callsSnapshot())
	}

	// Non-empty queue: the second load appends and does not start a new track.
	fp.mu.Lock()
	fp.state = molo.Playing
	fp.path = a
	fp.mu.Unlock()
	if err := svc.LoadPaths([]string{b}); err != nil {
		t.Fatalf("LoadPaths(second): %v", err)
	}
	got := fp.Queue()
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("queue after second load = %q, want [%q %q]", got, a, b)
	}
	if !fp.called("insert:" + b) {
		t.Fatalf("calls = %v, want the second load to append", fp.callsSnapshot())
	}
	if snap := fp.Snapshot(); snap.Path != a {
		t.Fatalf("second load changed the live track to %q, want %q", snap.Path, a)
	}
}

// TestSetShuffleReachesTheEngine pins the toggle's command wiring.
func TestSetShuffleReachesTheEngine(t *testing.T) {
	fp := newFakePlayer()
	svc := newPlayerService()
	svc.player = fp
	if err := svc.SetShuffle(true); err != nil {
		t.Fatalf("SetShuffle(true): %v", err)
	}
	if got := fp.callsSnapshot(); len(got) == 0 || got[len(got)-1] != "shuffle:true" {
		t.Fatalf("calls = %v, want shuffle:true", got)
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

// TestCoverIsEmptyWithoutArtwork covers the two no-art answers: an empty id, and
// an id the snapshot does not carry. Both must return "" rather than fall
// through to the wrong track's bytes.
func TestCoverIsEmptyWithoutArtwork(t *testing.T) {
	svc := newPlayerService()
	p, err := molo.New()
	if err != nil {
		t.Fatalf("molo.New: %v", err)
	}
	defer p.Close()
	svc.player = p

	if got := svc.Cover(""); got != "" {
		t.Errorf("Cover(\"\") = %q, want empty", got)
	}
	if got := svc.Cover("deadbeef"); got != "" {
		t.Errorf("Cover with an id the snapshot does not carry = %q, want empty", got)
	}
}

// TestCoverCachesByIdentity checks the memoisation contract: the same bytes are
// decoded once and served from the cache afterwards, which is what keeps a
// repeated snapshot from re-encoding art.
func TestCoverCachesByIdentity(t *testing.T) {
	svc := newPlayerService()

	art := []byte("not really a png, but the cache does not care")
	id := cover.ID(art)
	if id == "" {
		t.Fatal("cover.ID returned empty for non-empty bytes")
	}

	// Seed the cache directly: Cover's own decode path is covered by the cover
	// package, and this test is about the service's memoisation.
	svc.coverCache[id] = "data:image/jpeg;base64,cached"

	if got := svc.Cover(id); got != "data:image/jpeg;base64,cached" {
		t.Fatalf("Cover(%q) = %q, want the cached value", id, got)
	}
}

// TestCoverCacheEvictsOldest pins the bound: a session that plays more tracks
// than the cap must not grow the cache without limit, and the most recent
// entries must survive.
func TestCoverCacheEvictsOldest(t *testing.T) {
	svc := newPlayerService()
	for i := 0; i < coverCacheMax+10; i++ {
		svc.storeCover(fmt.Sprintf("id-%d", i), fmt.Sprintf("url-%d", i))
	}

	if len(svc.coverCache) != coverCacheMax {
		t.Fatalf("cache holds %d entries, want the cap %d", len(svc.coverCache), coverCacheMax)
	}
	if _, ok := svc.coverCache["id-0"]; ok {
		t.Error("oldest entry was not evicted")
	}
	last := fmt.Sprintf("id-%d", coverCacheMax+9)
	if _, ok := svc.coverCache[last]; !ok {
		t.Errorf("newest entry %q was evicted", last)
	}

	// Re-storing an existing id must not duplicate it in the order list.
	svc.storeCover(last, "updated")
	if len(svc.coverOrder) != coverCacheMax {
		t.Fatalf("order list grew to %d after a re-store, want %d", len(svc.coverOrder), coverCacheMax)
	}
	if svc.coverCache[last] != "updated" {
		t.Errorf("re-store did not update the value: %q", svc.coverCache[last])
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
