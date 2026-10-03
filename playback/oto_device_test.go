//go:build !freebsd && !android && !ios

package playback

import (
	"errors"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
)

var deviceFormat = core.FrameFormat{Rate: deviceRate, Ch: deviceChannels, Fmt: core.F32}

// silenceProvider produces a fixed number of frames and then reports end of
// stream. It never blocks, so it is safe on every close path. The counter is
// atomic because oto reads from its own goroutine while tests observe progress.
type silenceProvider struct {
	total int
	done  atomic.Int64
	value float32
}

func newSilenceProvider(total int, value float32) *silenceProvider {
	return &silenceProvider{total: total, value: value}
}

func (p *silenceProvider) ReadFrames(dst []float32) (int, error) {
	done := int(p.done.Load())
	if done >= p.total {
		return 0, io.EOF
	}
	frames := min(len(dst)/deviceChannels, p.total-done)
	for i := range dst[:frames*deviceChannels] {
		dst[i] = p.value
	}
	p.done.Add(int64(frames))

	return frames, nil
}

func (p *silenceProvider) framesRead() int64 { return p.done.Load() }

// openOtoDevice opens a device against a Provider, skipping the test with a
// visible reason when this machine has no audio server. The skip is deliberate
// and logged: a silent pass would hide that no audio path was exercised.
func openOtoDevice(t *testing.T, p Provider) *otoDevice {
	t.Helper()

	d, err := Open(otoBackendName)
	if err != nil {
		t.Fatalf("Open(%q): %v", otoBackendName, err)
	}
	dev, ok := d.(*otoDevice)
	if !ok {
		t.Fatalf("backend %q returned %T", otoBackendName, d)
	}
	if err := dev.Open(deviceFormat, p); err != nil {
		if errors.Is(err, ErrAudioInit) {
			t.Skipf("real audio unavailable, skipping: %v", err)
		}
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	return dev
}

func TestOtoRegistryExposesTheBackend(t *testing.T) {
	if !slicesContains(Names(), otoBackendName) {
		t.Fatalf("Names() = %v, missing %q", Names(), otoBackendName)
	}
}

func TestOtoOpenRejectsNilProvider(t *testing.T) {
	dev := newOtoDevice().(*otoDevice)
	if err := dev.Open(deviceFormat, nil); err == nil {
		t.Fatal("Open accepted a nil Provider")
	}
}

func TestOtoOpenRejectsForeignFormat(t *testing.T) {
	// Format validation runs before the driver is touched, so this stays
	// testable on a machine with no audio server.
	bad := []core.FrameFormat{
		{Rate: 44100, Ch: 2, Fmt: core.F32},
		{Rate: 48000, Ch: 1, Fmt: core.F32},
		{Rate: 48000, Ch: 2, Fmt: core.S16},
	}
	for _, f := range bad {
		dev := newOtoDevice().(*otoDevice)
		err := dev.Open(f, newSilenceProvider(0, 0))
		if !errors.Is(err, ErrUnsupportedFormat) {
			t.Fatalf("Open(%+v) error = %v, want ErrUnsupportedFormat", f, err)
		}
	}
}

func TestOtoStateErrorsBeforeOpen(t *testing.T) {
	dev := newOtoDevice().(*otoDevice)
	if err := dev.Start(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Start before Open = %v, want ErrNotOpen", err)
	}
	if err := dev.Pause(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Pause before Open = %v, want ErrNotOpen", err)
	}
	if err := dev.Resume(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Resume before Open = %v, want ErrNotOpen", err)
	}
	if got := dev.Latency(); got != 0 {
		t.Fatalf("Latency before Open = %v, want 0", got)
	}
}

func TestOtoCloseWithoutOpenIsInert(t *testing.T) {
	dev := newOtoDevice().(*otoDevice)
	if err := dev.Close(); err != nil {
		t.Fatalf("Close before Open: %v", err)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := dev.Start(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}
}

func TestOtoDoubleCloseAfterPlayingIsIdempotent(t *testing.T) {
	// The idempotence path that matters is Close while playing followed by
	// another Close: the first tears the player down, the second must not
	// touch a released handle.
	dev := openOtoDevice(t, newSilenceProvider(deviceRate/2, 0.01))
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	if err := dev.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := dev.Latency(); got != 0 {
		t.Fatalf("Latency after close = %v, want 0", got)
	}
}

func TestOtoDoubleOpenIsRejected(t *testing.T) {
	dev := openOtoDevice(t, newSilenceProvider(4800, 0))
	if err := dev.Open(deviceFormat, newSilenceProvider(4800, 0)); !errors.Is(err, ErrOpen) {
		t.Fatalf("second Open = %v, want ErrOpen", err)
	}
}

func TestOtoLifecycle(t *testing.T) {
	dev := openOtoDevice(t, newSilenceProvider(deviceRate/2, 0.01))

	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := dev.Start(); err != nil {
		t.Fatalf("redundant Start: %v", err)
	}
	eventually(t, 2*time.Second, "audio to be queued", func() bool { return dev.Latency() > 0 })

	if err := dev.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := dev.Pause(); err != nil {
		t.Fatalf("redundant Pause: %v", err)
	}
	if err := dev.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := dev.Resume(); err != nil {
		t.Fatalf("redundant Resume: %v", err)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOtoCloseWithoutStart(t *testing.T) {
	dev := openOtoDevice(t, newSilenceProvider(deviceRate, 0))
	if err := dev.Close(); err != nil {
		t.Fatalf("Close without Start: %v", err)
	}
}

func TestOtoCloseWhilePlaying(t *testing.T) {
	// Two seconds of audio, closed after a few milliseconds: the device must
	// not panic and must stop pulling from the Provider.
	p := newSilenceProvider(deviceRate*2, 0.01)
	dev := openOtoDevice(t, p)
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- dev.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close while playing: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close while playing did not return; the reader is stuck")
	}
}

func TestOtoFlushDropsQueuedAudio(t *testing.T) {
	// The bug this covers: Pause keeps what oto already read, so without a flush
	// the resume after a seek replays up to a buffer of the old position. The
	// observable, non-audio proof is that the queued latency goes to zero and
	// that the Provider is not asked for more while the device is parked.
	dev := openOtoDevice(t, newSilenceProvider(deviceRate*3, 0.01))
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, 2*time.Second, "the queue to fill", func() bool { return dev.Latency() > 0 })

	if err := dev.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	queued := dev.Latency()
	if queued == 0 {
		t.Fatal("Pause dropped the queue by itself; the test would not prove Flush does anything")
	}

	before := dev.src.provider.(*silenceProvider).framesRead()
	if err := dev.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := dev.Latency(); got != 0 {
		t.Fatalf("Latency after Flush = %v, want 0 (was %v)", got, queued)
	}

	time.Sleep(30 * time.Millisecond)
	if after := dev.src.provider.(*silenceProvider).framesRead(); after != before {
		t.Fatalf("Flush pulled %d more frames; it must not read the Provider", after-before)
	}

	// Flushing a parked device must not start playback.
	if err := dev.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestOtoFlushBeforeOpenIsInert(t *testing.T) {
	dev := newOtoDevice().(*otoDevice)
	if err := dev.Flush(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Flush before Open = %v, want ErrNotOpen", err)
	}
}

func TestOtoFlushAfterCloseIsRejected(t *testing.T) {
	dev := openOtoDevice(t, newSilenceProvider(deviceRate/4, 0))
	if err := dev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := dev.Flush(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Flush after Close = %v, want ErrClosed", err)
	}
}

func TestOtoLatencyTracksTheQueue(t *testing.T) {
	// 3 seconds of audio at unity so oto fills its buffer. The queue must show
	// up as a positive, bounded latency; unbounded would mean the field is
	// meaningless.
	dev := openOtoDevice(t, newSilenceProvider(deviceRate*3, 0.01))
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, 2*time.Second, "latency above zero", func() bool { return dev.Latency() > 0 })
	if got := dev.Latency(); got > 3*time.Second {
		t.Fatalf("Latency = %v, want under the queued stream length", got)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := dev.Latency(); got != 0 {
		t.Fatalf("Latency after Close = %v, want 0", got)
	}
}

func TestOtoPauseStopsConsumingTheProvider(t *testing.T) {
	// PauseAndStopReading, not Pause, is what makes this true: a paused player
	// must stop advancing the stream instead of buffering it in the background.
	p := newSilenceProvider(deviceRate*10, 0.01)
	dev := openOtoDevice(t, p)
	if err := dev.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, 2*time.Second, "the first reads", func() bool { return p.framesRead() > 0 })

	if err := dev.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	settled := p.framesRead()
	time.Sleep(100 * time.Millisecond)
	if got := p.framesRead(); got != settled {
		t.Fatalf("provider advanced from %d to %d frames while paused", settled, got)
	}
}

func TestOtoCloseDoesNotLeakGoroutines(t *testing.T) {
	// Warm up the shared context first: its mux loop goroutine belongs to the
	// context, not to any one device, so it must not be counted as a leak.
	warm := openOtoDevice(t, newSilenceProvider(deviceRate/4, 0))
	if err := warm.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for range 8 {
		dev := openOtoDevice(t, newSilenceProvider(deviceRate/4, 0.01))
		if err := dev.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
		if err := dev.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	eventually(t, 3*time.Second, "goroutines to settle", func() bool {
		runtime.GC()

		return runtime.NumGoroutine() <= before+1
	})
}

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

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}
