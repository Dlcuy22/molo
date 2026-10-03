package stream

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
)

// finiteToneOpener and finiteForwardToneOpener reuse the swap-test tone sources
// with a bounded frame count, so a test can drain the stream to EOS and inspect
// the whole sample sequence instead of a window: continuity across the seam is
// a property of the entire run, not of a slice that might straddle it.
func finiteToneOpener(value float32, total int64) Opener {
	return func(<-chan struct{}) (decode.Decoder, error) {
		return &toneSource{value: value, total: total}, nil
	}
}

func finiteForwardToneOpener(value float32, total int64) Opener {
	return func(<-chan struct{}) (decode.Decoder, error) {
		return forwardToneSource{&toneSource{value: value, total: total}}, nil
	}
}

// wrongRateSource is seekable but declares a different rate, so a rejection can
// only come from the seamless swap's format check and not from its seekability
// check: seekability is promoted from the embedded tone source.
type wrongRateSource struct{ *toneSource }

func (d *wrongRateSource) Info() core.StreamInfo {
	return core.StreamInfo{
		Format:      core.FrameFormat{Rate: 44100, Ch: canonicalFormat.Ch, Fmt: core.F32},
		TotalFrames: d.total,
	}
}

// TestSeamlessSwapKeepsTheBufferedAudio is the continuity proof. A finite source
// lets the whole sequence be inspected: the head the consumer already took,
// then the ring and pending that were live at the swap, then the new decoder.
// The concatenation must have exactly the source's frame count, so no frame was
// dropped at the seam (a flush would shorten the run) or repeated (a rewind
// would lengthen it). It must flip from the old value to the new exactly once,
// and the flip must land beyond the buffered head, which is the frontier the
// producer handed the new decoder. Together those pin that the ring survived
// and the new decoder took over on the exact next unemitted frame.
func TestSeamlessSwapKeepsTheBufferedAudio(t *testing.T) {
	const (
		total = 1 << 20
		pre   = 10000
	)
	s := newTestStreamer(t, finiteForwardToneOpener(0.5, total), Config{})
	startStreamer(t, s)

	// Stop short of the ring so a real buffer is live when the swap lands.
	head := readAll(t, s, pre)
	assertAll(t, head, 0.5, "pre-swap head")
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return s.Stats().Buffered > 0 })

	before := s.Position()
	if err := s.SwapDecoderSeamless(finiteToneOpener(0.25, total)); err != nil {
		t.Fatalf("SwapDecoderSeamless: %v", err)
	}
	// The consumer's next frame is unchanged: only the producer's decoder moved.
	if got := s.Position(); got != before {
		t.Fatalf("Position() after seamless swap = %d, want %d unchanged", got, before)
	}

	full := append(head, drain(t, s)...)
	if got := len(full) / canonicalFormat.Ch; int64(got) != total {
		t.Fatalf("delivered %d frames, want %d; the seam dropped or repeated frames", got, total)
	}
	if got, want := s.Position(), int64(total); got != want {
		t.Fatalf("Position() after drain = %d, want %d", got, want)
	}

	flip := -1
	for f := 0; f < int(total); f++ {
		switch v := full[f*canonicalFormat.Ch]; v {
		case 0.5:
			if flip >= 0 {
				t.Fatalf("old value reappeared at frame %d after the new value at %d", f, flip)
			}
		case 0.25:
			if flip < 0 {
				flip = f
			}
		default:
			t.Fatalf("frame %d = %v, want 0.5 before the swap and 0.25 after", f, v)
		}
	}
	if flip <= pre {
		t.Fatalf("the new value starts at frame %d, want beyond the buffered head at %d; the ring was not kept", flip, pre)
	}
}

// TestFlushingSwapDropsTheBufferSeamlessKeeps pins the difference the new
// primitive exists for. The same openers and the same head are used on both
// paths; the seamless swap still has the old value buffered, while the flushing
// swap has discarded it, so the next frames already carry the new value.
func TestFlushingSwapDropsTheBufferSeamlessKeeps(t *testing.T) {
	const (
		total = 1 << 20
		pre   = 8000
	)

	seamless := newTestStreamer(t, finiteForwardToneOpener(0.5, total), Config{})
	startStreamer(t, seamless)
	_ = readAll(t, seamless, pre)
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return seamless.Stats().Buffered > 0 })
	buffered := int(seamless.Stats().Buffered)

	if err := seamless.SwapDecoderSeamless(finiteToneOpener(0.25, total)); err != nil {
		t.Fatalf("SwapDecoderSeamless: %v", err)
	}
	assertAll(t, readAll(t, seamless, buffered), 0.5, "seamless buffered run")

	flushing := newTestStreamer(t, finiteForwardToneOpener(0.5, total), Config{})
	startStreamer(t, flushing)
	_ = readAll(t, flushing, pre)
	if err := flushing.SwapDecoder(0, finiteToneOpener(0.25, total)); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	assertAll(t, readAll(t, flushing, buffered), 0.25, "flushed run")
}

// TestSeamlessSwapRejectsAForwardOnlyReplacement checks that a replacement
// without decode.Seeker is refused rather than guessed at, and that the refusal
// leaves the original decoder playing at the same position.
func TestSeamlessSwapRejectsAForwardOnlyReplacement(t *testing.T) {
	const total = 1 << 20
	s := newTestStreamer(t, finiteForwardToneOpener(0.5, total), Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 4000)

	before := s.Position()
	err := s.SwapDecoderSeamless(finiteForwardToneOpener(0.25, total))
	if !errors.Is(err, ErrSeamlessNotSeekable) {
		t.Fatalf("SwapDecoderSeamless = %v, want ErrSeamlessNotSeekable", err)
	}
	if s.Position() != before {
		t.Fatalf("Position() = %d, want the untouched %d", s.Position(), before)
	}
	assertAll(t, readAll(t, s, 4000), 0.5, "post-reject")
}

// TestSeamlessSwapOpenerErrorLeavesTheStreamPlaying checks that a failed open
// cannot strand the stream: the old decoder keeps running from where it was.
func TestSeamlessSwapOpenerErrorLeavesTheStreamPlaying(t *testing.T) {
	boom := errors.New("seamless opener boom")
	const total = 1 << 20
	s := newTestStreamer(t, finiteForwardToneOpener(0.5, total), Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 4000)

	before := s.Position()
	err := s.SwapDecoderSeamless(func(<-chan struct{}) (decode.Decoder, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("SwapDecoderSeamless = %v, want the opener error", err)
	}
	if s.Position() != before {
		t.Fatalf("Position() = %d, want the untouched %d", s.Position(), before)
	}
	assertAll(t, readAll(t, s, 4000), 0.5, "post-failure")
}

// TestSeamlessSwapRejectsAFormatMismatch checks that a seekable decoder with a
// different layout is still refused, because the ring and the pre-module chain
// were configured for one format. The stream must be left untouched.
func TestSeamlessSwapRejectsAFormatMismatch(t *testing.T) {
	const total = 1 << 20
	s := newTestStreamer(t, finiteForwardToneOpener(0.5, total), Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 4000)

	before := s.Position()
	bad := func(<-chan struct{}) (decode.Decoder, error) {
		return &wrongRateSource{&toneSource{value: 0.25, total: total}}, nil
	}
	err := s.SwapDecoderSeamless(bad)
	if err == nil {
		t.Fatal("SwapDecoderSeamless accepted a decoder with a different format")
	}
	if errors.Is(err, ErrSeamlessNotSeekable) {
		t.Fatalf("SwapDecoderSeamless = %v, want a format error before the seekability check", err)
	}
	if s.Position() != before {
		t.Fatalf("Position() = %d, want the untouched %d", s.Position(), before)
	}
	assertAll(t, readAll(t, s, 4000), 0.5, "post-failure")
}

// TestSeamlessSwapDoesNotUnderrunAPacedConsumer runs a consumer slower than the
// decoder, which is the real-time case: a flush would empty the ring under it
// and it would read short. Because the ring is the runway the seamless swap
// keeps, the buffered frames cover the reopen and no underrun is counted.
func TestSeamlessSwapDoesNotUnderrunAPacedConsumer(t *testing.T) {
	const total = 1 << 20
	s := newTestStreamer(t, finiteForwardToneOpener(0.5, total), Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 4800)
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return s.Stats().Buffered > 0 })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]float32, 256*canonicalFormat.Ch)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.ReadFrames(buf); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	time.Sleep(20 * time.Millisecond)
	before := s.Stats().Underruns
	if err := s.SwapDecoderSeamless(finiteToneOpener(0.25, total)); err != nil {
		t.Fatalf("SwapDecoderSeamless: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	after := s.Stats().Underruns

	if after != before {
		t.Fatalf("Underruns rose from %d to %d across a seamless swap; the ring was flushed and the paced consumer starved", before, after)
	}
}

func TestSeamlessSwapBeforeStartIsRejected(t *testing.T) {
	s := newTestStreamer(t, finiteForwardToneOpener(0.5, 1<<20), Config{})

	if err := s.SwapDecoderSeamless(finiteToneOpener(0.25, 1<<20)); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("SwapDecoderSeamless before Start = %v, want ErrNotStarted", err)
	}
}

func TestSeamlessSwapRequiresAnOpener(t *testing.T) {
	s := newTestStreamer(t, finiteForwardToneOpener(0.5, 1<<20), Config{})
	startStreamer(t, s)
	_ = readAll(t, s, 10)

	if err := s.SwapDecoderSeamless(nil); err == nil {
		t.Fatal("SwapDecoderSeamless(nil) returned nil")
	}
}
