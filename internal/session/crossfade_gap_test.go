package session

import (
	"context"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/stream"
)

// newFiniteToneStreamer builds a streamer over a constant-value decoder that
// ends after total frames, so a test can drive the outgoing track to its end
// in the middle of a ramp.
func newFiniteToneStreamer(t *testing.T, value float32, total int64) *stream.Streamer {
	t.Helper()

	open := func(<-chan struct{}) (decode.Decoder, error) {
		return &toneDecoder{value: value, total: total}, nil
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

// TestReadCrossfadeKeepsSoundingWhenTheOutgoingTrackEndsEarly reproduces the
// reported stutter: the outgoing track's probed duration is longer than its
// real audio, so it reaches its end part-way through the ramp. The blend must
// keep sounding the incoming track as the outgoing one falls silent, not drop
// the whole buffer to silence (which is heard as a skip right at the handoff).
func TestReadCrossfadeKeepsSoundingWhenTheOutgoingTrackEndsEarly(t *testing.T) {
	// The outgoing track holds 480 frames (10 ms); the ramp is 960 frames
	// (20 ms), so the outgoing ends half-way through the blend.
	const outgoingFrames = 480
	const ramp = 960
	from := newFiniteToneStreamer(t, 1, outgoingFrames)
	to := newToneStreamer(t, 1) // constant full-scale incoming
	waitBuffered(t, to)

	p := newRoutedProvider(dsp.NewChain(), dsp.NewGain(1), newTap(), make(chan feedEvent, 8), make(chan struct{}))
	x := &crossfade{from: from, to: to, total: ramp}

	// Read the whole ramp in 240-frame buffers (5 ms), the way a device would.
	const buf = 240
	dst := make([]float32, buf*canonical.Ch)
	var out []float32
	for i := 0; i < ramp/buf+2; i++ {
		n, err := p.readCrossfade(x, dst)
		if n == 0 {
			// The bug: the provider hands the device silence for a whole
			// buffer as soon as the outgoing track ends, even though the
			// incoming track still has audio to fade in.
			t.Fatalf("buffer %d: readCrossfade returned 0 frames (err=%v); the blend dropped to silence mid-ramp", i, err)
		}
		for j := 0; j < n*canonical.Ch; j++ {
			out = append(out, dst[j])
		}
	}

	// Past the outgoing track's end the incoming track must still be audible:
	// its gain is climbing, so the output there must not be all-zero.
	silent := true
	for _, v := range out[outgoingFrames*canonical.Ch:] {
		if v != 0 {
			silent = false

			break
		}
	}
	if silent {
		t.Fatal("the output after the outgoing track ended was all silence; the incoming fade-in was lost")
	}
}

// TestReadCrossfadeKeepsTheOutgoingSoundingWhenTheIncomingStalls is the mirror
// case: the incoming track has nothing buffered for a stretch (a slow source).
// The outgoing side must keep sounding at its gain, so a stall on the incoming
// side is a dip in the incoming, never a gap in the whole output.
func TestReadCrossfadeKeepsTheOutgoingSoundingWhenTheIncomingStalls(t *testing.T) {
	from := newToneStreamer(t, 1) // constant full-scale outgoing
	waitBuffered(t, from)

	// An incoming streamer whose ring is deliberately empty: it was started but
	// nothing has been read from it, so TryReadFrames returns nothing.
	to := newFiniteToneStreamer(t, 1, 0)
	// Give the producer a moment to finish, so its ring stays empty.
	time.Sleep(20 * time.Millisecond)

	p := newRoutedProvider(dsp.NewChain(), dsp.NewGain(1), newTap(), make(chan feedEvent, 8), make(chan struct{}))
	x := &crossfade{from: from, to: to, total: 4800}

	const buf = 480
	dst := make([]float32, buf*canonical.Ch)
	n, err := p.readCrossfade(x, dst)
	if n != buf {
		t.Fatalf("readCrossfade returned %d frames (err=%v), want %d: an empty incoming side must not shrink the buffer", n, err, buf)
	}
	// The outgoing side carries the output, so it must not be silence.
	nonZero := false
	for i := 0; i < n*canonical.Ch; i++ {
		if dst[i] != 0 {
			nonZero = true

			break
		}
	}
	if !nonZero {
		t.Fatal("the output was silence while the outgoing track still had audio")
	}
}

// TestReadCrossfadePassesOnAShortOutgoingRead proves a partial read from the
// outgoing side is reported as a short read, not padded with silence: a
// mid-track underrun must not clip audio the producer has not handed over yet.
// The incoming side is full, so the blend simply covers the frames the outgoing
// produced.
func TestReadCrossfadePassesOnAShortOutgoingRead(t *testing.T) {
	// An outgoing streamer that has only a few frames buffered: its producer is
	// throttled by a decoder that returns one small chunk then blocks.
	open := func(<-chan struct{}) (decode.Decoder, error) {
		return &burstDecoder{frames: 60}, nil
	}
	from, err := stream.New(open, stream.Config{})
	if err != nil {
		t.Fatalf("stream.New: %v", err)
	}
	if err := from.Start(context.Background()); err != nil {
		t.Fatalf("stream.Start: %v", err)
	}
	t.Cleanup(func() { _ = from.Close() })
	waitBuffered(t, from)

	to := newToneStreamer(t, 1)
	waitBuffered(t, to)

	p := newRoutedProvider(dsp.NewChain(), dsp.NewGain(1), newTap(), make(chan feedEvent, 8), make(chan struct{}))
	x := &crossfade{from: from, to: to, total: 1 << 30}

	const buf = 4800 // ask for far more than the outgoing has
	dst := make([]float32, buf*canonical.Ch)
	n, err := p.readCrossfade(x, dst)
	if err != nil {
		t.Fatalf("readCrossfade: %v", err)
	}
	if n > 60 {
		t.Fatalf("readCrossfade returned %d frames, want at most the 60 the outgoing produced (a short read must not be padded)", n)
	}
	if n == 0 {
		t.Fatal("readCrossfade returned no frames despite buffered outgoing audio")
	}
}
