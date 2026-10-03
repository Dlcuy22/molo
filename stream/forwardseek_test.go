package stream

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/dlcuy22/player/decode"
)

// readerOnly hides io.Seeker, standing in for a network body. The Ogg factory
// then builds the forward-only reader, whose decoder still advertises a native
// SeekFrame that refuses. That combination is the real YTM upgrade state: a
// decoder that reports decode.Seeker but cannot reposition.
type readerOnly struct{ r io.Reader }

func (o readerOnly) Read(p []byte) (int, error) { return o.r.Read(p) }

// TestSeekRefusedByForwardOnlyDecoderFallsBack pins the difference between the
// real forward-only decoder and the test helper that hides Seeker: the pure-Go
// Opus decoder always has the SeekFrame method, but on a non-seekable source it
// returns an error rather than being absent. A seek must land by reopening and
// discarding, not fail the track, which is what the seamless upgrade relies on
// before its replacement is ready.
func TestSeekRefusedByForwardOnlyDecoderFallsBack(t *testing.T) {
	full := decodeFixture(t, "stereo_2s.opus")

	data, err := os.ReadFile(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	open := func(<-chan struct{}) (decode.Decoder, error) {
		return decode.NewPionOpusFactory().OpenReader(readerOnly{bytes.NewReader(data)})
	}
	s := newTestStreamer(t, open, Config{ForwardSeekFallback: true})
	startStreamer(t, s)

	// The decoder must satisfy Seeker, or the fallback under test is not the
	// one being exercised.
	if _, ok := s.dec.(decode.Seeker); !ok {
		t.Fatal("forward-only Opus decoder does not implement decode.Seeker; the test no longer pins the real case")
	}

	if err := s.SeekFrame(seekTarget); err != nil {
		t.Fatalf("SeekFrame on a refusing decoder: %v", err)
	}
	if got := s.Position(); got != seekTarget {
		t.Fatalf("Position() after seek = %d, want %d", got, seekTarget)
	}

	got := readFrames(t, s, seekWindow)
	want := full[seekTarget*canonicalFormat.Ch : (seekTarget+seekWindow)*canonicalFormat.Ch]
	if !float32sEqual(got, want) {
		t.Fatal("fallback post-seek window differs from the straight decode")
	}
}

// TestSeekRefusedWithoutFallbackSurfaces pins the default: without
// Config.ForwardSeekFallback a refused native seek is returned as-is, so a
// streamer that never opted into the forward-only path keeps its old behaviour
// and the caller can report the failure.
func TestSeekRefusedWithoutFallbackSurfaces(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	open := func(<-chan struct{}) (decode.Decoder, error) {
		return decode.NewPionOpusFactory().OpenReader(readerOnly{bytes.NewReader(data)})
	}
	s := newTestStreamer(t, open, Config{})
	startStreamer(t, s)

	if err := s.SeekFrame(seekTarget); !errors.Is(err, decode.ErrNotSeekable) {
		t.Fatalf("SeekFrame = %v, want decode.ErrNotSeekable", err)
	}
}
