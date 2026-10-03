package molo_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/playback"
)

// facadeDevice is a passive backend used to drive the facade without an audio
// server. It proves the facade reaches a real session through the registry.
type facadeDevice struct {
	mu      sync.Mutex
	started int
	paused  int
}

func (d *facadeDevice) Open(core.FrameFormat, playback.Provider) error { return nil }

func (d *facadeDevice) Start() error {
	d.mu.Lock()
	d.started++
	d.mu.Unlock()

	return nil
}

func (d *facadeDevice) Pause() error {
	d.mu.Lock()
	d.paused++
	d.mu.Unlock()

	return nil
}

func (d *facadeDevice) Resume() error          { return nil }
func (d *facadeDevice) Flush() error           { return nil }
func (d *facadeDevice) Close() error           { return nil }
func (d *facadeDevice) Latency() time.Duration { return 0 }
func (d *facadeDevice) Err() error             { return nil }

func init() {
	playback.Register("facade-test", func() playback.Device { return &facadeDevice{} })
}

func fixture(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("decode", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s is missing: %v", name, err)
	}

	return path
}

func waitState(t *testing.T, p molo.Player, want molo.State, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.Snapshot().State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for state %d", want)
}

func TestNewReturnsAPlayer(t *testing.T) {
	p, err := molo.New(molo.WithBackend("facade-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := p.Snapshot().State; got != molo.Idle {
		t.Fatalf("initial state = %d, want Idle", got)
	}
}

// TestHandWrittenOption proves a consumer outside the module can author an
// Option as a plain closure over the exported Config, with no access to the
// internal session type.
func TestHandWrittenOption(t *testing.T) {
	p, err := molo.New(
		molo.WithBackend("facade-test"),
		func(c *molo.Config) { c.Decoder = "opus-pion" },
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := p.Settings().Decoder; got != "opus-pion" {
		t.Fatalf("Settings().Decoder = %q, want opus-pion", got)
	}
}

func TestFacadePlaysATrack(t *testing.T) {
	p, err := molo.New(molo.WithBackend("facade-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	path := fixture(t, "short_stereo.opus")
	if err := p.Play(path); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, p, molo.Playing, 3*time.Second)

	if got := p.Snapshot().Path; got != path {
		t.Fatalf("Path = %q, want %q", got, path)
	}

	// The facade must hand the UI the session's concrete event types so a
	// type switch works without importing internal/session.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-p.Events():
			switch ev.(type) {
			case molo.TrackChanged:
				return
			case molo.StateChanged, molo.TrackEnded, molo.Seeked, molo.Failed:
			default:
				t.Fatalf("facade emitted an unknown event type %T", ev)
			}
		case <-deadline:
			t.Fatal("timed out waiting for a TrackChanged through the facade")
		}
	}
}

func TestFacadeRejectsAnEmptyQueue(t *testing.T) {
	p, err := molo.New(molo.WithBackend("facade-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if err := p.PlayQueue(nil); err == nil {
		t.Fatal("PlayQueue(nil) returned nil, want a validation error")
	}
	if err := p.Play(""); err == nil {
		t.Fatal("Play(\"\") returned nil, want a validation error")
	}
	if err := p.Seek(-time.Second); err == nil {
		t.Fatal("Seek(negative) returned nil, want a validation error")
	}
}

// TestFacadeReExportsCommandErrors proves the sentinels a synchronous command
// returns or wraps are the same values the session owns, so a consumer outside
// the module can match them with errors.Is.
func TestFacadeReExportsCommandErrors(t *testing.T) {
	p, err := molo.New(molo.WithBackend("facade-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if err := p.PlayQueue(nil); !errors.Is(err, molo.ErrEmptyQueue) {
		t.Fatalf("PlayQueue(nil) = %v, want ErrEmptyQueue", err)
	}

	bad := p.Settings()
	bad.Decoder = "not-a-codec"
	if err := p.ApplySettings(bad); !errors.Is(err, molo.ErrUnknownDecoder) {
		t.Fatalf("ApplySettings(bad decoder) = %v, want ErrUnknownDecoder", err)
	}

	bad = p.Settings()
	bad.Backend = "not-a-backend"
	if err := p.ApplySettings(bad); !errors.Is(err, molo.ErrUnknownBackend) {
		t.Fatalf("ApplySettings(bad backend) = %v, want ErrUnknownBackend", err)
	}

	bad = p.Settings()
	bad.Volume = 2.5
	if err := p.ApplySettings(bad); !errors.Is(err, molo.ErrBadVolume) {
		t.Fatalf("ApplySettings(bad volume) = %v, want ErrBadVolume", err)
	}

	bad = p.Settings()
	bad.ProbeMode = core.DurationMode(99)
	if err := p.ApplySettings(bad); !errors.Is(err, molo.ErrBadProbeMode) {
		t.Fatalf("ApplySettings(bad probe mode) = %v, want ErrBadProbeMode", err)
	}
}

func TestFacadeNonBlockingPlay(t *testing.T) {
	p, err := molo.New(molo.WithBackend("facade-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	done := make(chan error, 1)
	go func() { done <- p.Play(fixture(t, "short_stereo.opus")) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Play: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Play blocked")
	}
}

func TestFacadeVolumeAndQueue(t *testing.T) {
	p, err := molo.New(molo.WithBackend("facade-test"), molo.WithVolume(0.25))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := p.Snapshot().Volume; got != 0.25 {
		t.Fatalf("initial Volume = %v, want 0.25", got)
	}
	p.SetVolume(0.75)
	if got := p.Snapshot().Volume; got != 0.75 {
		t.Fatalf("Volume after SetVolume = %v, want 0.75", got)
	}

	if err := p.PlayQueue([]string{"a", "b"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	// Queue reflects the command without waiting for the session to run it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if q := p.Queue(); len(q) == 2 && q[0] == "a" && q[1] == "b" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Queue() never reflected PlayQueue: %q", p.Queue())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestFacadeExposesTheTapFeed proves the facade re-exports the visualizer feed
// and that Read is non-blocking on an idle molo. Post-gain sample values are
// asserted where the pipeline actually runs (internal/session); a passive
// backend here cannot push audio through the tap, so this test deliberately
// stops at the contract.
func TestFacadeExposesTheTapFeed(t *testing.T) {
	p, err := molo.New(molo.WithBackend("facade-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	tap := p.Tap()
	if tap == nil {
		t.Fatal("Tap() returned nil")
	}

	// An empty tap must answer immediately rather than park.
	done := make(chan int, 1)
	go func() { done <- tap.Read(make([]float32, 128)) }()
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("Read on an idle tap = %d frames, want 0", n)
		}
	case <-time.After(time.Second):
		t.Fatal("Read blocked on an idle tap")
	}

	// With no consumer yet the tap is inactive; Play with a passive device must
	// still not stall. Read() is what activates it.
	if err := p.Play(fixture(t, "short_stereo.opus")); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, p, molo.Playing, 3*time.Second)
}
