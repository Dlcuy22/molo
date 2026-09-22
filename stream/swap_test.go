package stream

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
)

// toneSource is a synthetic decoder with a native seek and a per-instance
// value, so a swap test can tell two implementations apart by the samples it
// reads rather than by a side channel.
type toneSource struct {
	mu    sync.Mutex
	value float32
	total int64
	pos   int64
}

func (d *toneSource) Info() core.StreamInfo {
	return core.StreamInfo{Format: canonicalFormat, TotalFrames: d.total}
}

func (d *toneSource) ReadFrames(dst []float32) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pos >= d.total {
		return 0, io.EOF
	}
	frames := min(int64(len(dst)/canonicalFormat.Ch), d.total-d.pos)
	for i := range dst[:int(frames)*canonicalFormat.Ch] {
		dst[i] = d.value
	}
	d.pos += frames

	return int(frames), nil
}

func (d *toneSource) SeekFrame(frame int64) error {
	d.mu.Lock()
	d.pos = frame
	d.mu.Unlock()

	return nil
}

func (d *toneSource) Close() error { return nil }

// forwardToneSource hides the native seek, forcing the reopen-and-discard
// fallback on the swap.
type forwardToneSource struct{ decode.Decoder }

func toneOpener(value float32) Opener {
	return func(<-chan struct{}) (decode.Decoder, error) {
		return &toneSource{value: value, total: 1 << 30}, nil
	}
}

func forwardToneOpener(value float32) Opener {
	return func(<-chan struct{}) (decode.Decoder, error) {
		return forwardToneSource{&toneSource{value: value, total: 1 << 30}}, nil
	}
}

// readAll reads count frames, tolerating the partial reads the ring allows.
func readAll(t *testing.T, s *Streamer, count int) []float32 {
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

func assertAll(t *testing.T, samples []float32, value float32, what string) {
	t.Helper()

	for i := range samples {
		if samples[i] != value {
			t.Fatalf("%s sample %d = %v, want %v", what, i, samples[i], value)
		}
	}
}

func TestSwapDecoderReopensWithTheNewImplementation(t *testing.T) {
	s := newTestStreamer(t, toneOpener(0.5), Config{})
	startStreamer(t, s)

	// Consume a little so there is a real position to preserve.
	assertAll(t, readAll(t, s, 10000), 0.5, "pre-swap")

	const target = 20000
	if err := s.SwapDecoder(target, toneOpener(0.25)); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	if got := s.Position(); got != target {
		t.Fatalf("Position() = %d, want the swap target %d", got, target)
	}

	// The ring was flushed, so every frame after the swap must come from the new
	// implementation: a stale 0.5 sample would mean the old decoder leaked.
	assertAll(t, readAll(t, s, 10000), 0.25, "post-swap")
	if s.Position() != target+10000 {
		t.Fatalf("Position() = %d, want %d", s.Position(), target+10000)
	}
}

func TestSwapDecoderFallsBackToDiscardForAForwardOnlyDecoder(t *testing.T) {
	s := newTestStreamer(t, forwardToneOpener(0.5), Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 10000)

	const target = 12345
	if err := s.SwapDecoder(target, forwardToneOpener(0.75)); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	if got := s.Position(); got != target {
		t.Fatalf("Position() = %d, want %d", got, target)
	}

	assertAll(t, readAll(t, s, 100), 0.75, "post-swap")
}

func TestSwapDecoderFailureKeepsTheOldDecoderPlaying(t *testing.T) {
	boom := errors.New("swap opener boom")
	s := newTestStreamer(t, toneOpener(0.5), Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 8000)

	before := s.Position()
	err := s.SwapDecoder(40000, func(<-chan struct{}) (decode.Decoder, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("SwapDecoder = %v, want the opener error", err)
	}

	// The stream must still be readable and still deliver the old value; a
	// failed swap may not leave it closed or repositioned at the target.
	if s.Position() != before {
		t.Fatalf("Position() = %d, want the pre-swap %d", s.Position(), before)
	}
	assertAll(t, readAll(t, s, 1000), 0.5, "post-failure")
}

func TestSwapDecoderRejectsADifferentFormat(t *testing.T) {
	s := newTestStreamer(t, toneOpener(0.5), Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 4000)

	before := s.Position()
	bad := func(<-chan struct{}) (decode.Decoder, error) {
		return &silentDecoder{
			format: core.StreamInfo{Format: core.FrameFormat{Rate: 44100, Ch: 2, Fmt: core.F32}, TotalFrames: 1000},
		}, nil
	}
	if err := s.SwapDecoder(20000, bad); err == nil {
		t.Fatal("SwapDecoder accepted a decoder with a different format")
	}

	// The old implementation must be back and the position untouched.
	if s.Position() != before {
		t.Fatalf("Position() = %d, want the pre-swap %d", s.Position(), before)
	}
	assertAll(t, readAll(t, s, 500), 0.5, "post-failure")
}

func TestSwapDecoderBeforeStartIsRejected(t *testing.T) {
	s := newTestStreamer(t, toneOpener(0.5), Config{})

	if err := s.SwapDecoder(1000, toneOpener(0.25)); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("SwapDecoder before Start = %v, want ErrNotStarted", err)
	}
}

func TestSwapDecoderRequiresAnOpener(t *testing.T) {
	s := newTestStreamer(t, toneOpener(0.5), Config{})
	startStreamer(t, s)

	if err := s.SwapDecoder(1000, nil); err == nil {
		t.Fatal("SwapDecoder(nil) returned nil")
	}
	// The stream is untouched by the rejected call.
	_ = readAll(t, s, 10)
}

// swapGate parks a swap inside the producer so a test can hold one in flight
// and count how many reopens a concurrent burst actually pays for.
type swapGate struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	open    sync.Once
}

func newSwapGate() *swapGate {
	return &swapGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *swapGate) free() { g.open.Do(func() { close(g.release) }) }

func (g *swapGate) enter() {
	g.once.Do(func() { close(g.entered) })
	<-g.release
}

// gatedSource parks its native seek on the gate. A burst of concurrent swaps
// then collapses at the streamer's latest-wins slot rather than reopening once
// per call.
type gatedSource struct {
	mu    sync.Mutex
	gate  *swapGate
	value float32
	total int64
	pos   int64
}

func (d *gatedSource) Info() core.StreamInfo {
	return core.StreamInfo{Format: canonicalFormat, TotalFrames: d.total}
}

func (d *gatedSource) ReadFrames(dst []float32) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pos >= d.total {
		return 0, io.EOF
	}
	frames := min(int64(len(dst)/canonicalFormat.Ch), d.total-d.pos)
	for i := range dst[:int(frames)*canonicalFormat.Ch] {
		dst[i] = d.value
	}
	d.pos += frames

	return int(frames), nil
}

func (d *gatedSource) SeekFrame(frame int64) error {
	d.gate.enter()

	d.mu.Lock()
	d.pos = frame
	d.mu.Unlock()

	return nil
}

func (d *gatedSource) Close() error { return nil }

func TestSwapDecoderCollapsesAConcurrentBurst(t *testing.T) {
	gate := newSwapGate()
	defer gate.free()

	var mu sync.Mutex
	opens := 0
	open := func(<-chan struct{}) (decode.Decoder, error) {
		mu.Lock()
		opens++
		mu.Unlock()

		return &gatedSource{gate: gate, value: 0.5, total: 1 << 30}, nil
	}

	s := newTestStreamer(t, open, Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 1000)

	// The first swap parks inside the producer waiting on the gate, so it has
	// to run off the test goroutine: SwapDecoder only returns once the streamer
	// has applied the request.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.SwapDecoder(5000, open)
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first swap never parked")
	}

	const n = 12
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.SwapDecoder(int64(1000+i*100), open)
		}(i)
	}

	// Let the callers collide on the gate, then release everything.
	time.Sleep(50 * time.Millisecond)
	gate.free()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the swap burst never drained")
	}

	mu.Lock()
	got := opens
	mu.Unlock()

	// The initial open plus the swaps. Latest-wins means the burst collapses to
	// the in-flight swap plus at most one replacement, not one per call.
	if got > 3 {
		t.Fatalf("%d concurrent swaps cost %d reopens; latest-wins did not collapse the burst", n, got)
	}
}
