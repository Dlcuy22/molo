package session

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/playback"
)

// seekGate is the latch a gated decoder parks a reposition on. A test uses it
// to hold a seek in flight while it observes what the control loop does around
// it, and to count how many repositioning calls a burst of Seeks cost.
type seekGate struct {
	entered chan struct{}
	release chan struct{}

	arrive sync.Once
	free   sync.Once

	mu    sync.Mutex
	calls int
	last  int64
}

func newSeekGate() *seekGate {
	return &seekGate{entered: make(chan struct{}), release: make(chan struct{})}
}

// open releases every parked reposition. It is idempotent so both a defer and
// an explicit release can call it.
func (g *seekGate) open() { g.free.Do(func() { close(g.release) }) }

func (g *seekGate) enter(frame int64) {
	g.arrive.Do(func() { close(g.entered) })
	g.mu.Lock()
	g.calls++
	g.last = frame
	g.mu.Unlock()
}

func (g *seekGate) wait() { <-g.release }

func (g *seekGate) counts() (int, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.calls, g.last
}

// gatedDecoder is a toneDecoder whose SeekFrame parks until the gate opens, so
// a test can hold a reposition in flight for as long as it needs.
type gatedDecoder struct {
	toneDecoder
	gate *seekGate
}

func (d *gatedDecoder) SeekFrame(frame int64) error {
	d.gate.enter(frame)
	d.gate.wait()

	d.mu.Lock()
	d.pos = frame
	err := d.seekErr
	d.mu.Unlock()

	return err
}

// costDecoder simulates the real pure-Go Opus seek: its SeekFrame blocks for a
// duration proportional to the target (costPerSecond of work per second of
// audio). It is what makes the latency measurement representative.
type costDecoder struct {
	toneDecoder
	gate          *seekGate
	costPerSecond time.Duration
}

func (d *costDecoder) SeekFrame(frame int64) error {
	d.gate.enter(frame)

	dur := time.Duration(frame) * time.Second / time.Duration(canonicalFormat.Rate)
	time.Sleep(time.Duration(int64(dur) * int64(d.costPerSecond) / int64(time.Second)))

	d.mu.Lock()
	d.pos = frame
	err := d.seekErr
	d.mu.Unlock()

	return err
}

// assertNoEvent fails if an event of type T arrives within the window. It is
// how the stale and interference tests prove nothing misleading was reported.
func assertNoEvent[T Event](t *testing.T, s *Session, within time.Duration) {
	t.Helper()

	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return
			}
			if e, is := ev.(T); is {
				t.Fatalf("unexpected %T: %+v", e, e)
			}
		case <-deadline:
			return
		}
	}
}

// TestSeekDoesNotBlockTheControlLoop is the load-bearing test for the bug: a
// reposition is parked, so a Pause issued meanwhile must be applied at once,
// and the seek completing later must not resume the device the user paused.
func TestSeekDoesNotBlockTheControlLoop(t *testing.T) {
	gate := newSeekGate()
	log := &orderLog{}
	defer gate.open()

	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &gatedDecoder{toneDecoder: toneDecoder{value: 0.5, total: 1 << 40}, gate: gate}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(log) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	if err := s.Seek(2 * time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the reposition never started")
	}

	// The seek is parked here. Pause must still be applied promptly.
	start := time.Now()
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, s, StatePaused, 500*time.Millisecond)
	if lat := time.Since(start); lat > 250*time.Millisecond {
		t.Fatalf("Pause took %v while a seek was in flight; the control loop was blocked", lat)
	}

	// Release the seek. It completes against the same track, but the user has
	// since paused, so the device must stay paused.
	gate.open()
	if ev := waitEvent[Seeked](t, s, 2*time.Second); ev.Position != 2*time.Second {
		t.Fatalf("Seeked.Position = %v, want 2s", ev.Position)
	}

	for _, op := range log.snapshot() {
		if op == "resume" {
			t.Fatalf("the completing seek resumed a session the user paused: %v", log.snapshot())
		}
	}
	if got := s.Snapshot().State; got != StatePaused {
		t.Fatalf("state after the seek completed = %s, want Paused", stateName(got))
	}
}

// TestSeekCollapsesABurstToTheNewestTarget proves latest-wins: a burst of seeks
// while the first is parked costs a small constant number of repositions, not
// one per call, and the newest target is the one applied last.
func TestSeekCollapsesABurstToTheNewestTarget(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &gatedDecoder{toneDecoder: toneDecoder{value: 0.5, total: 1 << 40}, gate: gate}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	const n = 12
	if err := s.Seek(time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first reposition never started")
	}

	for i := 2; i <= n; i++ {
		if err := s.Seek(time.Duration(i) * time.Second); err != nil {
			t.Fatalf("Seek %d: %v", i, err)
		}
	}
	// Let the control loop drain the burst; it is free while the seek parks.
	time.Sleep(100 * time.Millisecond)

	gate.open()
	want := time.Duration(n) * time.Second
	eventually(t, 2*time.Second, "the newest target to be applied", func() bool {
		_, last := gate.counts()

		return last == framesFor(want)
	})

	calls, last := gate.counts()
	if calls > n/3 {
		t.Fatalf("%d seeks produced %d SeekFrame calls; latest-wins did not collapse the burst", n, calls)
	}
	if last != framesFor(want) {
		t.Fatalf("last target = %d frames, want %d", last, framesFor(want))
	}

	// The newest target must be the one the session reports as landed.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if e, ok := ev.(Seeked); ok && e.Position == want {
				return
			}
		case <-deadline:
			t.Fatalf("no Seeked event for the newest target %v", want)
		}
	}
}

// TestSeekedReportsTheRepositionCost checks the payload a UI consumes: the
// target, the pre-seek origin, and a measured reposition cost.
func TestSeekedReportsTheRepositionCost(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &gatedDecoder{toneDecoder: toneDecoder{value: 0.5, total: 1 << 40}, gate: gate}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	if err := s.Seek(500 * time.Millisecond); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the reposition never started")
	}
	before := s.Snapshot().Position

	const hold = 60 * time.Millisecond
	time.Sleep(hold)
	gate.open()

	ev := waitEvent[Seeked](t, s, 2*time.Second)
	if ev.Position != 500*time.Millisecond {
		t.Fatalf("Seeked.Position = %v, want 500ms", ev.Position)
	}
	if ev.From != before {
		t.Fatalf("Seeked.From = %v, want the pre-seek position %v", ev.From, before)
	}
	if ev.Elapsed < hold/2 {
		t.Fatalf("Seeked.Elapsed = %v, want the measured reposition cost around %v", ev.Elapsed, hold)
	}
}

// TestStopDuringSeekDoesNotResume proves interference is honoured: after a Stop
// the abandoned seek must not resume the device or report anything.
func TestStopDuringSeekDoesNotResume(t *testing.T) {
	gate := newSeekGate()
	log := &orderLog{}
	defer gate.open()

	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &gatedDecoder{toneDecoder: toneDecoder{value: 0.5, total: 1 << 40}, gate: gate}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(log) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	if err := s.Seek(2 * time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the reposition never started")
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop retires the provider before it waits the streamer out, so a nil
	// provider is proof the Stop command reached the controller while the seek
	// was in flight.
	eventually(t, 2*time.Second, "the streamer to be retired", func() bool {
		return s.provider.current() == nil
	})
	gate.open()

	waitState(t, s, StateStopped, 2*time.Second)
	for _, op := range log.snapshot() {
		if op == "resume" {
			t.Fatalf("the abandoned seek resumed the device after Stop: %v", log.snapshot())
		}
	}
	assertNoEvent[Seeked](t, s, 150*time.Millisecond)
}

// TestStaleSeekAfterTrackChangeIsDiscarded proves a seek that lands after the
// queue moved on cannot touch the new track and reports nothing.
func TestStaleSeekAfterTrackChangeIsDiscarded(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	cfg := testConfig()
	cfg.openDecoder = func(path string) (decode.Decoder, error) {
		if path == "a" {
			return &gatedDecoder{toneDecoder: toneDecoder{value: 0.5, total: 1 << 40}, gate: gate}, nil
		}

		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"a", "b"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	if err := s.Seek(time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the reposition never started")
	}

	// Change track while the old seek parks. The controller retires the old
	// streamer, which cannot finish closing until the seek is released.
	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 2*time.Second, "the old streamer to be retired", func() bool {
		return s.provider.current() == nil
	})
	gate.open()

	eventually(t, 3*time.Second, "the second track to play", func() bool {
		snap := s.Snapshot()

		return snap.QueueIndex == 1 && snap.State == StatePlaying
	})

	// The new track must not have been dragged to the stale target.
	if pos := s.Snapshot().Position; pos >= 900*time.Millisecond {
		t.Fatalf("the stale seek moved the new track to %v", pos)
	}

	assertNoEvent[Seeked](t, s, 150*time.Millisecond)
}

// TestCloseDuringSeekReturns proves a session closed while a seek is parked
// tears down cleanly: no panic, no leaked goroutine, and Close returns.
func TestCloseDuringSeekReturns(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &gatedDecoder{toneDecoder: toneDecoder{value: 0.5, total: 1 << 40}, gate: gate}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	before := runtime.NumGoroutine()

	if err := s.Seek(2 * time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the reposition never started")
	}

	done := make(chan error, 1)
	go func() { done <- s.Close() }()

	select {
	case <-s.closing:
	case <-time.After(2 * time.Second):
		t.Fatal("Close never began tearing down")
	}

	gate.open()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close during a seek: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// The seek worker must be gone: it is registered on workerWG, which
	// shutdown waits out before Close returns.
	eventually(t, 3*time.Second, "goroutines to settle", func() bool {
		runtime.GC()

		return runtime.NumGoroutine() <= before
	})
}

// TestSeekCommandLatencyStaysLow measures the control loop's command latency
// while a seek whose cost is proportional to its target is in flight. The
// number it logs is the before/after measurement for the async seek.
func TestSeekCommandLatencyStaysLow(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	cfg := testConfig()
	cfg.openDecoder = func(string) (decode.Decoder, error) {
		return &costDecoder{
			toneDecoder:   toneDecoder{value: 0.5, total: 1 << 40},
			gate:          gate,
			costPerSecond: 6600 * time.Microsecond,
		}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	const target = 20 * time.Second
	seekStarted := time.Now()
	if err := s.Seek(target); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the reposition never started")
	}

	start := time.Now()
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, s, StatePaused, 3*time.Second)
	latency := time.Since(start)
	t.Logf("seek target %v simulated cost %v; Pause latency while in flight %v", target, time.Since(seekStarted), latency)
	if latency > 50*time.Millisecond {
		t.Fatalf("Pause latency %v while a %v seek was in flight", latency, target)
	}
}
