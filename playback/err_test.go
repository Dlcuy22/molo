//go:build !freebsd && !android && !ios

package playback

import (
	"errors"
	"io"
	"testing"
	"time"
)

// errProvider fails after a fixed number of frames. Its error is the one oto
// has to carry back on its own thread, which is what Device.Err is for.
type errProvider struct {
	framesLeft int
	err        error
}

func (p *errProvider) ReadFrames(dst []float32) (int, error) {
	if p.framesLeft <= 0 {
		return 0, p.err
	}
	frames := min(len(dst)/deviceChannels, p.framesLeft)
	for i := range dst[:frames*deviceChannels] {
		dst[i] = 0.01
	}
	p.framesLeft -= frames

	return frames, nil
}

func TestOtoErrIsNilOnAHealthyDevice(t *testing.T) {
	dev := openOtoDevice(t, newSilenceProvider(deviceRate/2, 0.01))
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := dev.Err(); err != nil {
		t.Fatalf("Err on a healthy device = %v, want nil", err)
	}
}

func TestOtoErrSurfacesAProviderFailure(t *testing.T) {
	sentinel := errors.New("decode exploded")
	dev := openOtoDevice(t, &errProvider{framesLeft: deviceRate / 10, err: sentinel})
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	eventually(t, 3*time.Second, "the provider error to surface", func() bool {
		return errors.Is(dev.Err(), sentinel)
	})
}

func TestOtoErrAfterCloseIsNil(t *testing.T) {
	// Close drops the player, so Err can no longer observe a backend failure.
	// Reporting a stale error would make every post-shutdown poll of Err look
	// like a fresh failure.
	dev := openOtoDevice(t, newSilenceProvider(deviceRate, 0.01))
	if err := dev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := dev.Err(); err != nil {
		t.Fatalf("Err after Close = %v, want nil", err)
	}
}

func TestOtoEndOfStreamIsNotAnError(t *testing.T) {
	// Reaching io.EOF is the normal end of a track. Err must stay nil so a
	// caller can tell a finished track from a broken one.
	dev := openOtoDevice(t, newSilenceProvider(deviceRate/10, 0.01))
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, 3*time.Second, "the stream to end", func() bool {
		return !otoIsPlaying(dev) && dev.Latency() == 0
	})
	if err := dev.Err(); err != nil {
		t.Fatalf("Err after EOS = %v, want nil", err)
	}
	if errors.Is(dev.Err(), io.EOF) {
		t.Fatal("Err reported io.EOF, which is the normal end of a track")
	}
}
