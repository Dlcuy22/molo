// Fake playback backend.
//
// The engine must be testable without a sound device: CI has no audio server,
// and a test that opens a real card can fail on the machine of whoever runs it.
// This backend consumes the Provider on its own goroutine exactly like oto does
// and discards the samples, so the whole engine above it runs unchanged with
// zero audio leakage. It is also the "print instead of hear" option: given a
// writer it reports each block it would have played.

package playback

import (
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlcuy22/molo/core"
)

// fakeBackendName is the registry key. It is not build-tagged: the fake is
// platform-independent, which is what makes it usable on every CI runner. It
// registers as a dev backend, so it stays selectable by name and by tests but
// never appears in a UI's output list.
const fakeBackendName = "fake"

// fakeDefaultPace is the wall-clock duration of one block. A background device
// that reads as fast as the Provider answers would spin a core and let a stream
// finish in microseconds instead of real time, so reads are paced by default.
const fakeDefaultPace = 100 * time.Millisecond

type fakeState uint8

const (
	fakeStateIdle fakeState = iota
	fakeStateOpen
	fakeStatePlaying
	fakeStatePaused
	fakeStateClosed
)

// FakeDevice is the test implementation of Device. The exported type is
// deliberate: tests outside this package need to reach SetPace and the
// counters without a type assertion on an unexported name.
type FakeDevice struct {
	mu    sync.Mutex
	state fakeState
	wake  chan struct{}
	// stop asks the read loop to end; exited is closed by the loop itself as
	// its last act. They must be separate channels: Close has to wait for the
	// loop to finish, and waiting on the same channel it just closed would
	// return immediately and let the loop keep writing after Close returned.
	stop   chan struct{}
	exited chan struct{}
	format core.FrameFormat
	buf    []float32
	out    io.Writer

	// pace is the wait between blocks, swapped atomically because SetPace may
	// be called from a test goroutine while the read loop is running.
	pace atomic.Int64

	// err is the sticky failure from the read loop. atomic.Pointer is required
	// because errors.Is and Err are polled from other goroutines.
	err atomic.Pointer[error]

	frames atomic.Int64
	blocks atomic.Int64
}

var _ Device = (*FakeDevice)(nil)

// NewFakeDevice returns a quiet fake. The backend factory uses it, so
// playback.Open("fake") hands out a device that needs no writer.
func NewFakeDevice() *FakeDevice { return newFakeDevice(nil) }

// NewFakeDeviceWithWriter returns a fake that prints one line per consumed
// block: index, frames, and that block's peak and RMS in dBFS. It is the
// "print the changes instead of hearing them" mode for manually inspecting a
// pipeline without a card.
func NewFakeDeviceWithWriter(w io.Writer) *FakeDevice { return newFakeDevice(w) }

func newFakeDevice(w io.Writer) *FakeDevice {
	d := &FakeDevice{state: fakeStateIdle, out: w}
	d.pace.Store(int64(fakeDefaultPace))

	return d
}

func init() {
	RegisterDev(fakeBackendName, func() Device { return NewFakeDevice() })
}

// SetPace sets the wall-clock duration of one block. Zero disables the wait so
// tests run at full speed; the default paces reads at real time so a long
// stream does not finish instantly.
func (d *FakeDevice) SetPace(p time.Duration) {
	if p < 0 {
		p = 0
	}
	d.pace.Store(int64(p))
}

// Frames reports how many frames the device has consumed. It is monotonic
// across Pause and Resume and is used by tests to observe progress.
func (d *FakeDevice) Frames() int64 { return d.frames.Load() }

// Blocks reports how many Provider reads have completed.
func (d *FakeDevice) Blocks() int64 { return d.blocks.Load() }

// Open binds the device to a format and a Provider. The format check mirrors
// oto: the engine only ever opens the canonical layout, so anything else is a
// bug above this layer and is rejected rather than resampled.
func (d *FakeDevice) Open(f core.FrameFormat, p Provider) error {
	if p == nil {
		return fmt.Errorf("playback/%s: Open needs a provider", fakeBackendName)
	}
	if f.Rate != core.CanonicalFormat.Rate || f.Ch != core.CanonicalFormat.Ch || f.Fmt != core.CanonicalFormat.Fmt {
		return fmt.Errorf("%w: got %+v", ErrUnsupportedFormat, f)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case fakeStateClosed:
		return ErrClosed
	case fakeStateIdle:
	default:
		return ErrOpen
	}

	// The buffer is allocated once, here, and never in the read loop: a
	// post-ring backend must not allocate on the audio path, and the fake
	// exists to exercise exactly that discipline. The 100 ms size is derived
	// from the format rather than restated so a rate change cannot leave the
	// block out of step with the engine.
	d.buf = make([]float32, f.Rate/10*f.Ch)
	d.format = f
	d.wake = make(chan struct{}, 1)
	d.stop = make(chan struct{})
	d.exited = make(chan struct{})
	d.state = fakeStateOpen

	go d.run(p)

	return nil
}

// run is the read loop, the fake's stand-in for oto's mux and audio threads.
// It starts at Open so the goroutine outlives a Pause, which is what lets
// Resume continue from where consumption stopped.
func (d *FakeDevice) run(p Provider) {
	// exited is closed on every return path, and it is the last thing the loop
	// does, so a Close that waits on it is a true barrier over the writer and
	// the frame counters.
	defer close(d.exited)

	for {
		if !d.waitForPlay() {
			d.runExit()

			return
		}

		d.mu.Lock()
		buf, format, out := d.buf, d.format, d.out
		d.mu.Unlock()

		// The read happens with no lock held. A locked lock would serialize
		// Pause and Close behind a Provider read that blocks until the stream
		// is live, and those two must stay free to run on other goroutines.
		n, err := p.ReadFrames(buf)

		// Close may have landed while that read was in flight. Checking stop
		// before the accounting is what makes Close a hard stop: frames a
		// closed device happened to pull are discarded, so Frames cannot grow
		// after Close returns.
		select {
		case <-d.stop:
			d.runExit()

			return
		default:
		}
		if n > 0 {
			d.frames.Add(int64(n))
			d.blocks.Add(1)
			if out != nil {
				writeFakeBlock(out, d.blocks.Load(), buf[:n*format.Ch], format)
			}
		}
		if err != nil {
			if err != io.EOF {
				// io.EOF is the normal end of a stream, so only a real failure
				// is stored for Err to surface.
				err = fmt.Errorf("playback/%s: provider read: %w", fakeBackendName, err)
				d.err.Store(&err)
			}
			d.finish()
			d.runExit()

			return
		}
		// A short or empty read is the Provider's right when it has nothing
		// buffered. Yielding here keeps that case from spinning the CPU.
		if n == 0 {
			runtime.Gosched()

			continue
		}

		// Pace by the audio just consumed, not by a fixed block, so a short
		// read does not make the stream run faster than real time. Ending the
		// wait on stop makes Close prompt on a device that is playing.
		if pace := time.Duration(d.pace.Load()); pace > 0 {
			wait := pace * time.Duration(n) / time.Duration(len(buf)/format.Ch)
			select {
			case <-time.After(wait):
			case <-d.stop:
				d.runExit()

				return
			}
		}
	}
}

// runExit drops the loop's references on every exit path. Close waits for the
// goroutine to return and then treats that return as proof the loop is gone,
// including the buffer and the writer it was handed.
func (d *FakeDevice) runExit() {
	d.mu.Lock()
	d.buf, d.out = nil, nil
	d.mu.Unlock()
}

// finish parks the loop after the Provider is exhausted. End of stream leaves
// the device open so a later Resume cannot replay it from the start, and any
// goroutine parked in waitForPlay is woken to observe that state.
func (d *FakeDevice) finish() {
	d.mu.Lock()
	if d.state != fakeStateClosed {
		d.state = fakeStateOpen
	}
	wake := d.wake
	d.mu.Unlock()
	signal(wake)
}

// waitForPlay blocks until at least one frame should be consumed. It reports
// false once the device is closed. Pausing makes the loop wait on the wake
// channel so nothing advances while the test expects silence.
func (d *FakeDevice) waitForPlay() bool {
	for {
		d.mu.Lock()
		if d.state == fakeStateClosed {
			d.mu.Unlock()

			return false
		}
		if d.state == fakeStatePlaying {
			d.mu.Unlock()

			return true
		}
		wake := d.wake
		d.mu.Unlock()

		select {
		case <-wake:
		case <-d.stop:
			return false
		}
	}
}

// Start begins consuming the Provider. It is a no-op on a device that is
// already playing, and it never restarts one that has reached end of stream:
// Start's contract is to not skip audio, not to replay from the beginning.
func (d *FakeDevice) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case fakeStateClosed:
		return ErrClosed
	case fakeStateIdle:
		return ErrNotOpen
	case fakeStatePlaying:
		return nil
	}
	d.state = fakeStatePlaying
	signal(d.wake)

	return nil
}

// Pause stops playback and stops consuming the Provider. The read loop is
// parked rather than cancelled, so the one read already in flight finishes and
// is accounted for before consumption halts. A device that was never started
// stays open and is left alone.
func (d *FakeDevice) Pause() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case fakeStateClosed:
		return ErrClosed
	case fakeStateIdle:
		return ErrNotOpen
	case fakeStateOpen, fakeStatePaused:
		return nil
	}
	d.state = fakeStatePaused

	return nil
}

// Resume continues after Pause. On an open-but-never-started device it is a
// no-op rather than an implicit Start, matching oto.
func (d *FakeDevice) Resume() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case fakeStateClosed:
		return ErrClosed
	case fakeStateIdle:
		return ErrNotOpen
	case fakeStateOpen, fakeStatePlaying:
		return nil
	}
	d.state = fakeStatePlaying
	signal(d.wake)

	return nil
}

// Flush is a no-op. Flush exists to drop audio a backend has queued but not
// yet played; the fake queues nothing, because each Provider pull is counted
// and discarded immediately, so there is never anything left to discard.
func (d *FakeDevice) Flush() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case fakeStateClosed:
		return ErrClosed
	case fakeStateIdle:
		return ErrNotOpen
	}

	return nil
}

// Close releases the device. It is idempotent and safe from any goroutine,
// including while audio is playing. It signals the read loop to stop and waits
// for it to actually exit before returning, so the Provider and the writer are
// not touched again afterwards, which is what lets a caller close or reuse the
// stream. A Provider that never returns would make the wait unbounded; that
// matches oto, whose Close also waits out the in-flight read.
func (d *FakeDevice) Close() error {
	d.mu.Lock()
	if d.state == fakeStateClosed {
		d.mu.Unlock()

		return nil
	}
	d.state = fakeStateClosed
	stop, exited := d.stop, d.exited
	d.mu.Unlock()

	if stop == nil {
		// Never opened, so there is no loop to stop. Close still records the
		// state so a later Open reports ErrClosed.
		return nil
	}
	close(stop)
	// Waiting for exited is what makes Close a barrier: the loop closes it as
	// its last act, after its final write and counter update, so the receive
	// orders all of that before Close returns to the caller.
	<-exited

	return nil
}

// Latency is zero: the fake counts and drops every frame in the same step, so
// there is never audio accepted but not yet played.
func (d *FakeDevice) Latency() time.Duration { return 0 }

// Err surfaces a failure from the read loop. The Provider's error has no
// synchronous call site, so this poll is the only way it reaches a caller.
// End of stream is not an error.
func (d *FakeDevice) Err() error {
	if p := d.err.Load(); p != nil {
		return *p
	}

	return nil
}

// signal offers to wake a waiter without ever blocking. The buffer is one deep
// and a redundant wakeup is harmless, so a full channel is dropped.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// writeFakeBlock prints one consumed block: its index, frame count, and peak
// and RMS in dBFS. This is the inspection path, off the hot loop for the quiet
// default, so a formatting allocation is fine here.
func writeFakeBlock(w io.Writer, index int64, samples []float32, f core.FrameFormat) {
	var sumSq float64
	peak := 0.0
	for _, s := range samples {
		a := math.Abs(float64(s))
		if a > peak {
			peak = a
		}
		sumSq += float64(s) * float64(s)
	}
	rms := 0.0
	if len(samples) > 0 {
		rms = math.Sqrt(sumSq / float64(len(samples)))
	}
	frames := len(samples) / f.Ch

	fmt.Fprintf(w, "fake: block %d frames=%d peak=%.1f dBFS rms=%.1f dBFS\n",
		index, frames, dbfs(peak), dbfs(rms))
}

// dbfs converts a linear amplitude to dBFS, clamped to a sane floor so silence
// prints as -inf-free text a test can match.
func dbfs(amplitude float64) float64 {
	if amplitude <= 0 {
		return -120
	}

	return 20 * math.Log10(amplitude)
}
