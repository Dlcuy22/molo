package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/core"
)

// testEnv builds a non-interactive env wired to a fake player. makeRaw is nil,
// so a test that wants the interactive path must opt in.
func testEnv(t *testing.T, p *fakePlayer) (env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	e := env{
		stdin:        strings.NewReader(""),
		stdout:       &stdout,
		stderr:       &stderr,
		interactive:  false,
		pollInterval: time.Millisecond,
		signals:      make(chan os.Signal, 1),
		newPlayer: func(...player.Option) (player.Player, error) {
			return p, nil
		},
		probe: nil,
	}

	return e, &stdout, &stderr
}

func writeTrack(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	return path
}

func TestRunUsageErrorExitsTwo(t *testing.T) {
	e, _, stderr := testEnv(t, newFakePlayer())

	if got := run([]string{"-nope"}, e); got != exitUsage {
		t.Fatalf("run(-nope) = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "usage error") {
		t.Fatalf("stderr = %q, want a usage error", stderr.String())
	}
}

func TestRunHelpExitsZero(t *testing.T) {
	e, stdout, _ := testEnv(t, newFakePlayer())

	if got := run([]string{"-h"}, e); got != exitOK {
		t.Fatalf("run(-h) = %d, want %d", got, exitOK)
	}
	if !strings.Contains(stdout.String(), "usage:") {
		t.Fatalf("stdout = %q, want usage text", stdout.String())
	}
}

func TestRunNoPlayableInputExitsThree(t *testing.T) {
	dir := t.TempDir()
	e, _, _ := testEnv(t, newFakePlayer())

	if got := run([]string{dir}, e); got != exitNoInput {
		t.Fatalf("run(empty dir) = %d, want %d", got, exitNoInput)
	}
}

func TestRunListPrintsQueueAndExitsZero(t *testing.T) {
	dir := t.TempDir()
	b := writeTrack(t, dir, "b.opus")
	a := writeTrack(t, dir, "a.opus")

	e, stdout, _ := testEnv(t, newFakePlayer())
	if got := run([]string{"-list", dir}, e); got != exitOK {
		t.Fatalf("run(-list) = %d, want %d", got, exitOK)
	}

	want := a + "\n" + b + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunPlaysToCompletionNonInteractive(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	p := newFakePlayer()
	p.autoEnd = true

	e, stdout, stderr := testEnv(t, p)
	if got := run([]string{track}, e); got != exitOK {
		t.Fatalf("run = %d, want %d; stderr=%q", got, exitOK, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "playing") {
		t.Fatalf("stdout = %q, want a plain playing line", out)
	}
	// Requirement 4: no control codes when stdout is not a terminal.
	if strings.ContainsAny(out, "\r\x1b") {
		t.Fatalf("non-TTY output contains control codes: %q", out)
	}
	if !p.called("PlayQueue") {
		t.Fatal("PlayQueue was never called")
	}
}

func TestRunPlaybackFailureExitsOne(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	p := newFakePlayer()
	p.fail = errors.New("no audio device")

	e, _, stderr := testEnv(t, p)
	if got := run([]string{track}, e); got != exitFailure {
		t.Fatalf("run = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(stderr.String(), "no audio device") {
		t.Fatalf("stderr = %q, want the playback error", stderr.String())
	}
}

func TestRunSignalStopsCleanlyAndExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	p := newFakePlayer() // stays playing forever
	interrupt := make(chan os.Signal, 1)

	e, _, _ := testEnv(t, p)
	e.signals = interrupt

	go func() {
		// Give run a moment to start playback before interrupting.
		time.Sleep(20 * time.Millisecond)
		interrupt <- os.Interrupt
	}()

	if got := run([]string{track}, e); got == exitOK {
		t.Fatalf("run after SIGINT = %d, want non-zero", got)
	}
	if !p.closed {
		t.Fatal("player was not closed after a signal")
	}
}

func TestRunInteractiveRawModeIsRestoredAndQuitExitsZero(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	p := newFakePlayer() // stays playing so only the quit key ends it

	var (
		mu       sync.Mutex
		restored bool
	)
	e, stdout, _ := testEnv(t, p)
	e.interactive = true
	e.stdin = strings.NewReader("q")
	e.makeRaw = func() (func(), error) {
		return func() {
			mu.Lock()
			restored = true
			mu.Unlock()
		}, nil
	}

	got := run([]string{track}, e)
	if got != exitOK {
		t.Fatalf("run(q) = %d, want %d", got, exitOK)
	}

	mu.Lock()
	defer mu.Unlock()
	if !restored {
		t.Fatal("raw mode was not restored")
	}
	if !strings.Contains(stdout.String(), "\x1b[K") {
		t.Fatalf("interactive output = %q, want a repainted progress line", stdout.String())
	}
}

func TestRunRawModeFailureExitsFailure(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	e, _, stderr := testEnv(t, newFakePlayer())
	e.interactive = true
	e.makeRaw = func() (func(), error) { return nil, errors.New("not a tty") }

	if got := run([]string{track}, e); got != exitFailure {
		t.Fatalf("run = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(stderr.String(), "not a tty") {
		t.Fatalf("stderr = %q, want the raw-mode error", stderr.String())
	}
}

func TestRunKeyShowsProgressInInteractiveMode(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	p := newFakePlayer()
	p.autoEnd = true
	p.endDelay = 30 * time.Millisecond
	p.snap.Duration = 2 * time.Second

	e, stdout, _ := testEnv(t, p)
	e.interactive = true
	e.stdin = strings.NewReader("") // EOF, so no key can end it early
	e.makeRaw = func() (func(), error) { return func() {}, nil }

	if got := run([]string{track}, e); got != exitOK {
		t.Fatalf("run = %d, want %d", got, exitOK)
	}
	if !strings.Contains(stdout.String(), "playing") {
		t.Fatalf("interactive output = %q, want a progress line", stdout.String())
	}
	if !strings.Contains(stdout.String(), "\x1b[K") {
		t.Fatalf("interactive output = %q, want ANSI erase", stdout.String())
	}
}

func TestRunListDoesNotTouchThePlayer(t *testing.T) {
	dir := t.TempDir()
	writeTrack(t, dir, "a.opus")

	built := false
	e, _, _ := testEnv(t, newFakePlayer())
	e.newPlayer = func(...player.Option) (player.Player, error) {
		built = true

		return newFakePlayer(), nil
	}

	if got := run([]string{"-list", dir}, e); got != exitOK {
		t.Fatalf("run(-list) = %d, want %d", got, exitOK)
	}
	if built {
		t.Fatal("-list constructed a player")
	}
}

func TestRunProbeUnknownIgnoresDurations(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	p := newFakePlayer()
	p.autoEnd = true
	p.endDelay = 30 * time.Millisecond

	probeCalled := false
	e, stdout, _ := testEnv(t, p)
	e.interactive = true
	e.stdin = strings.NewReader("") // EOF, so the queue end is the only exit
	e.makeRaw = func() (func(), error) { return func() {}, nil }
	e.probe = func(string, core.DurationMode) (time.Duration, error) {
		probeCalled = true

		return 9 * time.Second, nil
	}

	// -probe=unknown must not preflight, so the display has no total to show.
	if got := run([]string{"-probe", "unknown", track}, e); got != exitOK {
		t.Fatalf("run = %d, want %d", got, exitOK)
	}
	if probeCalled {
		t.Fatal("-probe=unknown still ran the preflight probe")
	}
	if !strings.Contains(stdout.String(), "--:--") {
		t.Fatalf("output = %q, want an indeterminate clock", stdout.String())
	}
}

func TestRunProbePreflightFillsUnknownDuration(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	p := newFakePlayer()
	p.autoEnd = true
	p.endDelay = 30 * time.Millisecond

	probed := make(chan string, 1)
	e, stdout, _ := testEnv(t, p)
	e.interactive = true
	e.stdin = strings.NewReader("")
	e.makeRaw = func() (func(), error) { return func() {}, nil }
	e.probe = func(path string, mode core.DurationMode) (time.Duration, error) {
		probed <- path

		return 42 * time.Second, nil
	}

	if got := run([]string{"-probe", "scan", track}, e); got != exitOK {
		t.Fatalf("run = %d, want %d", got, exitOK)
	}

	select {
	case got := <-probed:
		if got != track {
			t.Fatalf("probed %q, want %q", got, track)
		}
	default:
		t.Fatal("the preflight probe was not called")
	}

	if !strings.Contains(stdout.String(), "0:42") {
		t.Fatalf("output = %q, want the preflight duration", stdout.String())
	}
}

func TestDisplayDurationPrefersTheEngineOverThePreflight(t *testing.T) {
	// The CLI preflight and the engine probe are two independent measurements
	// and can legitimately differ (a scan versus a tail read). Once the engine
	// has a total, it is what is actually being played, so it must win;
	// otherwise the progress bar shows a denominator that disagrees with the
	// audio.
	preflight := []time.Duration{42 * time.Second}

	snap := player.Snapshot{QueueIndex: 0, Duration: 3 * time.Second}
	if got := displayDuration(snap, preflight, core.DurationProbe); got != 3*time.Second {
		t.Fatalf("displayDuration = %v, want the engine's 3s", got)
	}

	// Before the engine probe lands, the preflight is the only number there is.
	snap.Duration = 0
	if got := displayDuration(snap, preflight, core.DurationProbe); got != 42*time.Second {
		t.Fatalf("displayDuration with no engine total = %v, want the preflight 42s", got)
	}

	// -probe=unknown asks for no total at all; the preflight is empty in that
	// mode, and the engine total is still shown if it arrives.
	snap.Duration = 0
	if got := displayDuration(snap, nil, core.DurationUnknown); got != 0 {
		t.Fatalf("displayDuration = %v, want 0 for unknown mode", got)
	}
}

func TestRunStdinIsNotReadWhenNotInteractive(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	// A reader that fails the test if anything reads from it.
	p := newFakePlayer()
	p.autoEnd = true

	e, _, _ := testEnv(t, p)
	e.stdin = failReader{t: t}
	e.interactive = false

	if got := run([]string{track}, e); got != exitOK {
		t.Fatalf("run = %d, want %d", got, exitOK)
	}
}

// TestRunDoesNotPreflightWhenNotInteractive covers the cost side of the
// non-TTY contract: a run that paints no progress line must not read a tail of
// every file to feed a display nobody sees.
func TestRunDoesNotPreflightWhenNotInteractive(t *testing.T) {
	dir := t.TempDir()
	track := writeTrack(t, dir, "a.opus")

	p := newFakePlayer()
	p.autoEnd = true

	probeCalled := false
	e, _, _ := testEnv(t, p)
	e.probe = func(string, core.DurationMode) (time.Duration, error) {
		probeCalled = true

		return time.Second, nil
	}

	if got := run([]string{"-probe", "scan", track}, e); got != exitOK {
		t.Fatalf("run = %d, want %d", got, exitOK)
	}
	if probeCalled {
		t.Fatal("a non-interactive run preflighted durations")
	}
}

// failReader fails the test if a non-interactive run reads stdin.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read in non-interactive mode")

	return 0, errors.New("unexpected read")
}
