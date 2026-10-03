package session

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/playback"
)

// swapBackendName is a second registered backend, so the session's backend
// preference tests can name something other than the default. It is registered
// once for this test binary; the factory is never used because every test wires
// the newDevice seam.
const swapBackendName = "swap-test"

func init() {
	playback.Register(swapBackendName, func() playback.Device { return &recordingDevice{} })
}

// swapCodecs accepts the fake names these tests drive, including the one the
// failure test makes the opener reject at engine time.
func swapCodecs(name string) error {
	switch name {
	case "", "codec-a", "codec-b", "codec-bad":
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownDecoder, name)
	}
}

// openerCount reports how many decoder opens the opener log recorded.
func openerCount(l *openerLog) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.names)
}

// last returns the most recently built device, which the backend-swap tests
// assert was installed.
func (f *recorderFactory) last() playback.Device {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.made) == 0 {
		return nil
	}

	return f.made[len(f.made)-1]
}

// lastBackendName reports the backend name the last build was asked for.
func (f *recorderFactory) lastBackendName() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lastBackend
}

// isClosed reports whether Close was called on this pump device.
func (d *pumpDevice) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.closed
}

// wasStarted reports whether Start ever ran.
func (d *pumpDevice) wasStarted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.started
}

// within asserts got is inside want +/- tol.
func within(t *testing.T, got, want, tol time.Duration, what string) {
	t.Helper()

	delta := got - want
	if delta < 0 {
		delta = -delta
	}
	if delta > tol {
		t.Fatalf("%s = %v, want %v +/- %v", what, got, want, tol)
	}
}

// toneSession wires a session over a synthetic tone decoder and a real pump
// device, with an optional opener override.
func toneSession(t *testing.T, opts func(*Config)) (*Session, *orderLog) {
	t.Helper()

	devLog := &orderLog{}
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40, log: devLog}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(devLog) }}).new
	if opts != nil {
		opts(&cfg)
	}

	return newSession(t, cfg), devLog
}

// TestSwapDecoderWhilePlayingReopensAndKeepsThePosition is the load-bearing
// test: a live decoder change actually opens the new implementation, labels it,
// preserves the position and keeps audio flowing.
func TestSwapDecoderWhilePlayingReopensAndKeepsThePosition(t *testing.T) {
	log := newOpenerLog()
	devLog := &orderLog{}
	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = log.openers()
	cfg.validateDecoder = swapCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(devLog) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the first decoder to be built", func() bool { return log.at(0) != "" })
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	before := s.Snapshot().Position
	if err := s.SwapDecoder("codec-b"); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	ev := waitEvent[Swapped](t, s, 2*time.Second)
	if ev.Kind != "decoder" || ev.Name != "codec-b" {
		t.Fatalf("Swapped = %+v, want kind decoder name codec-b", ev)
	}

	eventually(t, 2*time.Second, "the new decoder labels", func() bool {
		return s.Snapshot().Decoder == "codec-b"
	})
	eventually(t, 2*time.Second, "the reopen to be recorded", func() bool { return log.at(1) == "codec-b" })

	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a swap = %s, want Playing", stateName(got))
	}

	after := s.Snapshot().Position
	if after == 0 {
		t.Fatal("position regressed to 0 after the swap")
	}
	within(t, after, before, 500*time.Millisecond, "position after the swap")

	// Audio must keep flowing on the new decoder.
	eventually(t, 2*time.Second, "audio to keep moving", func() bool { return s.Snapshot().Position > after })
}

// TestSwapDecoderWhilePausedStaysPaused pins that a live swap does not start
// audio the user asked to keep paused, and still preserves the position.
func TestSwapDecoderWhilePausedStaysPaused(t *testing.T) {
	log := newOpenerLog()
	devLog := &orderLog{}
	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = log.openers()
	cfg.validateDecoder = swapCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(devLog) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, s, StatePaused, 2*time.Second)

	before := s.Snapshot().Position
	devLog.reset()
	if err := s.SwapDecoder("codec-b"); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	waitEvent[Swapped](t, s, 2*time.Second)

	if got := s.Snapshot().State; got != StatePaused {
		t.Fatalf("state after a paused swap = %s, want Paused", stateName(got))
	}
	// A paused device does not consume, so the swap target is exactly the frozen
	// position: an exact match pins "target is the current position, no jump".
	if got := s.Snapshot().Position; got != before {
		t.Fatalf("position after a paused swap = %v, want the exact pre-swap %v", got, before)
	}
	for _, op := range devLog.snapshot() {
		if op == "resume" {
			t.Fatalf("a paused swap resumed the device: %v", devLog.snapshot())
		}
	}
}

// TestSwapDecoderFailureKeepsTheOldDecoder proves a failed reopen restores the
// previous implementation at the same position and reports Failed, not Swapped.
func TestSwapDecoderFailureKeepsTheOldDecoder(t *testing.T) {
	boom := errors.New("swap opener boom")
	log := newOpenerLog()
	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = func(name, path string) (decode.Decoder, error) {
		if name == "codec-bad" {
			log.add(name)

			return nil, boom
		}

		return log.openers()(name, path)
	}
	cfg.validateDecoder = swapCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	before := s.Snapshot().Position
	if err := s.SwapDecoder("codec-bad"); err != nil {
		t.Fatalf("SwapDecoder returned a validation error for an engine failure: %v", err)
	}
	ev := waitEvent[Failed](t, s, 2*time.Second)
	if !errors.Is(ev.Err, boom) {
		t.Fatalf("Failed.Err = %v, want the opener error", ev.Err)
	}
	assertNoEvent[Swapped](t, s, 150*time.Millisecond)

	// The old decoder must be back: labels unchanged, position not regressed,
	// and audio still flowing.
	if got := s.Snapshot().Decoder; got != "codec-a" {
		t.Fatalf("Snapshot.Decoder = %q, want the restored codec-a", got)
	}
	within(t, s.Snapshot().Position, before, 500*time.Millisecond, "position after a failed swap")
	after := s.Snapshot().Position
	eventually(t, 2*time.Second, "audio to keep moving", func() bool { return s.Snapshot().Position > after })
}

// TestSwapDecoderRejectsABadNameSynchronously proves an invalid name never
// reaches the engine and changes nothing.
func TestSwapDecoderRejectsABadNameSynchronously(t *testing.T) {
	log := newOpenerLog()
	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = log.openers()
	cfg.validateDecoder = swapCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.SwapDecoder("nope"); !errors.Is(err, ErrUnknownDecoder) {
		t.Fatalf("SwapDecoder(nope) = %v, want ErrUnknownDecoder", err)
	}
	if got := s.Settings().Decoder; got != "codec-a" {
		t.Fatalf("Settings().Decoder = %q, want the unchanged codec-a", got)
	}

	// The same on a live track: no reopen, no state change.
	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the track to be built", func() bool { return log.at(0) != "" })
	opened := openerCount(log)

	if err := s.SwapDecoder("nope"); !errors.Is(err, ErrUnknownDecoder) {
		t.Fatalf("SwapDecoder(nope) on a live track = %v, want ErrUnknownDecoder", err)
	}
	if got := s.Settings().Decoder; got != "codec-a" {
		t.Fatalf("rejected swap changed the preference: %q", got)
	}
	if got := openerCount(log); got != opened {
		t.Fatalf("a rejected swap reopened the decoder: %d opens, want %d", got, opened)
	}
	assertNoEvent[Swapped](t, s, 100*time.Millisecond)
}

// TestSwapDecoderWithNoTrackSetsTheNextPreference pins the no-live-track path:
// the swap succeeds and the next track opens with the new decoder.
func TestSwapDecoderWithNoTrackSetsTheNextPreference(t *testing.T) {
	log := newOpenerLog()
	cfg := testConfig()
	cfg.openDecoder = log.openers()
	cfg.validateDecoder = swapCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.SwapDecoder("codec-b"); err != nil {
		t.Fatalf("SwapDecoder with no track: %v", err)
	}
	// The preference update runs on the control loop, so it is observable
	// shortly after the command returns rather than before.
	eventually(t, time.Second, "the preference to update", func() bool {
		return s.Settings().Decoder == "codec-b"
	})

	if err := s.Play("one"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the next track to use the preference", func() bool { return log.at(0) == "codec-b" })
	if got := s.Snapshot().Decoder; got != "codec-b" {
		t.Fatalf("Snapshot.Decoder = %q, want codec-b", got)
	}
}

// gatedNamedDecoder is a gated decoder that also describes itself, so the burst
// test can assert the snapshot reflects the decoder the swap landed on.
type gatedNamedDecoder struct {
	gatedDecoder
	codec string
}

func (d *gatedNamedDecoder) DecoderName() string { return d.codec }
func (d *gatedNamedDecoder) ParserName() string  { return d.codec + "-parser" }

// TestSwapDecoderCollapsesABurst proves the swap shares the seek worker's
// latest-wins slot: a burst while one swap is parked costs a small constant
// number of reopens, not one per call.
func TestSwapDecoderCollapsesABurst(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	var mu sync.Mutex
	opens := 0
	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = func(name, path string) (decode.Decoder, error) {
		if name != "codec-b" {
			return &toneDecoder{value: 0.5, total: 1 << 40}, nil
		}
		mu.Lock()
		opens++
		mu.Unlock()

		return &gatedNamedDecoder{
			gatedDecoder: gatedDecoder{toneDecoder: toneDecoder{value: 0.25, total: 1 << 40}, gate: gate},
			codec:        "codec-b",
		}, nil
	}
	cfg.validateDecoder = swapCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	if err := s.SwapDecoder("codec-b"); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first swap never started")
	}

	const n = 12
	for i := 0; i < n; i++ {
		if err := s.SwapDecoder("codec-b"); err != nil {
			t.Fatalf("SwapDecoder %d: %v", i, err)
		}
	}
	// Let the control loop drain the burst; it is free while the swap parks.
	time.Sleep(100 * time.Millisecond)

	gate.open()
	waitEvent[Swapped](t, s, 3*time.Second)

	mu.Lock()
	got := opens
	mu.Unlock()

	if got > 3 {
		t.Fatalf("%d swapped calls cost %d reopens; latest-wins did not collapse the burst", n+1, got)
	}
	eventually(t, 2*time.Second, "the new labels", func() bool { return s.Snapshot().Decoder == "codec-b" })
}

// TestCloseDuringSwapReturns proves a session closed while a swap is parked
// tears down cleanly without leaking a goroutine.
func TestCloseDuringSwapReturns(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = func(name, path string) (decode.Decoder, error) {
		if name == "codec-b" {
			return &gatedDecoder{toneDecoder: toneDecoder{value: 0.25, total: 1 << 40}, gate: gate}, nil
		}

		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.validateDecoder = swapCodecs
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

	if err := s.SwapDecoder("codec-b"); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the swap never started")
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
			t.Fatalf("Close during a swap: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}

	eventually(t, 3*time.Second, "goroutines to settle", func() bool {
		runtime.GC()

		return runtime.NumGoroutine() <= before
	})
}

// TestCloseDuringBackendSwapClosesTheNewDevice proves a device a backend swap
// opened but never installed is released on shutdown, so Close cannot leak an
// audio server handle.
func TestCloseDuringBackendSwapClosesTheNewDevice(t *testing.T) {
	gate := make(chan struct{})
	opened := make(chan playback.Device, 1)
	var openedOnce sync.Once

	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = func(string) (playback.Device, error) {
		select {
		case <-gate:
			dev := newPumpDevice(nil)
			openedOnce.Do(func() { opened <- dev })

			return dev, nil
		default:
		}

		return newPumpDevice(nil), nil
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	// Park the replacement build, then close: the new device exists but the
	// control loop is being torn down, so the worker must release it.
	close(gate)
	if err := s.SwapBackend(swapBackendName); err != nil {
		t.Fatalf("SwapBackend: %v", err)
	}
	fresh := <-opened

	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}

	if !fresh.(*pumpDevice).isClosed() {
		t.Fatal("a device opened by a backend swap was leaked by Close")
	}
}

// TestSwapBackendWhilePlayingRebuildsTheDevice proves a live backend change
// builds a fresh device against the same provider, closes the old one and
// keeps the position.
func TestSwapBackendWhilePlayingRebuildsTheDevice(t *testing.T) {
	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	before := s.Snapshot().Position
	if err := s.SwapBackend(swapBackendName); err != nil {
		t.Fatalf("SwapBackend: %v", err)
	}
	ev := waitEvent[Swapped](t, s, 2*time.Second)
	if ev.Kind != "backend" || ev.Name != swapBackendName {
		t.Fatalf("Swapped = %+v, want kind backend name %s", ev, swapBackendName)
	}

	if got := factory.count(); got != 2 {
		t.Fatalf("a backend swap built %d devices, want 2", got)
	}
	old := factory.first().(*pumpDevice)
	if !old.isClosed() {
		t.Fatal("the old device was not closed after the new one opened")
	}
	fresh := factory.last().(*pumpDevice)
	if fresh.isClosed() {
		t.Fatal("the new device is closed")
	}
	if !fresh.wasStarted() {
		t.Fatal("the new device was not started while the session stayed Playing")
	}
	if fresh.provider != s.provider {
		t.Fatal("the new device was not opened against the session's stable provider")
	}

	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a backend swap = %s, want Playing", stateName(got))
	}
	if got := s.Snapshot().Backend; got != swapBackendName {
		t.Fatalf("Snapshot.Backend = %q, want %s", got, swapBackendName)
	}
	within(t, s.Snapshot().Position, before, 500*time.Millisecond, "position after a backend swap")
	after := s.Snapshot().Position
	eventually(t, 2*time.Second, "audio to keep moving", func() bool { return s.Snapshot().Position > after })
}

// TestSwapBackendWhilePausedStaysPaused proves the replacement device is
// installed unstarted when the session is paused, so it does not begin audio on
// its own.
func TestSwapBackendWhilePausedStaysPaused(t *testing.T) {
	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, s, StatePaused, 2*time.Second)

	if err := s.SwapBackend(swapBackendName); err != nil {
		t.Fatalf("SwapBackend: %v", err)
	}
	waitEvent[Swapped](t, s, 2*time.Second)

	if got := factory.count(); got != 2 {
		t.Fatalf("a paused backend swap built %d devices, want 2", got)
	}
	fresh := factory.last().(*pumpDevice)
	if fresh.wasStarted() {
		t.Fatal("a paused backend swap started the replacement device")
	}
	if got := s.Snapshot().State; got != StatePaused {
		t.Fatalf("state after a paused backend swap = %s, want Paused", stateName(got))
	}
}

// TestPausedBackendSwapStartsOnResume pins the bookkeeping around a paused
// swap: the replacement device is installed unstarted, and a later Resume must
// start it rather than ask it to continue.
func TestPausedBackendSwapStartsOnResume(t *testing.T) {
	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, s, StatePaused, 2*time.Second)

	if err := s.SwapBackend(swapBackendName); err != nil {
		t.Fatalf("SwapBackend: %v", err)
	}
	waitEvent[Swapped](t, s, 2*time.Second)

	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitState(t, s, StatePlaying, 2*time.Second)

	fresh := factory.last().(*pumpDevice)
	if !fresh.wasStarted() {
		t.Fatal("Resume after a paused backend swap did not start the replacement device")
	}
	after := s.Snapshot().Position
	eventually(t, 2*time.Second, "audio to move on the new backend", func() bool { return s.Snapshot().Position > after })
}

// TestSwapBackendFailureKeepsTheOldDevice proves an Open failure leaves the
// playing device alone and reports Failed without a Swapped.
func TestSwapBackendFailureKeepsTheOldDevice(t *testing.T) {
	boom := errors.New("no such backend")
	calls := 0
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = func(string) (playback.Device, error) {
		calls++
		if calls == 1 {
			return newPumpDevice(nil), nil
		}

		return nil, boom
	}
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	before := s.Snapshot().Position
	if err := s.SwapBackend("oto"); err != nil {
		t.Fatalf("SwapBackend: %v", err)
	}
	ev := waitEvent[Failed](t, s, 2*time.Second)
	if !errors.Is(ev.Err, boom) {
		t.Fatalf("Failed.Err = %v, want the newDevice error", ev.Err)
	}
	assertNoEvent[Swapped](t, s, 150*time.Millisecond)

	// The old device is retained and audio keeps flowing on it.
	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a failed backend swap = %s, want Playing", stateName(got))
	}
	within(t, s.Snapshot().Position, before, 500*time.Millisecond, "position after a failed backend swap")
	after := s.Snapshot().Position
	eventually(t, 2*time.Second, "audio to keep moving", func() bool { return s.Snapshot().Position > after })
}

// TestSwapBackendWithNoDeviceSetsTheNextPreference pins the no-device path: the
// swap succeeds and the next device build uses the new backend.
func TestSwapBackendWithNoDeviceSetsTheNextPreference(t *testing.T) {
	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	if err := s.SwapBackend(swapBackendName); err != nil {
		t.Fatalf("SwapBackend with no device: %v", err)
	}
	// The preference update runs on the control loop, so it is observable
	// shortly after the command returns rather than before.
	eventually(t, time.Second, "the preference to update", func() bool {
		return s.Settings().Backend == swapBackendName
	})

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	if got := factory.lastBackendName(); got != swapBackendName {
		t.Fatalf("the next device build used backend %q, want %s", got, swapBackendName)
	}
	if got := s.Snapshot().Backend; got != swapBackendName {
		t.Fatalf("Snapshot.Backend = %q, want %s", got, swapBackendName)
	}
}

// TestSwapBackendRejectsAnUnknownNameSynchronously proves a bad backend is
// refused before it reaches the engine.
func TestSwapBackendRejectsAnUnknownNameSynchronously(t *testing.T) {
	factory := &recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}
	cfg := testConfig()
	cfg.newDevice = factory.new
	s := newSession(t, cfg)

	if err := s.SwapBackend("not-a-backend"); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("SwapBackend = %v, want ErrUnknownBackend", err)
	}
	if got := factory.count(); got != 0 {
		t.Fatalf("a rejected backend swap built %d devices", got)
	}
	if got := s.Settings().Backend; got != cfg.Backend {
		t.Fatalf("Settings().Backend = %q, want the unchanged %q", got, cfg.Backend)
	}
	assertNoEvent[Swapped](t, s, 100*time.Millisecond)
}
