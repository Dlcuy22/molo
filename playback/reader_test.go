package playback

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedProvider is a Provider whose reads are scripted per call. It is the
// whole reason the adapter is testable without an audio device: it can emit a
// zero-length read, block, fail, or end on command.
type scriptedProvider struct {
	mu    sync.Mutex
	calls int
	steps []readStep
	block chan struct{}
}

type readStep struct {
	frames []float32
	err    error
}

func (p *scriptedProvider) ReadFrames(dst []float32) (int, error) {
	p.mu.Lock()
	p.calls++
	var step readStep
	if len(p.steps) > 0 {
		step = p.steps[0]
		p.steps = p.steps[1:]
	}
	block := p.block
	p.mu.Unlock()

	if block != nil {
		<-block
	}

	n := copy(dst, step.frames)

	return n / 2, step.err
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls
}

func (p *scriptedProvider) unblock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.block)
}

func decodeFloat32LE(t *testing.T, b []byte) []float32 {
	t.Helper()

	if len(b)%4 != 0 {
		t.Fatalf("blob length %d is not a multiple of 4", len(b))
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}

	return out
}

func TestPCMReaderEncodesLittleEndianFloat32(t *testing.T) {
	want := []float32{1, -0.5, 0.25, -1.5}
	p := &scriptedProvider{steps: []readStep{{frames: want}}}
	r := newPCMReader(p, 2)

	buf := make([]byte, len(want)*4)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != len(want)*4 {
		t.Fatalf("Read = %d bytes, want %d", n, len(want)*4)
	}
	got := decodeFloat32LE(t, buf)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPCMReaderNeverReturnsZeroNoError(t *testing.T) {
	// A zero-length read with a nil error would make oto's multiplexer spin.
	// The adapter must keep pulling until the provider has something.
	p := &scriptedProvider{steps: []readStep{
		{},
		{},
		{frames: []float32{0.5, 0.5}},
	}}
	r := newPCMReader(p, 2)

	buf := make([]byte, 2*4)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != 2*4 {
		t.Fatalf("Read = %d bytes, want %d", n, 2*4)
	}
	if got := p.callCount(); got < 3 {
		t.Fatalf("provider called %d times, want at least 3 after two empty reads", got)
	}
}

func TestPCMReaderBlocksOnEmptyProviderInsteadOfReturning(t *testing.T) {
	// A provider that returns (0, nil) and refuses to block on its own must not
	// turn into a (0, nil) to oto. The adapter has to wait for the provider.
	p := &scriptedProvider{
		steps: []readStep{{frames: []float32{1, 1}}},
		block: make(chan struct{}),
	}
	r := newPCMReader(p, 2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2*4)
		if _, err := r.Read(buf); err != nil {
			t.Errorf("Read: %v", err)
		}
	}()

	select {
	case <-done:
		t.Fatal("Read returned while the provider was still blocked")
	case <-time.After(50 * time.Millisecond):
	}

	p.unblock()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("Read did not return after the provider was unblocked")
	}
}

func TestPCMReaderReturnsEOFatStreamEnd(t *testing.T) {
	p := &scriptedProvider{steps: []readStep{{err: io.EOF}}}
	r := newPCMReader(p, 2)

	buf := make([]byte, 2*4)
	n, err := r.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read error = %v, want io.EOF", err)
	}
	if n != 0 {
		t.Fatalf("Read = %d bytes, want 0", n)
	}
}

func TestPCMReaderPropagatesStreamError(t *testing.T) {
	sentinel := errors.New("boom")
	p := &scriptedProvider{steps: []readStep{{err: sentinel}}}
	r := newPCMReader(p, 2)

	buf := make([]byte, 2*4)
	_, err := r.Read(buf)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Read error = %v, want %v", err, sentinel)
	}
}

func TestPCMReaderDeliversFramesBeforeTerminalError(t *testing.T) {
	// A short read may legally carry the end-of-stream error; the frames must
	// reach oto and the error must wait for the next call.
	p := &scriptedProvider{steps: []readStep{
		{frames: []float32{0.1, 0.1}, err: io.EOF},
	}}
	r := newPCMReader(p, 2)

	buf := make([]byte, 2*4)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("first Read error = %v, want nil (frames were delivered)", err)
	}
	if n != 2*4 {
		t.Fatalf("first Read = %d bytes, want %d", n, 2*4)
	}
	if _, err := r.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("second Read error = %v, want io.EOF", err)
	}
}

func TestPCMReaderCloseStopsReading(t *testing.T) {
	p := &scriptedProvider{steps: []readStep{{frames: []float32{1, 1}}}}
	r := newPCMReader(p, 2)
	r.close()

	buf := make([]byte, 2*4)
	n, err := r.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read after close error = %v, want io.EOF", err)
	}
	if n != 0 {
		t.Fatalf("Read after close = %d bytes, want 0", n)
	}
	if got := p.callCount(); got != 0 {
		t.Fatalf("provider was called %d times after close, want 0", got)
	}
}

func TestPCMReaderCloseIsIdempotent(t *testing.T) {
	r := newPCMReader(&scriptedProvider{}, 2)
	r.close()
	r.close()

	if !r.closed.Load() {
		t.Fatal("closed flag is false after close")
	}
}

func TestPCMReaderZeroLengthBufferIsAllowed(t *testing.T) {
	p := &scriptedProvider{}
	r := newPCMReader(p, 2)

	n, err := r.Read(nil)
	if n != 0 || err != nil {
		t.Fatalf("Read(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestPCMReaderNeverWritesPastTheBuffer(t *testing.T) {
	// oto may hand a slice whose length is not a whole number of frames. The
	// adapter must round down and never write a partial frame.
	want := []float32{1, 2, 3, 4}
	p := &scriptedProvider{steps: []readStep{{frames: want}}}
	r := newPCMReader(p, 2)

	buf := make([]byte, 3*4) // one and a half stereo frames
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != 2*4 {
		t.Fatalf("Read = %d bytes, want %d (one whole frame)", n, 2*4)
	}
	if got := decodeFloat32LE(t, buf[:n]); got[0] != 1 || got[1] != 2 {
		t.Fatalf("Read produced %v, want [1 2]", got)
	}
}

func TestPCMReaderCloseUnblocksAProviderThatCannotReturn(t *testing.T) {
	// The adapter checks the closed flag every iteration, so a provider that
	// keeps reporting (0, nil) cannot strand the oto read loop once the device
	// is closed.
	var calls atomic.Int64
	p := providerFunc(func(dst []float32) (int, error) {
		calls.Add(1)

		return 0, nil
	})
	r := newPCMReader(p, 2)
	go r.close()

	deadline := timeoutAfter()
	for {
		select {
		case <-deadline:
			t.Fatal("Read never returned after close with a (0, nil) provider")
		default:
		}
		buf := make([]byte, 2*4)
		if _, err := r.Read(buf); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("Read error = %v, want io.EOF", err)
		}
	}
}

// providerFunc adapts a function to Provider for the odd-shaped tests.
type providerFunc func(dst []float32) (int, error)

func (f providerFunc) ReadFrames(dst []float32) (int, error) { return f(dst) }

// timeoutAfter bounds a wait so a hang fails the test instead of hanging the
// suite, which matters because the audited behaviours are all "must not spin
// forever" cases.
func timeoutAfter() <-chan time.Time { return time.After(2 * time.Second) }
