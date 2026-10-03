package playback

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
)

// fakeFormat is the canonical layout the fake accepts, named here so the tests
// read as intent rather than a struct literal repeated six times.
var fakeFormat = core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}

// sineProvider yields a fixed number of frames of a sine and then io.EOF. It
// is the fake's stand-in for the streamer, and it is deterministic so a test
// can assert on the frame count.
type sineProvider struct {
	total  int
	done   int
	phase  float64
	blocks int
}

func (p *sineProvider) ReadFrames(dst []float32) (int, error) {
	if p.done >= p.total {
		return 0, io.EOF
	}
	frames := min(len(dst)/fakeFormat.Ch, p.total-p.done)
	for i := range frames {
		v := float32(math.Sin(p.phase))
		p.phase += 2 * math.Pi * 440 / float64(fakeFormat.Rate)
		for c := 0; c < fakeFormat.Ch; c++ {
			dst[i*fakeFormat.Ch+c] = v
		}
	}
	p.done += frames
	p.blocks++

	return frames, nil
}

// openFakeDevice opens an unthrottled fake against p and closes it on cleanup.
func openFakeDevice(t *testing.T, p Provider) *FakeDevice {
	t.Helper()

	d := NewFakeDevice()
	d.SetPace(0)
	if err := d.Open(fakeFormat, p); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	return d
}

// waitForFrames polls until the device has consumed at least want frames. The
// read loop runs on its own goroutine, so a test has to observe it rather than
// assume it has already run.
func waitForFrames(t *testing.T, d *FakeDevice, want int64) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for d.Frames() < want {
		if time.Now().After(deadline) {
			t.Fatalf("Frames() = %d, want at least %d", d.Frames(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFakeStartConsumesProviderAndReachesEOF(t *testing.T) {
	const total = 9600
	p := &sineProvider{total: total}
	d := openFakeDevice(t, p)

	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrames(t, d, total)

	if got := d.Frames(); got != total {
		t.Fatalf("Frames() = %d, want %d", got, total)
	}
	if got := d.Blocks(); got == 0 {
		t.Fatal("Blocks() = 0, want at least one consumed block")
	}
	if err := d.Err(); err != nil {
		t.Fatalf("Err() = %v after end of stream, want nil", err)
	}
}

func TestFakePauseStopsConsumptionAndResumeContinues(t *testing.T) {
	// The provider is effectively endless here; the point is to freeze and
	// unfreeze the loop, not to finish it.
	p := &countingProvider{total: math.MaxInt32}
	d := openFakeDevice(t, p)

	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrames(t, d, 1)

	if err := d.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// The frame count may still tick up while the in-flight read lands, so
	// settle first and then require the count to stay flat.
	time.Sleep(20 * time.Millisecond)
	frozen := d.Frames()
	time.Sleep(200 * time.Millisecond)
	if got := d.Frames(); got != frozen {
		t.Fatalf("Frames() advanced from %d to %d while paused", frozen, got)
	}

	if err := d.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitForFrames(t, d, frozen+1)
}

// countingProvider is an endless sine that never blocks, so a paused device
// has no reason to be handed more frames. It counts calls to prove the loop
// itself stopped, not just the frame counter.
type countingProvider struct {
	total int
	done  int
}

func (p *countingProvider) ReadFrames(dst []float32) (int, error) {
	if p.done >= p.total {
		return 0, io.EOF
	}
	frames := len(dst) / fakeFormat.Ch
	p.done += frames

	return frames, nil
}

func TestFakeCloseIsIdempotentAndSafeWhilePlaying(t *testing.T) {
	p := &countingProvider{total: math.MaxInt32}
	d := openFakeDevice(t, p)

	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrames(t, d, 1)

	if err := d.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Every operation after Close reports the closed contract rather than
	// touching the Provider again.
	if err := d.Start(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}
	if err := d.Pause(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Pause after Close = %v, want ErrClosed", err)
	}
	if err := d.Flush(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Flush after Close = %v, want ErrClosed", err)
	}
}

func TestFakeCloseStopsTheReadLoop(t *testing.T) {
	p := &countingProvider{total: math.MaxInt32}
	d := openFakeDevice(t, p)

	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrames(t, d, 1)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Close waits for the loop to stop, so the count is settled once it
	// returns and must not grow afterwards.
	settled := d.Frames()
	time.Sleep(50 * time.Millisecond)
	if got := d.Frames(); got != settled {
		t.Fatalf("Frames() advanced from %d to %d after Close", settled, got)
	}
}

func TestFakeClosedOperationsReturnErrClosed(t *testing.T) {
	d := NewFakeDevice()
	if err := d.Close(); err != nil {
		t.Fatalf("Close on an idle device: %v", err)
	}
	if err := d.Open(fakeFormat, &sineProvider{total: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Open after Close = %v, want ErrClosed", err)
	}
	if err := d.Resume(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Resume after Close = %v, want ErrClosed", err)
	}
}

func TestFakeUnopenedOperationsReturnErrNotOpen(t *testing.T) {
	d := NewFakeDevice()
	if err := d.Start(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Start before Open = %v, want ErrNotOpen", err)
	}
	if err := d.Pause(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Pause before Open = %v, want ErrNotOpen", err)
	}
	if err := d.Flush(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Flush before Open = %v, want ErrNotOpen", err)
	}
}

func TestFakeOpenRejectsBadInput(t *testing.T) {
	d := NewFakeDevice()
	if err := d.Open(fakeFormat, nil); err == nil {
		t.Fatal("Open accepted a nil Provider")
	}
	if err := d.Open(core.FrameFormat{Rate: 44100, Ch: 2, Fmt: core.F32}, &sineProvider{total: 1}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Open with a 44.1 kHz format = %v, want ErrUnsupportedFormat", err)
	}
	if err := d.Open(fakeFormat, &sineProvider{total: 1}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.Open(fakeFormat, &sineProvider{total: 1}); !errors.Is(err, ErrOpen) {
		t.Fatalf("second Open = %v, want ErrOpen", err)
	}
}

func TestFakeErrSurfacesProviderFailure(t *testing.T) {
	sentinel := errors.New("decode exploded")
	p := providerFunc(func([]float32) (int, error) { return 0, sentinel })
	d := openFakeDevice(t, p)

	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for d.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Err() stayed nil after the Provider failed")
		}
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(d.Err(), sentinel) {
		t.Fatalf("Err() = %v, want it to wrap %v", d.Err(), sentinel)
	}
}

func TestFakeRegistryExposesTheBackend(t *testing.T) {
	if !slicesContains(Names(), fakeBackendName) {
		t.Fatalf("Names() = %v, missing %q", Names(), fakeBackendName)
	}
	dev, err := Open(fakeBackendName)
	if err != nil {
		t.Fatalf("Open(%q): %v", fakeBackendName, err)
	}
	fake, ok := dev.(*FakeDevice)
	if !ok {
		t.Fatalf("Open(%q) returned %T", fakeBackendName, dev)
	}
	fake.SetPace(0)
	if err := fake.Open(fakeFormat, &sineProvider{total: 480}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := fake.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrames(t, fake, 480)
	if err := fake.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestFakeIsHiddenFromUserNames is the contract that keeps a dev sink out of a
// listener's output dropdown: it stays openable by name, but UserNames does
// not offer it. Both halves matter, so both are asserted.
func TestFakeIsHiddenFromUserNames(t *testing.T) {
	if !slicesContains(Names(), fakeBackendName) {
		t.Fatalf("Names() = %v, missing %q; a dev backend must stay selectable by name", Names(), fakeBackendName)
	}
	if slicesContains(UserNames(), fakeBackendName) {
		t.Fatalf("UserNames() = %v, must not offer the dev backend %q", UserNames(), fakeBackendName)
	}
}

// TestRegisterDevHidesOnlyDevBackends pins the flag to the entry, not the
// registry: a normal Register stays visible, and a dev one does not. A fake
// registry is used so the assertion cannot be broken by a future backend.
func TestRegisterDevHidesOnlyDevBackends(t *testing.T) {
	r := NewRegistry()
	r.Register("real", func() Device { return NewFakeDevice() })
	r.RegisterDev("dev", func() Device { return NewFakeDevice() })

	if got := r.Names(); !slicesEqual(got, []string{"dev", "real"}) {
		t.Fatalf("Names() = %v, want [dev real]", got)
	}
	if got := r.UserNames(); !slicesEqual(got, []string{"real"}) {
		t.Fatalf("UserNames() = %v, want [real]", got)
	}
	if _, err := r.Open("dev"); err != nil {
		t.Fatalf("Open(dev): %v; a dev backend must still open", err)
	}
}

func slicesEqual(a, b []string) bool {
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

func TestFakeLatencyIsZero(t *testing.T) {
	p := &countingProvider{total: math.MaxInt32}
	d := openFakeDevice(t, p)

	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrames(t, d, 1)
	if got := d.Latency(); got != 0 {
		t.Fatalf("Latency() = %v, want 0: the fake queues nothing", got)
	}
}

func TestFakeWriterPrintsConsumedBlocks(t *testing.T) {
	var buf bytes.Buffer
	d := NewFakeDeviceWithWriter(&buf)
	d.SetPace(0)
	if err := d.Open(fakeFormat, &sineProvider{total: 9600}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrames(t, d, 9600)
	// Close is the barrier that orders the loop's writes to buf before this
	// read. bytes.Buffer is not safe to read while the loop may still write.
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "block 1 frames=") {
		t.Fatalf("writer output missing a block line:\n%s", out)
	}
	if !strings.Contains(out, "dBFS") {
		t.Fatalf("writer output missing level in dBFS:\n%s", out)
	}
}
