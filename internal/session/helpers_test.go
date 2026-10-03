package session

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/meta"
	"github.com/dlcuy22/molo/playback"
)

// canonicalFormat is the one format the engine speaks. Tests pin it independently so
// a change to the session's own constant cannot silently agree with itself.
var canonicalFormat = core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}

// fixture resolves a Phase 1 fixture. The media bytes are shared with decode,
// stream and playback rather than duplicated per package.
func fixture(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("..", "..", "decode", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s is missing: %v", name, err)
	}

	return path
}

// orderLog records the order in which engine operations happen. The seek test
// uses it to prove the device is paused before the streamer is repositioned.
type orderLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *orderLog) add(op string) {
	l.mu.Lock()
	l.ops = append(l.ops, op)
	l.mu.Unlock()
}

func (l *orderLog) reset() {
	l.mu.Lock()
	l.ops = nil
	l.mu.Unlock()
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.ops...)
}

// recordingDevice is a Device that consumes nothing. It lets tests drive the
// session state machine without the timing a real consumer introduces, and it
// records the lifecycle calls the seek gate asserts on.
type recordingDevice struct {
	mu      sync.Mutex
	log     *orderLog
	opened  core.FrameFormat
	starts  int
	pauses  int
	resumes int
	flushes int
	err     error
	closed  bool
}

func newRecordingDevice(log *orderLog) *recordingDevice {
	return &recordingDevice{log: log}
}

func (d *recordingDevice) Open(f core.FrameFormat, _ playback.Provider) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return playback.ErrClosed
	}
	d.opened = f

	return nil
}

func (d *recordingDevice) Start() error {
	d.mu.Lock()
	d.starts++
	d.mu.Unlock()

	return nil
}

func (d *recordingDevice) Pause() error {
	d.mu.Lock()
	d.pauses++
	d.mu.Unlock()
	if d.log != nil {
		d.log.add("pause")
	}

	return nil
}

func (d *recordingDevice) Resume() error {
	d.mu.Lock()
	d.resumes++
	d.mu.Unlock()
	if d.log != nil {
		d.log.add("resume")
	}

	return nil
}

func (d *recordingDevice) Flush() error {
	d.mu.Lock()
	d.flushes++
	d.mu.Unlock()
	if d.log != nil {
		d.log.add("flush")
	}

	return nil
}

func (d *recordingDevice) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()

	return nil
}

func (d *recordingDevice) Latency() time.Duration { return 0 }

func (d *recordingDevice) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.err
}

func (d *recordingDevice) setErr(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
}

// pumpDevice is a Device that consumes its Provider on its own goroutine, like
// a real push backend does. It gives the queue and end-of-track tests a real
// consumer without an audio server, and its Pause waits for an in-flight read
// exactly as the oto backend does.
type pumpDevice struct {
	mu       sync.Mutex
	cond     *sync.Cond
	provider playback.Provider
	log      *orderLog

	open    bool
	started bool
	paused  bool
	closed  bool
	reading bool
	err     error
}

func newPumpDevice(log *orderLog) *pumpDevice {
	d := &pumpDevice{log: log}
	d.cond = sync.NewCond(&d.mu)

	return d
}

func (d *pumpDevice) Open(f core.FrameFormat, p playback.Provider) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return playback.ErrClosed
	}
	if d.open {
		return playback.ErrOpen
	}
	d.provider = p
	d.open = true

	return nil
}

func (d *pumpDevice) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return playback.ErrClosed
	}
	if !d.open {
		return playback.ErrNotOpen
	}
	if d.started {
		return nil
	}
	d.started = true
	d.paused = false
	go d.loop()

	return nil
}

func (d *pumpDevice) loop() {
	buf := make([]float32, 4800*canonicalFormat.Ch)
	for {
		d.mu.Lock()
		for d.paused && !d.closed {
			d.cond.Wait()
		}
		if d.closed {
			d.mu.Unlock()

			return
		}
		p := d.provider
		d.reading = true
		d.mu.Unlock()

		if p == nil {
			d.mu.Lock()
			d.reading = false
			d.cond.Broadcast()
			d.mu.Unlock()

			continue
		}
		n, err := p.ReadFrames(buf)

		d.mu.Lock()
		d.reading = false
		d.cond.Broadcast()
		d.mu.Unlock()

		if err != nil {
			return
		}
		// Consume at real-time pace. Without this the device drains a whole
		// fixture in well under a millisecond, so a short track can finish
		// before a test has any chance to observe the Playing state, and
		// end-of-track races look like flakiness rather than a broken engine.
		if n > 0 {
			time.Sleep(time.Duration(n) * time.Second / time.Duration(canonicalFormat.Rate))
		}
	}
}

func (d *pumpDevice) Pause() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return playback.ErrClosed
	}
	if !d.open {
		return playback.ErrNotOpen
	}
	d.paused = true
	for d.reading {
		d.cond.Wait()
	}
	if d.log != nil {
		d.log.add("pause")
	}

	return nil
}

func (d *pumpDevice) Resume() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return playback.ErrClosed
	}
	if !d.open {
		return playback.ErrNotOpen
	}
	d.paused = false
	d.cond.Broadcast()
	if d.log != nil {
		d.log.add("resume")
	}

	return nil
}

// Flush models the real backend dropping the audio it had queued but not yet
// played. pumpDevice reads on demand rather than buffering ahead, so there is
// nothing to drop; recording the call is what lets a test pin the ordering.
func (d *pumpDevice) Flush() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return playback.ErrClosed
	}
	if !d.open {
		return playback.ErrNotOpen
	}
	if d.log != nil {
		d.log.add("flush")
	}

	return nil
}

func (d *pumpDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	d.paused = true
	d.cond.Broadcast()

	return nil
}

func (d *pumpDevice) Latency() time.Duration { return 0 }

func (d *pumpDevice) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.err
}

// fakeFactory builds one device per call and keeps them, so a test can prove
// how many devices a track change created.
type fakeFactory struct {
	mu          sync.Mutex
	made        []playback.Device
	lastBackend string
}

func (f *fakeFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.made)
}

func (f *fakeFactory) first() playback.Device {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.made) == 0 {
		return nil
	}

	return f.made[0]
}

// recorderFactory counts devices built from a template constructor.
type recorderFactory struct {
	fakeFactory
	build func() playback.Device
}

func (f *recorderFactory) new(backend string) (playback.Device, error) {
	d := f.build()
	f.mu.Lock()
	f.made = append(f.made, d)
	f.lastBackend = backend
	f.mu.Unlock()

	return d, nil
}

// toneDecoder is a synthetic source that never opens a file. Its frame count
// is large enough that a track does not end on its own, which makes the manual
// Next/Prev/Pause/Seek tests deterministic. SeekFrame writes to the shared
// log so the seek gate can be checked in order.
type toneDecoder struct {
	value   float32
	total   int64
	log     *orderLog
	mu      sync.Mutex
	pos     int64
	seekErr error
}

func (d *toneDecoder) Info() core.StreamInfo {
	return core.StreamInfo{Format: canonicalFormat, TotalFrames: d.total}
}

func (d *toneDecoder) ReadFrames(dst []float32) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pos >= d.total {
		return 0, io.EOF
	}
	frames := min(int64(len(dst)/canonicalFormat.Ch), d.total-d.pos)
	for i := range dst[:int(frames)*canonicalFormat.Ch] {
		dst[i] = d.value
	}
	d.pos += frames

	return int(frames), nil
}

func (d *toneDecoder) SeekFrame(frame int64) error {
	d.mu.Lock()
	d.pos = frame
	err := d.seekErr
	d.mu.Unlock()
	if d.log != nil {
		d.log.add("seek")
	}

	return err
}

func (d *toneDecoder) Close() error { return nil }

// testConfig returns a Config wired to synthetic seams. The caller overrides
// the pieces it cares about.
func testConfig() Config {
	return Config{
		Backend:     "oto",
		EventBuffer: 64,
		Volume:      1,
		Resolver:    meta.Default(),
	}
}

// settingsEqual compares two Settings. Settings carries a Pipeline, whose stage
// slices make the struct non-comparable, so a field-by-field comparison is the
// only way to tell whether a rejected update really changed nothing.
func settingsEqual(a, b Settings) bool {
	return a.Volume == b.Volume &&
		a.Decoder == b.Decoder &&
		a.Backend == b.Backend &&
		a.ProbeMode == b.ProbeMode &&
		reflect.DeepEqual(a.Pipeline, b.Pipeline)
}

// newSession starts a session and closes it when the test ends.
func newSession(t *testing.T, cfg Config) *Session {
	t.Helper()

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// eventually polls a predicate and fails the test rather than hanging.
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

func waitState(t *testing.T, s *Session, want State, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, "state "+stateName(want), func() bool { return s.Snapshot().State == want })
}

// waitTrackChanged waits for the TrackChanged event for one queue index,
// discarding the events that belong to earlier tracks.
func waitTrackChanged(t *testing.T, s *Session, index int, timeout time.Duration) TrackChanged {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events channel closed while waiting for TrackChanged %d", index)
			}
			if tc, ok := ev.(TrackChanged); ok && tc.Index == index {
				return tc
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for TrackChanged %d", index)

	return TrackChanged{}
}

// waitEvent returns the next event of type T, discarding the others. It fails
// the test instead of hanging when the event never arrives.
func waitEvent[T Event](t *testing.T, s *Session, timeout time.Duration) T {
	t.Helper()

	var zero T
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events channel closed while waiting for %T", zero)
			}
			if e, ok := ev.(T); ok {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %T", zero)
		}
	}
}

// staticProbe reports a fixed stream info, so duration handling is testable
// without touching a file.
func staticProbe(info core.StreamInfo) func(string, decode.ProbeOptions) (core.StreamInfo, error) {
	return func(string, decode.ProbeOptions) (core.StreamInfo, error) { return info, nil }
}
