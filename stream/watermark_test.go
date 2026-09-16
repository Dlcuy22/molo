package stream

import (
	"errors"
	"io"
	"runtime"
	"testing"
	"time"
)

func TestWatermarksBoundTheProducer(t *testing.T) {
	const (
		ring  = 1024
		high  = 512
		low   = 256
		chunk = 128
	)
	dec := newScriptedDecoder(canonicalFormat, chunk, 1<<30, 0.25)
	s := newTestStreamer(t, dec.opener(), Config{
		RingFrames:  ring,
		HighWater:   high,
		LowWater:    low,
		ChunkFrames: chunk,
	})
	startStreamer(t, s)

	// With nobody consuming, the producer must stop at the high watermark
	// instead of decoding the whole stream into memory.
	eventually(t, 2*time.Second, "producer to reach steady state", func() bool {
		return s.Stats().Buffered >= high
	})
	time.Sleep(50 * time.Millisecond)

	if got := s.ring.Len(); got > high+chunk {
		t.Fatalf("ring holds %d frames, want at most high+chunk = %d", got, high+chunk)
	}

	first := s.Stats().Decoded
	time.Sleep(50 * time.Millisecond)
	if got := s.Stats().Decoded; got != first {
		t.Fatalf("producer kept decoding while full: %d then %d", first, got)
	}

	// Draining below the low mark must restart it.
	buf := make([]float32, low*canonicalFormat.Ch)
	if _, err := s.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames: %v", err)
	}
	eventually(t, 2*time.Second, "producer to refill", func() bool {
		return s.Stats().Decoded > first
	})
	eventually(t, 2*time.Second, "producer to refill past the low mark", func() bool {
		return s.ring.Len() > low
	})
}

func TestProducerDoesNotSpinWhileFull(t *testing.T) {
	// A busy-waiting producer would keep incrementing its decode counter; a
	// parked one does not.
	dec := newScriptedDecoder(canonicalFormat, 64, 1<<30, 0.25)
	s := newTestStreamer(t, dec.opener(), Config{RingFrames: 256, HighWater: 256, LowWater: 128, ChunkFrames: 64})
	startStreamer(t, s)

	eventually(t, 2*time.Second, "producer to fill", func() bool { return s.ring.Len() >= 256 })
	time.Sleep(100 * time.Millisecond)

	before := s.Stats().Decoded
	time.Sleep(100 * time.Millisecond)
	if after := s.Stats().Decoded; after != before {
		t.Fatalf("decoded counter moved from %d to %d while the ring was full", before, after)
	}
}

func TestCloseWakesProducerBlockedOnFullRing(t *testing.T) {
	// A free-running decoder fills the ring until the producer parks at the
	// high watermark. Close must release that parked writer, not wait for the
	// consumer to drain.
	dec := newScriptedDecoder(canonicalFormat, 256, 1<<30, 0.25)
	s := newTestStreamer(t, dec.opener(), Config{RingFrames: 512, HighWater: 512, LowWater: 256, ChunkFrames: 256})
	startStreamer(t, s)

	eventually(t, 2*time.Second, "producer to fill the ring", func() bool { return s.ring.Len() >= 512 })

	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()
	waitWithin(t, 2*time.Second, "Close with a full ring", done)
}

func TestInvalidWatermarksAreRejected(t *testing.T) {
	dec := newScriptedDecoder(canonicalFormat, 64, 1024, 0.25)
	for _, cfg := range []Config{
		{RingFrames: 256, HighWater: 512, LowWater: 128},
		{RingFrames: 256, HighWater: 256, LowWater: 256},
		{RingFrames: 256, HighWater: 128, LowWater: 200},
	} {
		if _, err := New(dec.opener(), cfg); err == nil {
			t.Fatalf("New accepted %+v", cfg)
		}
	}
}

func TestConcurrentProducerConsumerIsConsistent(t *testing.T) {
	want := decodeFixture(t, "stereo_2s.opus")

	s := newTestStreamer(t, pionOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, s)

	type result struct {
		pcm []float32
		err error
	}
	got := make(chan result, 1)
	go func() {
		// An odd-size buffer keeps the read/write boundaries off the ring's
		// power-of-two wrap points.
		buf := make([]float32, 777*canonicalFormat.Ch)
		var out []float32
		for {
			n, err := s.ReadFrames(buf)
			if n > 0 {
				out = append(out, buf[:n*canonicalFormat.Ch]...)
			}
			if err != nil {
				got <- result{pcm: out, err: err}

				return
			}
		}
	}()

	select {
	case res := <-got:
		if !errors.Is(res.err, io.EOF) {
			t.Fatalf("consumer stopped with %v, want io.EOF", res.err)
		}
		if !float32sEqual(res.pcm, want) {
			t.Fatalf("concurrent stream produced %d samples, want %d", len(res.pcm), len(want))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never finished")
	}
}

func TestNoGoroutineLeakAcrossStreamerLives(t *testing.T) {
	base := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		s, err := New(pionOpener(fixturePath(t, "short_stereo.opus")), Config{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		startStreamer(t, s)
		_ = drain(t, s)
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// Close blocks until the producer exits, so a real leak would have shown up
	// as a hang above. Allow the runtime a moment to reap before counting.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutines grew from %d to %d over five streamers", base, runtime.NumGoroutine())
}
