//go:build !freebsd && !android && !ios

package session

import (
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/playback"
)

// TestIntegrationQueuePlaysInOrder drives the fake backend over two real Opus
// fixtures. It is the end-to-end proof that the controller, streamer, gain and
// device fit together, and it is quiet: the fake consumes the same bytes at the
// same real-time pace as oto but discards them, so the test never opens a sound
// card and never leaks audio. The backend is still a real Device behind the
// same registry, so the wiring under test is unchanged.
func TestIntegrationQueuePlaysInOrder(t *testing.T) {
	cfg := testConfig()
	cfg.Backend = "fake"
	cfg.EventBuffer = 128
	s := newSession(t, cfg)

	a, b := fixture(t, "stereo_2s.opus"), fixture(t, "mono_1s.opus")
	if err := s.PlayQueue([]string{a, b}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}

	start := time.Now()

	// Wait for the queue to reach the second track. The fake backend never
	// reports ErrAudioInit, so a Failed event here is a real failure.
	deadline := time.After(20 * time.Second)
	advanced := false
	for !advanced {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("events channel closed early")
			}
			switch ev := ev.(type) {
			case TrackChanged:
				if ev.Index == 1 {
					advanced = true
				}
			case Failed:
				t.Fatalf("playback failed: %v", ev.Err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the queue to advance")
		}
	}

	waitState(t, s, StateStopped, 15*time.Second)
	elapsed := time.Since(start)

	// The two fixtures are 2 s and 1 s of audio. Real playback cannot finish
	// faster than the media, which is what makes this a playback test and not
	// a stub.
	want := 3 * time.Second
	t.Logf("end-to-end queue playback: %v wall clock for %v of audio", elapsed.Round(time.Millisecond), want)
	if elapsed < want-250*time.Millisecond {
		t.Fatalf("queue finished in %v, too fast for %v of audio", elapsed, want)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("queue took %v, far past expectations", elapsed)
	}

	if got := s.Snapshot().QueueIndex; got != 1 {
		t.Fatalf("QueueIndex = %d, want the last track 1", got)
	}
	if got := s.Snapshot().Stats.Underruns; got > 2 {
		t.Fatalf("stream reported %d underruns, want at most a couple at the track boundary", got)
	}
}

// TestIntegrationDeviceIsReusedAcrossRealTracks checks the whole point of the
// fixed format: the sink is opened once, not per track. It uses a factory that
// builds fake devices and counts them, so the proof holds without opening a
// sound card or playing the fixtures aloud.
func TestIntegrationDeviceIsReusedAcrossRealTracks(t *testing.T) {
	count := &countingFactory{}
	cfg := testConfig()
	cfg.Backend = "fake"
	cfg.EventBuffer = 128
	cfg.newDevice = count.new
	s := newSession(t, cfg)

	a, b := fixture(t, "short_stereo.opus"), fixture(t, "mono_1s.opus")
	if err := s.PlayQueue([]string{a, b}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for s.Snapshot().QueueIndex != 1 {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("events channel closed early")
			}
			if f, ok := ev.(Failed); ok {
				t.Fatalf("playback failed: %v", f.Err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the queue to advance")
		}
	}

	if got := count.created(); got != 1 {
		t.Fatalf("the queue built %d devices across a track change, want 1", got)
	}
}

// countingFactory records how many devices the session constructed. It opens
// through the same registry the engine uses, so pointing it at the fake backend
// keeps the count meaningful without touching a sound card. The control
// goroutine is the only caller, but the mutex keeps it race-clean under the
// race detector if that ever changes.
type countingFactory struct {
	mu   sync.Mutex
	made []playback.Device
}

func (f *countingFactory) new(backend string) (playback.Device, error) {
	d, err := playback.Open(backend)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.made = append(f.made, d)
	f.mu.Unlock()

	return d, nil
}

func (f *countingFactory) created() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.made)
}
