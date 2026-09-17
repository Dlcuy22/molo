package tui

import (
	"sync"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/meta"
)

// fakePlayer is a player.Player that never touches audio or the file system.
// It lets the model's key handling, event translation and rendering be driven
// from a unit test with no terminal and no engine.
type fakePlayer struct {
	mu     sync.Mutex
	snap   player.Snapshot
	events chan player.Event
	queue  []string
	calls  []string
	tap    *fakeTap
	closed bool
}

func newFakePlayer() *fakePlayer {
	return &fakePlayer{
		events: make(chan player.Event, 64),
		snap:   player.Snapshot{Volume: 1, QueueIndex: -1},
		tap:    &fakeTap{},
	}
}

func (f *fakePlayer) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakePlayer) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.calls {
		if c == call {
			return true
		}
	}

	return false
}

func (f *fakePlayer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

func (f *fakePlayer) Snapshot() player.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.snap
}

func (f *fakePlayer) Events() <-chan player.Event { return f.events }

func (f *fakePlayer) emit(ev player.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- ev
}

func (f *fakePlayer) Play(path string) error { return f.PlayQueue([]string{path}) }

func (f *fakePlayer) PlayQueue(paths []string) error {
	f.mu.Lock()
	f.record("PlayQueue")
	f.queue = append([]string(nil), paths...)
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Next() error {
	f.mu.Lock()
	f.record("Next")
	f.advanceLocked(1)
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Prev() error {
	f.mu.Lock()
	f.record("Prev")
	f.advanceLocked(-1)
	f.mu.Unlock()

	return nil
}

// advanceLocked moves the fake's current track within its queue so a next/prev
// in a test produces the track change and state a real engine would.
func (f *fakePlayer) advanceLocked(delta int) {
	if len(f.queue) == 0 {
		return
	}

	idx := f.snap.QueueIndex
	if idx < 0 {
		idx = 0
	} else {
		idx += delta
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(f.queue) {
		idx = len(f.queue) - 1
	}

	f.snap.QueueIndex = idx
	f.snap.QueueLen = len(f.queue)
	f.snap.Path = f.queue[idx]
	f.snap.Position = 0
	f.snap.Duration = 0
	f.snap.Meta = meta.Meta{}
	f.events <- player.TrackChanged{Index: idx, Path: f.queue[idx]}
}

func (f *fakePlayer) Queue() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.queue...)
}

func (f *fakePlayer) Pause() error {
	f.mu.Lock()
	f.record("Pause")
	f.snap.State = player.Paused
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Resume() error {
	f.mu.Lock()
	f.record("Resume")
	f.snap.State = player.Playing
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Stop() error {
	f.mu.Lock()
	f.record("Stop")
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Seek(d time.Duration) error {
	f.mu.Lock()
	f.record("Seek")
	f.snap.Position = d
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) SetVolume(v float64) {
	f.mu.Lock()
	f.record("SetVolume")
	f.snap.Volume = v
	f.mu.Unlock()
}

func (f *fakePlayer) Tap() player.Tap { return f.tap }

func (f *fakePlayer) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.events)

	return nil
}

func (f *fakePlayer) setSnapshot(fn func(*player.Snapshot)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.snap)
}

// fakeTap serves canned mono frames. Read is non-blocking by contract; this one
// is too.
type fakeTap struct {
	mu      sync.Mutex
	samples []float32
	reads   int
}

func (t *fakeTap) Read(dst []float32) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reads++

	return copy(dst, t.samples)
}

func (t *fakeTap) setSamples(s []float32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = append([]float32(nil), s...)
}

func (t *fakeTap) readCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.reads
}

var _ player.Player = (*fakePlayer)(nil)
