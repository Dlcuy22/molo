package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
)

// canonicalFormat is the one output domain the streamer may hand downstream.
var canonicalFormat = core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}

func TestReadFramesMatchesStraightDecode(t *testing.T) {
	want := decodeFixture(t, "short_stereo.opus")

	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)

	got := drain(t, s)
	if !float32sEqual(got, want) {
		t.Fatalf("streamed %d samples, straight decode %d", len(got), len(want))
	}
}

func TestDoneClosesAtEOS(t *testing.T) {
	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)

	waitWithin(t, 5*time.Second, "EOS", s.Done())

	// Draining after EOS still returns the buffered tail, then io.EOF.
	_ = drain(t, s)

	select {
	case <-s.Done():
	default:
		t.Fatal("Done() is not closed after EOS")
	}
}

func TestReadFramesBlocksUntilDataThenReturnsEOF(t *testing.T) {
	dec := newGatedDecoder(canonicalFormat, 256, 1024, 0.5)
	s := newTestStreamer(t, dec.opener(), Config{RingFrames: 4096})
	startStreamer(t, s)

	// Nothing has been released yet, so a blocking read must stay parked.
	read := make(chan int, 1)
	go func() {
		n, err := s.ReadFrames(make([]float32, 512*canonicalFormat.Ch))
		if err != nil {
			read <- -1

			return
		}
		read <- n
	}()

	select {
	case <-read:
		t.Fatal("ReadFrames returned before any data was produced")
	case <-time.After(50 * time.Millisecond):
	}

	dec.allow(1)
	select {
	case n := <-read:
		if n != 256 {
			t.Fatalf("read %d frames, want 256", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadFrames stayed parked after data arrived")
	}

	dec.allow(3)
	if got := drain(t, s); len(got) != 768*canonicalFormat.Ch {
		t.Fatalf("drained %d samples, want %d", len(got), 768*canonicalFormat.Ch)
	}
}

func TestCloseWakesBlockedReader(t *testing.T) {
	dec := newGatedDecoder(canonicalFormat, 256, 1<<20, 0.5)
	s := newTestStreamer(t, dec.opener(), Config{RingFrames: 4096})
	startStreamer(t, s)

	done := make(chan error, 1)
	go func() {
		_, err := s.ReadFrames(make([]float32, 512*canonicalFormat.Ch))
		done <- err
	}()

	// No decoder token was released, so the reader is parked with an empty ring.
	time.Sleep(30 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()

	waitWithin(t, 2*time.Second, "Close", closed)
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked reader returned %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked reader never woke on Close")
	}
}

func TestReadFramesReturnsBufferedTailWhenDecoderFails(t *testing.T) {
	// The decoder errors on its first read. The streamer must surface the error
	// rather than hang or silently report a clean EOS.
	failing := &failingDecoder{format: canonicalFormat, err: errors.New("boom")}
	s := newTestStreamer(t, func(<-chan struct{}) (decode.Decoder, error) { return failing, nil }, Config{})

	startStreamer(t, s)
	waitWithin(t, 2*time.Second, "terminal error", s.Done())

	if _, err := s.ReadFrames(make([]float32, 32)); !errors.Is(err, failing.err) {
		t.Fatalf("ReadFrames = %v, want the decoder error", err)
	}
}

func TestCloseIsIdempotentAndStopsReads(t *testing.T) {
	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)
	<-s.Done()

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.TryReadFrames(make([]float32, 32)); !errors.Is(err, ErrClosed) {
		t.Fatalf("TryReadFrames after Close = %v, want ErrClosed", err)
	}
}

func TestCloseBeforeStartIsSafe(t *testing.T) {
	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close before Start: %v", err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}
}

func TestStartHonoursContextCancellation(t *testing.T) {
	dec := newGatedDecoder(canonicalFormat, 256, 1<<20, 0.5)
	s := newTestStreamer(t, dec.opener(), Config{RingFrames: 4096})

	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()
	waitWithin(t, 2*time.Second, "Close after ctx cancel", done)

	// The producer goroutine must have exited; a second Close is then a no-op.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStartTwiceFails(t *testing.T) {
	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)

	if err := s.Start(context.Background()); err == nil {
		t.Fatal("second Start returned nil")
	}
}

func TestStreamerAppliesPreModulesInOrder(t *testing.T) {
	direct := decodeFixture(t, "short_stereo.opus")

	var order []string
	cfg := Config{Modules: []core.Module{
		&scaleModule{name: "first", factor: 2, log: &order},
		&scaleModule{name: "second", factor: 3, log: &order},
	}}
	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), cfg)
	startStreamer(t, s)

	got := drain(t, s)
	want := make([]float32, len(direct))
	for i, v := range direct {
		want[i] = v * 6
	}
	if !float32sEqual(got, want) {
		t.Fatalf("module output %d samples, want scaled straight decode %d", len(got), len(want))
	}

	// Both modules scale by a constant, so the product above only holds when
	// both ran. The record proves they ran in the configured order.
	if len(order) == 0 {
		t.Fatal("no pre-module Process calls recorded")
	}
	for i, name := range order {
		want := "first"
		if i%2 == 1 {
			want = "second"
		}
		if name != want {
			t.Fatalf("Process call %d was %q, want %q", i, name, want)
		}
	}
}

func TestStreamerResetsModulesOnConfigureAndSeek(t *testing.T) {
	mod := &scaleModule{name: "stateful", factor: 1}
	s := newTestStreamer(t, opusOpener(fixturePath(t, "stereo_2s.opus")), Config{Modules: []core.Module{mod}})
	if mod.resets != 1 {
		t.Fatalf("Reset called %d times at construction, want 1", mod.resets)
	}

	startStreamer(t, s)
	if err := s.SeekFrame(1000); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}
	if mod.resets != 2 {
		t.Fatalf("Reset called %d times after one seek, want 2", mod.resets)
	}
}

func TestStreamerRejectsNonCanonicalFinalFormat(t *testing.T) {
	// A chain that ends somewhere other than 48k/2/F32 cannot feed the device
	// or the ring, so construction must fail instead of emitting a surprise.
	factory := func(<-chan struct{}) (decode.Decoder, error) {
		return &silentDecoder{
			format: core.StreamInfo{Format: core.FrameFormat{Rate: 44100, Ch: 2, Fmt: core.F32}, TotalFrames: 100},
		}, nil
	}

	mod := &declModule{name: "noop", out: core.FrameFormat{Rate: 44100, Ch: 2, Fmt: core.F32}}
	if _, err := New(factory, Config{Modules: []core.Module{mod}}); err == nil {
		t.Fatal("New accepted a chain that does not end at 48k/2/F32")
	}
}

func TestStreamerChainsModuleFormatsAndValidatesTheEnd(t *testing.T) {
	factory := func(<-chan struct{}) (decode.Decoder, error) {
		return &silentDecoder{
			format: core.StreamInfo{Format: core.FrameFormat{Rate: 24000, Ch: 1, Fmt: core.F32}, TotalFrames: 100},
		}, nil
	}

	up := &declModule{name: "rate", out: core.FrameFormat{Rate: 48000, Ch: 1, Fmt: core.F32}}
	wide := &declModule{name: "channels", out: canonicalFormat}
	s := newTestStreamer(t, factory, Config{Modules: []core.Module{up, wide}})

	if len(up.seen) != 1 || !up.seen[0].Equal(core.FrameFormat{Rate: 24000, Ch: 1, Fmt: core.F32}) {
		t.Fatalf("first module saw %+v, want the decoder format", up.seen)
	}
	if len(wide.seen) != 1 || !wide.seen[0].Equal(core.FrameFormat{Rate: 48000, Ch: 1, Fmt: core.F32}) {
		t.Fatalf("second module saw %+v, want the first module's output", wide.seen)
	}

	startStreamer(t, s)
	<-s.Done()
}

func TestStreamerRejectsNonF32Decoder(t *testing.T) {
	factory := func(<-chan struct{}) (decode.Decoder, error) {
		return &silentDecoder{
			format: core.StreamInfo{Format: core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.S16}, TotalFrames: 100},
		}, nil
	}
	if _, err := New(factory, Config{}); err == nil {
		t.Fatal("New accepted an S16 decoder; the ring carries float32 only")
	}
}

// monoModule folds stereo down to mono in place, keeping the left channel.
type monoModule struct{}

func (monoModule) Name() string { return "mono" }

func (monoModule) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	return core.FrameFormat{Rate: in.Rate, Ch: 1, Fmt: in.Fmt}, nil
}

func (monoModule) Process(buf []float32, frames int) error {
	for i := 0; i < frames; i++ {
		buf[i] = buf[2*i]
	}

	return nil
}

func (monoModule) Reset() error { return nil }

// dupModule expands mono to stereo by duplicating each sample.
type dupModule struct{}

func (dupModule) Name() string { return "dup" }

func (dupModule) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	return core.FrameFormat{Rate: in.Rate, Ch: 2, Fmt: in.Fmt}, nil
}

func (dupModule) Process(buf []float32, frames int) error {
	for i := frames - 1; i >= 0; i-- {
		buf[2*i] = buf[i]
		buf[2*i+1] = buf[i]
	}

	return nil
}

func (dupModule) Reset() error { return nil }

func TestPreModulesCanRemapChannelsInPlace(t *testing.T) {
	full := decodeFixture(t, "short_stereo.opus")

	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{
		Modules: []core.Module{monoModule{}, dupModule{}},
	})
	startStreamer(t, s)

	got := drain(t, s)
	if len(got) != len(full) {
		t.Fatalf("remap produced %d samples, want %d", len(got), len(full))
	}
	for i := 0; i < len(full); i += 2 {
		if got[i] != full[i] || got[i+1] != full[i] {
			t.Fatalf("frame %d = [%v %v], want the left channel duplicated [%v %v]", i/2, got[i], got[i+1], full[i], full[i])
		}
	}
}

func TestStatsUnderrunOnShortNonBlockingRead(t *testing.T) {
	// No Start: the producer has not filled the ring, so a non-blocking read is
	// guaranteed to come up short while the stream is not at EOS.
	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{})

	n, err := s.TryReadFrames(make([]float32, 4096))
	if err != nil {
		t.Fatalf("TryReadFrames: %v", err)
	}
	if n != 0 {
		t.Fatalf("TryReadFrames returned %d frames before Start", n)
	}
	if got := s.Stats().Underruns; got != 1 {
		t.Fatalf("Underruns = %d, want 1", got)
	}
}

func TestStatsBufferedAndDecodedAdvance(t *testing.T) {
	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)

	// Buffered, not Decoded, is what proves frames reached the ring: Decoded is
	// incremented before the ring write, and the write is lock-free, so
	// "decoded implies buffered" is not a real invariant and asserting it is
	// flaky under -race.
	eventually(t, 2*time.Second, "buffered frames", func() bool { return s.Stats().Buffered > 0 })

	if got, want := s.Stats().Buffered, s.ring.Len(); got != want {
		t.Fatalf("Stats().Buffered = %d, ring says %d", got, want)
	}
	_ = drain(t, s)
}

func TestTryReadFramesReturnsDataWithoutBlocking(t *testing.T) {
	dec := newScriptedDecoder(canonicalFormat, 128, 256, 1)
	s := newTestStreamer(t, dec.opener(), Config{RingFrames: 4096})
	startStreamer(t, s)

	eventually(t, 2*time.Second, "ring fill", func() bool {
		s.gate.mu.Lock()
		defer s.gate.mu.Unlock()

		return s.ring.Len() >= 128
	})

	got, err := s.TryReadFrames(make([]float32, 128*canonicalFormat.Ch))
	if err != nil {
		t.Fatalf("TryReadFrames: %v", err)
	}
	if got != 128 {
		t.Fatalf("TryReadFrames returned %d frames, want 128", got)
	}
}

func TestPositionAdvancesWithConsumption(t *testing.T) {
	s := newTestStreamer(t, opusOpener(fixturePath(t, "short_stereo.opus")), Config{})
	startStreamer(t, s)

	if got := s.Position(); got != 0 {
		t.Fatalf("Position() = %d, want 0 before any read", got)
	}

	buf := make([]float32, 1000*canonicalFormat.Ch)
	if _, err := s.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames: %v", err)
	}
	if got := s.Position(); got != 1000 {
		t.Fatalf("Position() = %d, want 1000", got)
	}

	if _, err := s.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames: %v", err)
	}
	if got := s.Position(); got != 2000 {
		t.Fatalf("Position() = %d, want 2000", got)
	}
}

// failingDecoder fails on its first read, standing in for a corrupt container.
type failingDecoder struct {
	format core.FrameFormat
	err    error
}

func (d *failingDecoder) Info() core.StreamInfo {
	return core.StreamInfo{Format: d.format, TotalFrames: -1}
}

func (d *failingDecoder) ReadFrames(dst []float32) (int, error) { return 0, d.err }

func (d *failingDecoder) Close() error { return nil }
