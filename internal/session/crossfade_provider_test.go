package session

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/stream"
)

// newToneStreamer builds and starts a streamer over a synthetic constant-value
// decoder, so the mix in readCrossfade is deterministic and needs no fixture.
func newToneStreamer(t *testing.T, value float32) *stream.Streamer {
	t.Helper()

	open := func(<-chan struct{}) (decode.Decoder, error) {
		return &toneDecoder{value: value, total: 1 << 40}, nil
	}
	st, err := stream.New(open, stream.Config{})
	if err != nil {
		t.Fatalf("stream.New: %v", err)
	}
	if err := st.Start(context.Background()); err != nil {
		t.Fatalf("stream.Start: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return st
}

// waitBuffered blocks until a streamer has produced some audio, so a
// non-blocking read on the incoming side has something to return.
func waitBuffered(t *testing.T, st *stream.Streamer) {
	t.Helper()

	eventually(t, 2*time.Second, "the streamer to buffer audio", func() bool {
		return st.Stats().Buffered > 0
	})
}

// TestReadCrossfadeBlendsBothStreamsWithEqualPower pins the core of the blend:
// the outgoing and incoming samples are summed with the equal-power curve, so
// at the start of the ramp the output is the outgoing track and at the end it
// is the incoming one, with the power held constant across.
func TestReadCrossfadeBlendsBothStreamsWithEqualPower(t *testing.T) {
	from := newToneStreamer(t, 1)
	to := newToneStreamer(t, 0)
	waitBuffered(t, from)
	waitBuffered(t, to)

	p := newRoutedProvider(dsp.NewChain(), dsp.NewGain(1), newTap(), make(chan feedEvent, 8), make(chan struct{}))

	const ramp = 480 // 10 ms
	x := &crossfade{from: from, to: to, total: ramp}

	// Read the whole ramp in one buffer. The mix is the outgoing gain applied
	// to 1 plus the incoming gain applied to 0, so each sample equals the
	// outgoing gain, which must follow cos(t*pi/2).
	dst := make([]float32, ramp*canonical.Ch)
	n, err := p.readCrossfade(x, dst)
	if err != nil {
		t.Fatalf("readCrossfade: %v", err)
	}
	if n != ramp {
		t.Fatalf("readCrossfade mixed %d frames, want %d", n, ramp)
	}

	for i := 0; i < ramp; i++ {
		tt := float64(i) / float64(ramp)
		wantOut, _ := dsp.CrossfadeGains(tt)
		got := float64(dst[i*canonical.Ch])
		if math.Abs(got-wantOut) > 1e-4 {
			t.Fatalf("frame %d = %v, want the outgoing gain %v", i, got, wantOut)
		}
	}

	// The first frame is the outgoing track untouched and the last is nearly
	// the incoming one, so the blend really spans from one to the other.
	if got := dst[0]; got < 0.99 {
		t.Fatalf("first blended sample = %v, want ~1 (outgoing)", got)
	}
	if !x.finished() {
		t.Fatal("the ramp did not report finished after its full length")
	}
}

// TestReadCrossfadeNotifiesOnceWhenFinished proves the completion signal is
// one-shot: the controller must be told once, not on every buffer past the end.
func TestReadCrossfadeNotifiesOnceWhenFinished(t *testing.T) {
	from := newToneStreamer(t, 1)
	to := newToneStreamer(t, 0)
	waitBuffered(t, from)
	waitBuffered(t, to)

	feed := make(chan feedEvent, 8)
	p := newRoutedProvider(dsp.NewChain(), dsp.NewGain(1), newTap(), feed, make(chan struct{}))

	const ramp = 64
	x := &crossfade{from: from, to: to, total: ramp}
	dst := make([]float32, ramp*canonical.Ch)

	if _, err := p.readCrossfade(x, dst); err != nil {
		t.Fatalf("readCrossfade: %v", err)
	}
	// A second read past the ramp must not send a second notification.
	if _, err := p.readCrossfade(x, dst); err != nil {
		t.Fatalf("readCrossfade (past ramp): %v", err)
	}

	count := 0
	for {
		select {
		case ev := <-feed:
			if ev.kind == feedCrossfadeDone {
				count++
			}
		default:
			if count != 1 {
				t.Fatalf("crossfade-done notifications = %d, want exactly 1", count)
			}

			return
		}
	}
}

// TestAbortCrossfadeReturnsTheIncomingStreamer proves a manual transition can
// take the incoming streamer back to close it, and that aborting twice is safe.
func TestAbortCrossfadeReturnsTheIncomingStreamer(t *testing.T) {
	from := newToneStreamer(t, 1)
	to := newToneStreamer(t, 0)

	p := newRoutedProvider(dsp.NewChain(), dsp.NewGain(1), newTap(), make(chan feedEvent, 8), make(chan struct{}))
	p.beginCrossfade(from, to, 480)

	got := p.abortCrossfade()
	if got != to {
		t.Fatalf("abortCrossfade returned %p, want the incoming streamer %p", got, to)
	}
	if again := p.abortCrossfade(); again != nil {
		t.Fatalf("a second abort returned %p, want nil", again)
	}
}

// TestBeginCrossfadeRejectsDegenerateInputs keeps a bad ramp from being
// published: a nil side or a zero length is a no-op, not a broken mix.
func TestBeginCrossfadeRejectsDegenerateInputs(t *testing.T) {
	from := newToneStreamer(t, 1)
	to := newToneStreamer(t, 0)

	p := newRoutedProvider(dsp.NewChain(), dsp.NewGain(1), newTap(), make(chan feedEvent, 8), make(chan struct{}))

	p.beginCrossfade(nil, to, 480)
	if p.xfade.Load() != nil {
		t.Fatal("a nil outgoing streamer published a crossfade")
	}
	p.beginCrossfade(from, nil, 480)
	if p.xfade.Load() != nil {
		t.Fatal("a nil incoming streamer published a crossfade")
	}
	p.beginCrossfade(from, to, 0)
	if p.xfade.Load() != nil {
		t.Fatal("a zero-length ramp published a crossfade")
	}
}
