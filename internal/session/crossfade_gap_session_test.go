package session

import (
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/playback"
)

// captureDevice consumes its provider on its own goroutine like a push backend
// and records the peak level of every buffer, so a test can prove the output
// never dropped to silence across a handoff. A short silent buffer at a track
// boundary is normal; a silent buffer in the middle of a blend is the bug.
type captureDevice struct {
	mu       sync.Mutex
	provider playback.Provider
	peaks    []float32
	stop     chan struct{}
	once     sync.Once
}

func newCaptureDevice() *captureDevice {
	return &captureDevice{stop: make(chan struct{})}
}

func (d *captureDevice) Open(_ core.FrameFormat, p playback.Provider) error {
	d.provider = p

	return nil
}

func (d *captureDevice) Start() error {
	go func() {
		buf := make([]float32, 4800*canonical.Ch)
		for {
			select {
			case <-d.stop:
				return
			default:
			}
			n, err := d.provider.ReadFrames(buf)
			if err != nil {
				return
			}
			var peak float32
			for i := 0; i < n*canonical.Ch; i++ {
				if v := buf[i]; v < 0 {
					if -v > peak {
						peak = -v
					}
				} else if v > peak {
					peak = v
				}
			}
			d.mu.Lock()
			d.peaks = append(d.peaks, peak)
			d.mu.Unlock()
			if n > 0 {
				time.Sleep(time.Duration(n) * time.Second / time.Duration(canonical.Rate))
			}
		}
	}()

	return nil
}

func (d *captureDevice) Pause() error  { return nil }
func (d *captureDevice) Resume() error { return nil }
func (d *captureDevice) Flush() error  { return nil }
func (d *captureDevice) Close() error {
	d.once.Do(func() { close(d.stop) })

	return nil
}
func (d *captureDevice) Latency() time.Duration { return 0 }
func (d *captureDevice) Err() error             { return nil }

// TestCrossfadeHasNoSilentGapWhenTheProbedDurationIsLong reproduces the reported
// stutter end to end: the outgoing track's probe reports a duration longer than
// its real audio, so it ends part-way through the blend. The device output must
// stay sounding through the handoff, never dropping a whole buffer to silence
// the moment the outgoing track runs out.
func TestCrossfadeHasNoSilentGapWhenTheProbedDurationIsLong(t *testing.T) {
	// Track one holds 0.5 s of audio but probes as 1.0 s, so with a 1 s ramp the
	// blend starts at 0 s remaining (immediately) and the outgoing ends at
	// 0.5 s into it.
	const realFrames = 24000   // 0.5 s
	const probedFrames = 48000 // 1.0 s

	dev := newCaptureDevice()
	cfg := testConfig()
	cfg.openDecoder = func(_, path string) (decode.Decoder, error) {
		if path == "one" {
			return &toneDecoder{value: 0.25, total: realFrames}, nil
		}

		return &toneDecoder{value: 0.25, total: 48000 * 30}, nil
	}
	cfg.probeStream = func(path string, _ decode.ProbeOptions) (core.StreamInfo, error) {
		if path == "one" {
			return core.StreamInfo{Format: canonical, TotalFrames: probedFrames}, nil
		}

		return core.StreamInfo{Format: canonical, TotalFrames: 48000 * 30}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return dev }}).new
	s := newSession(t, cfg)

	if err := s.SetCrossfade(CrossfadeSettings{Enabled: true, DurationMs: MinCrossfadeMs}); err != nil {
		t.Fatalf("SetCrossfade: %v", err)
	}
	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Let the whole handoff happen: the outgoing ends at 0.5 s and the incoming
	// is promoted and keeps playing.
	waitTrackChanged(t, s, 1, 6*time.Second)
	time.Sleep(300 * time.Millisecond)

	dev.mu.Lock()
	peaks := append([]float32(nil), dev.peaks...)
	dev.mu.Unlock()

	if len(peaks) < 5 {
		t.Fatalf("the device captured only %d buffers, too few to judge the handoff", len(peaks))
	}
	// After the first buffer (the very start can be quiet), no buffer across the
	// handoff may be all silence: that is the dropout the user heard.
	for i := 1; i < len(peaks); i++ {
		if peaks[i] == 0 {
			t.Fatalf("buffer %d of %d was silent during the handoff; peaks=%v", i, len(peaks), peaks)
		}
	}
}
