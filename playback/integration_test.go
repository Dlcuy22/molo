//go:build !freebsd && !android && !ios

package playback

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/stream"
)

// fixturePath points at the Phase 1 Opus fixtures. They live in decode's
// testdata because this path is about the same bytes those tests cover; the
// integration here is the wiring, not the codec.
func fixturePath(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("..", "decode", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s is missing: %v", name, err)
	}

	return path
}

// startStreamer builds and starts a real streamer over a real Opus fixture.
func startStreamer(t *testing.T, name string) *stream.Streamer {
	t.Helper()

	path := fixturePath(t, name)
	open := func(<-chan struct{}) (decode.Decoder, error) {
		return decode.NewOpusFactory().Open(path)
	}

	s, err := stream.New(open, stream.Config{})
	if err != nil {
		t.Fatalf("stream.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return s
}

// TestIntegrationStreamPlaysToCompletion is the real end-to-end proof: a real
// Opus fixture, decoded by the real decoder, buffered by the real streamer,
// pulled through the real oto device. It fails, rather than hangs, if any
// stage stops making progress.
func TestIntegrationStreamPlaysToCompletion(t *testing.T) {
	const totalFrames = 96000 // the stereo_2s fixture is exactly 2 s at 48 kHz

	s := startStreamer(t, "stereo_2s.opus")
	dev := openOtoDevice(t, s)

	start := time.Now()
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the stream to be exhausted and every buffered frame to drain.
	waitFor(t, 15*time.Second, "the stream to finish and drain", func() bool {
		select {
		case <-s.Done():
		default:
			return false
		}

		return s.Position() >= totalFrames && dev.Latency() == 0 && !otoIsPlaying(dev)
	})
	elapsed := time.Since(start)

	if got := s.Position(); got < totalFrames || got > totalFrames+5760 {
		// The pure-Go decoder emits up to one extra Opus packet past the
		// granule total, the same tail decode/opus_test.go allows.
		t.Fatalf("Position() = %d, want within [%d, %d]", got, totalFrames, totalFrames+5760)
	}
	if err := s.Stats().Underruns; err != 0 {
		t.Fatalf("stream reported %d underruns, want 0", err)
	}

	// The fixture is 2 s; a real playback should be within a generous margin of
	// that. The lower bound is what makes this meaningful: it cannot pass by
	// returning instantly, which a stub would.
	want := 2 * time.Second
	t.Logf("end-to-end playback: %v wall clock for a %v fixture", elapsed.Round(time.Millisecond), want)
	if elapsed < want-250*time.Millisecond {
		t.Fatalf("playback finished in %v, too fast for a %v fixture", elapsed, want)
	}
	if elapsed > want+5*time.Second {
		t.Fatalf("playback took %v, far past the %v fixture", elapsed, want)
	}

	if err := dev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestIntegrationGainOnThePostRingPath inserts dsp.Gain where the plan puts it,
// between the ring and the device, and proves the real path still completes
// with a live volume change. The wrapper is exactly what the future facade
// will do; keeping it in a test here pins the placement.
func TestIntegrationGainOnThePostRingPath(t *testing.T) {
	s := startStreamer(t, "short_stereo.opus")

	gain := dsp.NewGain(1)
	if _, err := gain.Configure(deviceFormat); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	prov := &gainProvider{src: s, gain: gain}
	dev := openOtoDevice(t, prov)

	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	gain.SetVolume(0.5)

	waitFor(t, 10*time.Second, "the short fixture to finish", func() bool {
		select {
		case <-s.Done():
		default:
			return false
		}

		return dev.Latency() == 0 && !otoIsPlaying(dev)
	})
	if got := s.Position(); got < 12000 || got > 12000+5760 {
		t.Fatalf("Position() = %d, want within [12000, %d]", got, 12000+5760)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestIntegrationCloseMidPlayback proves the whole path tears down while audio
// is in flight without deadlocking: the device stops pulling, so the streamer
// can be closed and replayed afterwards.
func TestIntegrationCloseMidPlayback(t *testing.T) {
	s := startStreamer(t, "stereo_2s.opus")
	dev := openOtoDevice(t, s)
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 5*time.Second, "playback to start", func() bool { return s.Position() > 0 })

	done := make(chan error, 1)
	go func() { done <- dev.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close mid-playback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close mid-playback did not return")
	}

	// A closed device must not keep draining the streamer.
	time.Sleep(50 * time.Millisecond)
	paused := s.Position()
	time.Sleep(100 * time.Millisecond)
	if s.Position() != paused {
		t.Fatalf("streamer advanced from %d to %d frames after the device closed", paused, s.Position())
	}
}

// gainProvider is the post-ring seam: it reads from the ring and scales in
// place before the device sees a sample, with no allocation on the way.
type gainProvider struct {
	src  Provider
	gain *dsp.Gain
}

func (p *gainProvider) ReadFrames(dst []float32) (int, error) {
	n, err := p.src.ReadFrames(dst)
	if n > 0 {
		if perr := p.gain.Process(dst, n); perr != nil && !errors.Is(perr, io.EOF) {
			return n, perr
		}
	}

	return n, err
}

// otoIsPlaying reports whether the wrapped oto player is still running. The
// real player drains synchronously, so IsPlaying turning false is the signal
// that the tail reached the device.
func otoIsPlaying(d *otoDevice) bool {
	d.mu.Lock()
	player := d.player
	d.mu.Unlock()
	if player == nil {
		return false
	}

	return player.IsPlaying()
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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
