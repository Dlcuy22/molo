package stream

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
)

// fixturePath points at the Phase 1 fixtures rather than duplicating megabytes
// of Opus into this package's testdata; both packages test the same bytes.
func fixturePath(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("..", "decode", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s is missing: %v", name, err)
	}

	return path
}

// pionOpener reopens the pure-Go decoder, which is what both the native and the
// fallback seek paths need. It uses the fast default, whose 80 ms warm-up makes
// a seek bounded but not byte-exact against a straight decode.
func pionOpener(path string) Opener {
	return func(<-chan struct{}) (decode.Decoder, error) {
		return decode.NewPionOpusFactory().Open(path)
	}
}

// pionExactOpener reopens the bit-perfect pure-Go variant. Tests that assert a
// native seek reproduces a straight decode byte for byte use this one, because
// that byte identity is exactly what the exact warm-up buys; the fast default
// has a bounded transient instead.
func pionExactOpener(path string) Opener {
	return func(<-chan struct{}) (decode.Decoder, error) {
		return decode.NewPionOpusExactFactory().Open(path)
	}
}

// forwardOnly hides decode.Seeker behind the base Decoder interface, so the
// streamer is forced onto the reopen-and-discard fallback.
type forwardOnly struct {
	decode.Decoder
}

func forwardOnlyOpener(path string) Opener {
	return wrapForwardOnly(decode.NewPionOpusFactory().Open, path)
}

// forwardOnlyExactOpener is the fallback half of the byte-exact seek tests.
func forwardOnlyExactOpener(path string) Opener {
	return wrapForwardOnly(decode.NewPionOpusExactFactory().Open, path)
}

func wrapForwardOnly(open func(string) (decode.Decoder, error), path string) Opener {
	return func(<-chan struct{}) (decode.Decoder, error) {
		d, err := open(path)
		if err != nil {
			return nil, err
		}

		return forwardOnly{d}, nil
	}
}

func newTestStreamer(t *testing.T, open Opener, cfg Config) *Streamer {
	t.Helper()

	s, err := New(open, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return s
}

// drain reads until the stream reports end of stream and returns the PCM.
func drain(t *testing.T, s *Streamer) []float32 {
	t.Helper()

	buf := make([]float32, 4096)
	var out []float32
	for {
		n, err := s.ReadFrames(buf)
		if n > 0 {
			out = append(out, buf[:n*canonicalFormat.Ch]...)
		}
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadFrames: %v", err)
		}
		if n == 0 {
			t.Fatal("ReadFrames made no progress and returned no error")
		}
	}
}

func decodeFixture(t *testing.T, name string) []float32 {
	t.Helper()

	d, err := decode.NewPionOpusFactory().Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer d.Close()

	buf := make([]float32, 4096)
	var out []float32
	for {
		n, err := d.ReadFrames(buf)
		if n > 0 {
			out = append(out, buf[:n*canonicalFormat.Ch]...)
		}
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
	}
}

func float32sEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// scaleModule is a format-preserving stub that records the order in which
// modules ran and multiplies every sample by a constant.
type scaleModule struct {
	name   string
	factor float32
	log    *[]string
	resets int
}

func (m *scaleModule) Name() string { return m.name }

func (m *scaleModule) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	return in, nil
}

func (m *scaleModule) Process(buf []float32, frames int) error {
	if m.log != nil {
		*m.log = append(*m.log, m.name)
	}
	for i := range buf {
		buf[i] *= m.factor
	}

	return nil
}

func (m *scaleModule) Reset() error {
	m.resets++

	return nil
}

// silentDecoder produces zeroed PCM for a fixed frame count. It lets format
// validation be tested without a real container.
type silentDecoder struct {
	format core.StreamInfo
	frames int64
	done   int64
}

func (d *silentDecoder) Info() core.StreamInfo { return d.format }

func (d *silentDecoder) ReadFrames(dst []float32) (int, error) {
	if d.done >= d.frames {
		return 0, io.EOF
	}
	n := min(int64(len(dst)/d.format.Format.Ch), d.frames-d.done)
	for i := range dst[:n*int64(d.format.Format.Ch)] {
		dst[i] = 0
	}
	d.done += n

	return int(n), nil
}

func (d *silentDecoder) Close() error { return nil }

// declModule only declares a format; it never touches PCM. It exists to prove
// the Configure chain runs in order and its end is validated.
type declModule struct {
	name string
	out  core.FrameFormat
	seen []core.FrameFormat
}

func (m *declModule) Name() string { return m.name }

func (m *declModule) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	m.seen = append(m.seen, in)

	return m.out, nil
}

func (m *declModule) Process(buf []float32, frames int) error { return nil }

func (m *declModule) Reset() error { return nil }

// scriptedDecoder hands out fixed-size chunks. With a nil gate it produces
// freely, which is how the watermark tests let the producer run until the ring
// itself stops it. With a non-nil gate it produces only when allow is called,
// which makes the blocking tests deterministic instead of timing-dependent.
type scriptedDecoder struct {
	format   core.FrameFormat
	gate     chan struct{}
	stop     chan struct{}
	external <-chan struct{}
	stopOnce sync.Once
	chunk    int
	total    int
	done     int
	value    float32
}

func newScriptedDecoder(format core.FrameFormat, chunk, total int, value float32) *scriptedDecoder {
	return &scriptedDecoder{
		format: format,
		stop:   make(chan struct{}),
		chunk:  chunk,
		total:  total,
		value:  value,
	}
}

func newGatedDecoder(format core.FrameFormat, chunk, total int, value float32) *scriptedDecoder {
	d := newScriptedDecoder(format, chunk, total, value)
	d.gate = make(chan struct{}, 1024)

	return d
}

// opener returns an Opener that hands out this decoder and wires the streamer's
// shutdown signal into it, so a blocked read is interruptible exactly like a
// real interruptible source would be.
func (d *scriptedDecoder) opener() Opener {
	return func(stop <-chan struct{}) (decode.Decoder, error) {
		d.external = stop

		return d, nil
	}
}

func (d *scriptedDecoder) Info() core.StreamInfo {
	return core.StreamInfo{Format: d.format, TotalFrames: int64(d.total)}
}

func (d *scriptedDecoder) ReadFrames(dst []float32) (int, error) {
	if d.done >= d.total {
		return 0, io.EOF
	}
	if d.gate != nil {
		select {
		case <-d.stop:
			return 0, decode.ErrClosed
		case <-d.external:
			return 0, decode.ErrClosed
		case <-d.gate:
		}
	}

	frames := min(min(d.chunk, d.total-d.done), len(dst)/d.format.Ch)
	for i := 0; i < frames*d.format.Ch; i++ {
		dst[i] = d.value
	}
	d.done += frames

	return frames, nil
}

func (d *scriptedDecoder) Close() error {
	d.stopOnce.Do(func() { close(d.stop) })

	return nil
}

func (d *scriptedDecoder) allow(n int) {
	for i := 0; i < n; i++ {
		d.gate <- struct{}{}
	}
}

// eventually polls a predicate; it fails the test rather than hanging when the
// state never changes.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

func startStreamer(t *testing.T, s *Streamer) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return ctx
}
