package session

import (
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/playback"
)

// waitDebugKind returns the next debug record of the given kind, discarding
// others, and fails rather than hanging when it never arrives.
func waitDebugKind(t *testing.T, s *Session, kind DebugKind, timeout time.Duration) DebugEvent {
	t.Helper()

	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.DebugEvents():
			if !ok {
				t.Fatalf("debug channel closed while waiting for %s", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s debug record", kind)
		}
	}
}

// TestDebugEventsAreConsumerGated pins the zero-cost default: a session that
// never asks for debug records records none, so ordinary playback is unaffected
// and no config knob is needed to keep it quiet.
func TestDebugEventsAreConsumerGated(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &describedToneDecoder{
			toneDecoder: toneDecoder{value: 0.5, total: 1 << 40},
			decoder:     "gate-codec",
			parser:      "gate-parser",
		}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if s.debug.active.Load() {
		t.Fatal("the debug sink is active before any consumer attached")
	}

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Nothing attached, so the sink must still be idle and hold no records.
	if s.debug.active.Load() {
		t.Fatal("the debug sink became active without a consumer")
	}
	select {
	case ev := <-s.debug.channel():
		t.Fatalf("a debug record was buffered with no consumer: %+v", ev)
	default:
	}

	// Attaching flips it on, so the same session starts recording from here.
	if s.DebugEvents() == nil {
		t.Fatal("DebugEvents returned a nil channel")
	}
	if !s.debug.active.Load() {
		t.Fatal("the debug sink did not activate on first read")
	}
}

// TestDebugQueueDropsOldestAndNeverBlocks is the bounded-memory assertion: a
// slow debug consumer loses the oldest record and the producer never stalls.
func TestDebugQueueDropsOldestAndNeverBlocks(t *testing.T) {
	sink := newDebugSink(4)
	sink.attach()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			sink.emit(DebugEvent{Kind: DebugState, Count: int64(i)})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked; the control loop would stall behind a slow debug consumer")
	}

	if got := sink.droppedCount(); got != 96 {
		t.Fatalf("dropped = %d, want 96", got)
	}

	// The four newest survive, in order.
	var kept []int64
	for len(sink.channel()) > 0 {
		kept = append(kept, (<-sink.channel()).Count)
	}
	if len(kept) != 4 || kept[0] != 96 || kept[3] != 99 {
		t.Fatalf("kept = %v, want the newest four 96..99", kept)
	}
}

// TestDebugTrackReportsResolvedCodec proves the record a debug view needs to
// answer "what codec is playing": the resolved decoder and parser labels, the
// backend, and the track path.
func TestDebugTrackReportsResolvedCodec(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &describedToneDecoder{
			toneDecoder: toneDecoder{value: 0.5, total: 1 << 40},
			decoder:     "alpha-codec",
			parser:      "alpha-parser",
		}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}

	ev := waitDebugKind(t, s, DebugTrack, 3*time.Second)
	if ev.Path != "tone" {
		t.Fatalf("Path = %q, want tone", ev.Path)
	}
	if ev.Decoder != "alpha-codec" || ev.Parser != "alpha-parser" {
		t.Fatalf("codec = %q/%q, want alpha-codec/alpha-parser", ev.Decoder, ev.Parser)
	}
	if ev.Backend != "oto" {
		t.Fatalf("Backend = %q, want oto", ev.Backend)
	}
}

// TestDebugUnderrunDeltaReported drives the streamer into short reads and
// proves the poll reports the increase, not just the cumulative total. The ring
// is deliberately smaller than the device's read, so every read comes up short
// while the stream is live.
func TestDebugUnderrunDeltaReported(t *testing.T) {
	cfg := testConfig()
	cfg.RingFrames = 1024
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	// Attach before playing so the activation and the first shortfalls are
	// recorded.
	_ = s.DebugEvents()

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	ev := waitDebugKind(t, s, DebugUnderrun, 3*time.Second)
	if ev.Delta < 1 {
		t.Fatalf("Delta = %d, want at least 1", ev.Delta)
	}
	if ev.Count < ev.Delta {
		t.Fatalf("Count = %d is less than Delta = %d; the total should be cumulative", ev.Count, ev.Delta)
	}
}

// TestDebugStateReported proves the transitions a debug view mirrors from the
// control events also land on the debug stream.
func TestDebugStateReported(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	_ = s.DebugEvents()

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitState(t, s, StateStopped, 3*time.Second)

	// Drain what remains and confirm at least one state record landed. The
	// exact sequence depends on scheduling, so only presence is asserted.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.DebugEvents():
			if ev.Kind == DebugState {
				return
			}
		case <-deadline:
			t.Fatal("no DebugState record arrived for a session that transitioned twice")
		}
	}
}

// TestDebugEventsDoNotDisplaceControlEvents is the independence assertion: a
// debug consumer must not be able to cost the control stream an event, which is
// the reason debug records live on their own queue rather than on Events.
func TestDebugEventsDoNotDisplaceControlEvents(t *testing.T) {
	cfg := testConfig()
	// A buffer large enough for the handful of control events a start and stop
	// produce, but far too small for the debug flood: if the two streams shared
	// a queue, the underrun records would evict the control events.
	cfg.EventBuffer = 8
	cfg.RingFrames = 1024
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	// A debug consumer that never reads, plus a stream that keeps producing
	// underrun records, must not change what the control stream delivers.
	_ = s.DebugEvents()

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitState(t, s, StateStopped, 3*time.Second)

	if got := s.Snapshot().DroppedEvents; got != 0 {
		t.Fatalf("DroppedEvents = %d, want 0: debug records displaced control events", got)
	}
}
