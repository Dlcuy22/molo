package session

import (
	"math"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/playback"
)

func TestTapRingOverwritesOldest(t *testing.T) {
	r := newTapRing(4)
	r.writeMono([]float32{1, 2, 3})
	r.writeMono([]float32{4, 5, 6})

	// Six frames were written into a four-frame ring, so the two oldest are
	// gone and the consumer gets the newest four in order.
	out := make([]float32, 8)
	n := r.readMono(out)
	if n != 4 {
		t.Fatalf("readMono = %d frames, want 4", n)
	}
	want := []float32{3, 4, 5, 6}
	for i, v := range want {
		if out[i] != v {
			t.Fatalf("frame %d = %v, want %v", i, out[i], v)
		}
	}
}

func TestTapRingLappedConsumerSkipsToNewest(t *testing.T) {
	r := newTapRing(4)
	r.writeMono([]float32{1, 2, 3, 4})
	// Nobody reads, and the writer wraps past the ring by three frames. The
	// newest four frames are 4,5,6,7; the detector must skip 1,2,3, which no
	// longer exist.
	r.writeMono([]float32{5, 6, 7})

	got := make([]float32, 8)
	if n := r.readMono(got); n != 4 {
		t.Fatalf("read = %d frames, want 4", n)
	}
	for i, v := range []float32{4, 5, 6, 7} {
		if got[i] != v {
			t.Fatalf("frame %d = %v, want %v", i, got[i], v)
		}
	}
	if dropped := r.droppedFrames(); dropped != 3 {
		t.Fatalf("dropped = %d, want 3 (the overwritten frames)", dropped)
	}
}

func TestTapReadDropsOldestWhenDstIsSmall(t *testing.T) {
	r := newTapRing(4)
	r.writeMono([]float32{1, 2, 3, 4})

	// A reader that asks for less than is buffered gets the newest frames and
	// skips the oldest, which is what a live visualizer wants after a gap.
	out := make([]float32, 2)
	if n := r.readMono(out); n != 2 {
		t.Fatalf("read = %d frames, want 2", n)
	}
	if out[0] != 3 || out[1] != 4 {
		t.Fatalf("read = %v, want the newest frames [3 4]", out)
	}
	if dropped := r.droppedFrames(); dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
}

func TestTapPublishesMonoDownmix(t *testing.T) {
	tap := newTap()
	tap.attach()

	// Two stereo frames: (1, -1) downmixes to 0, (0.5, 0.5) to 0.5.
	buf := []float32{1, -1, 0.5, 0.5}
	tap.publish(buf, 2, 2)

	out := make([]float32, 4)
	n := tap.Read(out)
	if n != 2 {
		t.Fatalf("Read = %d frames, want 2", n)
	}
	if out[0] != 0 {
		t.Fatalf("mono frame 0 = %v, want 0", out[0])
	}
	if math.Abs(float64(out[1]-0.5)) > 1e-6 {
		t.Fatalf("mono frame 1 = %v, want 0.5", out[1])
	}
}

func TestTapInactivePublishIsNoop(t *testing.T) {
	tap := newTap()
	// Never attached: publish must do nothing observable.
	tap.publish([]float32{1, 2, 3, 4}, 2, 2)

	if n := tap.Read(make([]float32, 4)); n != 0 {
		t.Fatalf("Read on an inactive tap = %d frames, want 0", n)
	}
}

// TestHeldTapIsInactiveUntilRead proves the zero-cost clause at the session
// level: asking for the Tap handle and never reading it must not turn the
// publisher on.
func TestHeldTapIsInactiveUntilRead(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	_ = s.Tap()
	if s.tap.active.Load() {
		t.Fatal("Tap() alone activated the feed; a held handle must cost nothing")
	}

	// The first Read is what activates it.
	s.Tap().Read(nil)
	if !s.tap.active.Load() {
		t.Fatal("Read did not activate the feed")
	}
}

func TestTapReadAndPublishDoNotAllocate(t *testing.T) {
	tap := newTap()
	tap.attach()

	stereo := make([]float32, 4096*2)
	for i := range stereo {
		stereo[i] = float32(i%13) / 13
	}
	out := make([]float32, 4096)

	publish := testing.AllocsPerRun(1000, func() {
		tap.publish(stereo, 4096, 2)
	})
	if publish != 0 {
		t.Fatalf("publish allocated %v times per call, want 0", publish)
	}

	read := testing.AllocsPerRun(1000, func() {
		tap.Read(out)
	})
	if read != 0 {
		t.Fatalf("Read allocated %v times per call, want 0", read)
	}
}

func TestTapPublishAcrossScratchWindows(t *testing.T) {
	tap := newTap()
	tap.attach()

	// More frames than the scratch window forces the batching path, which must
	// still downmix in order and never allocate.
	frames := len(tap.scratch)*2 + 7
	buf := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		buf[i*2] = float32(i)
		buf[i*2+1] = 0
	}
	tap.publish(buf, frames, 2)

	out := make([]float32, frames)
	n := tap.Read(out)
	if n != frames {
		t.Fatalf("Read = %d frames, want %d", n, frames)
	}
	for i := range out {
		if out[i] != float32(i)/2 {
			t.Fatalf("frame %d = %v, want %v", i, out[i], float32(i)/2)
		}
	}
}

// TestProviderPublishesPostGainAudio proves the splice order: the gain runs,
// then the tap observes, then the device reads. A unity tone scaled to half
// volume must reach both the device buffer and the tap as 0.5.
func TestProviderPublishesPostGainAudio(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 1.0, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	tap := s.Tap()
	tap.Read(nil) // first Read activates the feed before the provider publishes

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return s.Snapshot().Stats.Buffered > 0 })

	s.SetVolume(0.5)

	buf := make([]float32, 256*canonical.Ch)
	if _, err := s.provider.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames: %v", err)
	}
	for i, v := range buf {
		if math.Abs(float64(v-0.5)) > 1e-6 {
			t.Fatalf("device sample %d = %v, want 0.5 (gain applied before the device)", i, v)
		}
	}

	out := make([]float32, 256)
	n := tap.Read(out)
	if n == 0 {
		t.Fatal("tap observed no frames after a device read")
	}
	for i := range out[:n] {
		if math.Abs(float64(out[i]-0.5)) > 1e-6 {
			t.Fatalf("tap sample %d = %v, want 0.5 (tap must observe post-gain audio)", i, out[i])
		}
	}
}

// TestTapNeverBlocksPlayback reads once to activate the feed, then stops
// consuming entirely. Playback must keep making progress: a full tap ring
// overwrites oldest instead of stalling the audio thread.
func TestTapNeverBlocksPlayback(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.25, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	tap := s.Tap()
	tap.Read(nil) // activate, then abandon the consumer

	eventually(t, 3*time.Second, "playback to advance past 48000 frames with an unread tap", func() bool {
		return streamPosition(s) > 48000
	})
	if s.Snapshot().State != StatePlaying {
		t.Fatalf("state = %d, want Playing; the tap stalled playback", s.Snapshot().State)
	}
}

// TestProviderReadAllocatesNothingWhenTapInactive proves the zero-cost claim:
// with no consumer the provider's read path does not allocate, and a Tap handle
// that is never read must not activate publishing.
func TestProviderReadAllocatesNothingWhenTapInactive(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return s.Snapshot().Stats.Buffered > 0 })

	_ = s.Tap() // held but never read: must stay inactive

	buf := make([]float32, 256*canonical.Ch)
	// Warm the path once, then measure. The tap is never read.
	s.provider.ReadFrames(buf)

	allocs := testing.AllocsPerRun(200, func() {
		if _, err := s.provider.ReadFrames(buf); err != nil {
			t.Fatalf("ReadFrames: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("ReadFrames allocated %v times per call with no tap consumer, want 0", allocs)
	}
}

// TestTapConcurrentProducerConsumer exercises the real topology: one writer
// (the audio path) and one reader (a UI) on their own goroutines. It is the
// test the race detector is pointed at.
func TestTapConcurrentProducerConsumer(t *testing.T) {
	tap := newTapRing(1024)
	tap.attach()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]float32, 256*canonical.Ch)
		for i := range buf {
			buf[i] = float32(i%5) / 5
		}
		for i := 0; i < 2000; i++ {
			tap.publish(buf, 256, canonical.Ch)
		}
	}()

	out := make([]float32, 128)
	for {
		select {
		case <-done:
			return
		default:
			tap.Read(out)
		}
	}
}

// streamPosition reads the live streamer position without going through the
// provider lock, so the progress assertion cannot itself disturb the tap.
func streamPosition(s *Session) int64 {
	live := s.provider.current()
	if live == nil {
		return 0
	}

	return live.Position()
}

// BenchmarkTapPublishInactive measures the cost the audio path pays when no
// consumer has attached: one atomic load and a return.
func BenchmarkTapPublishInactive(b *testing.B) {
	tap := newTap()
	buf := make([]float32, 4800*canonical.Ch)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tap.publish(buf, 4800, canonical.Ch)
	}
}

// BenchmarkTapPublishActive measures the cost of a live tap: a stereo to mono
// downmix and a ring copy, still allocation-free.
func BenchmarkTapPublishActive(b *testing.B) {
	tap := newTap()
	tap.attach()
	buf := make([]float32, 4800*canonical.Ch)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tap.publish(buf, 4800, canonical.Ch)
	}
}

// BenchmarkTapRead measures the consumer side on a full ring.
func BenchmarkTapRead(b *testing.B) {
	tap := newTap()
	tap.attach()
	tap.writeMono(make([]float32, tapRingFrames))
	out := make([]float32, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tap.Read(out)
	}
}
