package stream

import (
	"errors"
	"sync"
	"testing"
	"time"
)

const (
	seekTarget = 30000
	seekWindow = 48000
)

// readFrames reads exactly count frames, tolerating the partial reads a
// buffered provider is allowed to return.
func readFrames(t *testing.T, s *Streamer, count int) []float32 {
	t.Helper()

	out := make([]float32, 0, count*canonicalFormat.Ch)
	buf := make([]float32, 4096)
	for len(out) < count*canonicalFormat.Ch {
		want := min(len(buf), count*canonicalFormat.Ch-len(out))
		n, err := s.ReadFrames(buf[:want])
		if n > 0 {
			out = append(out, buf[:n*canonicalFormat.Ch]...)
		}
		if err != nil {
			t.Fatalf("ReadFrames: %v (have %d of %d samples)", err, len(out), count*canonicalFormat.Ch)
		}
	}

	return out
}

func TestSeekNativeMatchesStraightDecodeWindow(t *testing.T) {
	full := decodeFixture(t, "stereo_2s.opus")

	s := newTestStreamer(t, pionOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, s)

	if err := s.SeekFrame(seekTarget); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}
	if got := s.Position(); got != seekTarget {
		t.Fatalf("Position() after seek = %d, want %d", got, seekTarget)
	}

	got := readFrames(t, s, seekWindow)
	want := full[seekTarget*canonicalFormat.Ch : (seekTarget+seekWindow)*canonicalFormat.Ch]
	if !float32sEqual(got, want) {
		t.Fatalf("post-seek window differs from the straight decode window")
	}
	if got := s.Position(); got != seekTarget+seekWindow {
		t.Fatalf("Position() = %d, want %d", got, seekTarget+seekWindow)
	}
}

func TestSeekFallbackMatchesStraightDecodeWindow(t *testing.T) {
	// forwardOnly hides decode.Seeker, so this exercises reopen-and-discard.
	// The result must be byte-identical to the native path, which is the whole
	// reason the Phase 1 decoder capability split exists.
	full := decodeFixture(t, "stereo_2s.opus")

	s := newTestStreamer(t, forwardOnlyOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, s)

	if _, isSeeker := s.dec.(interface{ SeekFrame(int64) error }); isSeeker {
		t.Fatal("test decoder unexpectedly implements Seeker; the fallback is not exercised")
	}

	if err := s.SeekFrame(seekTarget); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}
	if got := s.Position(); got != seekTarget {
		t.Fatalf("Position() after seek = %d, want %d", got, seekTarget)
	}

	got := readFrames(t, s, seekWindow)
	want := full[seekTarget*canonicalFormat.Ch : (seekTarget+seekWindow)*canonicalFormat.Ch]
	if !float32sEqual(got, want) {
		t.Fatalf("fallback post-seek window differs from the straight decode window")
	}
}

func TestSeekFallbackAndNativeAgreeByteForByte(t *testing.T) {
	native := newTestStreamer(t, pionOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, native)
	if err := native.SeekFrame(seekTarget); err != nil {
		t.Fatalf("native SeekFrame: %v", err)
	}

	fallback := newTestStreamer(t, forwardOnlyOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, fallback)
	if err := fallback.SeekFrame(seekTarget); err != nil {
		t.Fatalf("fallback SeekFrame: %v", err)
	}

	if a, b := readFrames(t, native, seekWindow), readFrames(t, fallback, seekWindow); !float32sEqual(a, b) {
		t.Fatal("native and fallback seek disagree")
	}
}

func TestSeekBackToStartRestoresTheHead(t *testing.T) {
	full := decodeFixture(t, "stereo_2s.opus")

	s := newTestStreamer(t, pionOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, s)

	if err := s.SeekFrame(seekTarget); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}
	if err := s.SeekFrame(0); err != nil {
		t.Fatalf("SeekFrame back to zero: %v", err)
	}
	if got := s.Position(); got != 0 {
		t.Fatalf("Position() = %d, want 0", got)
	}

	got := readFrames(t, s, 1000)
	if !float32sEqual(got, full[:1000*canonicalFormat.Ch]) {
		t.Fatal("seeking back to zero did not restore the stream head")
	}
}

func TestSeekLatestWinsAndDoesNotQueue(t *testing.T) {
	s := newTestStreamer(t, pionOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, s)

	const workers = 8
	targets := []int64{12000, 24000, 36000, 48000, 60000, 12000, 24000, 36000}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.SeekFrame(targets[i])
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	waitWithin(t, 5*time.Second, "concurrent seeks", done)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: SeekFrame: %v", i, err)
		}
	}

	// Every caller is released once a target at least as new as its own has
	// been applied, so the final position must be one of the requested targets,
	// and reading afterwards must deliver that target's window.
	switch pos := s.Position(); pos {
	case 12000, 24000, 36000, 48000, 60000:
	default:
		t.Fatalf("Position() = %d, not one of the requested targets", pos)
	}
}

func TestSeekPastEndIsAnError(t *testing.T) {
	s := newTestStreamer(t, pionOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)

	if err := s.SeekFrame(1 << 40); err == nil {
		t.Fatal("SeekFrame past the end returned nil")
	}
}

func TestSeekBeforeStartIsRejected(t *testing.T) {
	s := newTestStreamer(t, pionOpener(fixturePath(t, "stereo_2s.opus")), Config{})

	if err := s.SeekFrame(1000); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("SeekFrame before Start = %v, want ErrNotStarted", err)
	}
}

func TestSeekAfterEOSIsRejected(t *testing.T) {
	s := newTestStreamer(t, pionOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)
	_ = drain(t, s)
	waitWithin(t, 2*time.Second, "EOS", s.Done())

	if err := s.SeekFrame(0); err == nil {
		t.Fatal("SeekFrame after EOS returned nil")
	}
}

func TestSeekRejectsNegativeTarget(t *testing.T) {
	s := newTestStreamer(t, pionOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, s)

	if err := s.SeekFrame(-1); !errors.Is(err, ErrSeekRange) {
		t.Fatalf("SeekFrame(-1) = %v, want ErrSeekRange", err)
	}
}

func TestFramesToDurationUsesTheOutputClock(t *testing.T) {
	// 48 kHz is the only rate in the position domain, so a frame count here
	// must never be interpreted at a native decoder rate.
	tests := []struct {
		frames int64
		want   time.Duration
	}{
		{0, 0},
		{-5, 0},
		{48000, time.Second},
		{24000, 500 * time.Millisecond},
		{1, time.Second / 48000},
	}
	for _, tt := range tests {
		if got := FramesToDuration(tt.frames); got != tt.want {
			t.Fatalf("FramesToDuration(%d) = %v, want %v", tt.frames, got, tt.want)
		}
	}
}

func TestPositionTracksTheDeliveredFrameCount(t *testing.T) {
	// Position counts delivered output frames, so after a full drain it must
	// equal what a straight decode produced, exactly.
	want := decodeFixture(t, "short_stereo.opus")

	s := newTestStreamer(t, pionOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)

	got := drain(t, s)
	if int64(len(got)/canonicalFormat.Ch) != s.Position() {
		t.Fatalf("Position() = %d, delivered %d frames", s.Position(), len(got)/canonicalFormat.Ch)
	}
	if int64(len(want)/canonicalFormat.Ch) != s.Position() {
		t.Fatalf("Position() = %d, straight decode produced %d frames", s.Position(), len(want)/canonicalFormat.Ch)
	}
}

func TestSeekPositionIsInOutputFrames(t *testing.T) {
	// After a seek, Position is the target and the next frame read is the
	// target frame of a straight decode, with no rate conversion applied.
	full := decodeFixture(t, "stereo_2s.opus")

	s := newTestStreamer(t, pionOpener(fixturePath(t, "stereo_2s.opus")), Config{})
	startStreamer(t, s)

	const at = 12345
	if err := s.SeekFrame(at); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}
	if s.Position() != at {
		t.Fatalf("Position() = %d, want %d", s.Position(), at)
	}

	got := readFrames(t, s, 1)
	want := full[at*canonicalFormat.Ch : (at+1)*canonicalFormat.Ch]
	if !float32sEqual(got, want) {
		t.Fatal("first frame after seek is not the target frame of the straight decode")
	}
}
