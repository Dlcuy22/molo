package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/dsp"
)

// fakePlayer is a molo.Player that never touches audio or the file system.
// It exists so the CLI's key dispatch, event drain and exit-code logic can be
// exercised in a unit test.
type fakePlayer struct {
	mu       sync.Mutex
	snap     molo.Snapshot
	events   chan molo.Event
	queue    []string
	calls    []string
	settings molo.Settings
	closed   bool

	// fail makes PlayQueue report a playback failure instead of playing.
	fail error
	// autoEnd makes PlayQueue run through every track and stop, like a real
	// player reaching the end of its queue.
	autoEnd bool
	// endDelay is a pause between tracks, so the display has time to observe a
	// playing state when a test wants that.
	endDelay time.Duration
}

func newFakePlayer() *fakePlayer {
	return &fakePlayer{
		events: make(chan molo.Event, 64),
		snap:   molo.Snapshot{Volume: 1, QueueIndex: -1},
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

func (f *fakePlayer) Snapshot() molo.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.snap
}

func (f *fakePlayer) Events() <-chan molo.Event { return f.events }

func (f *fakePlayer) emit(ev molo.Event) {
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
	fail := f.fail
	autoEnd := f.autoEnd
	delay := f.endDelay
	f.mu.Unlock()

	if fail != nil {
		go func() {
			f.emit(molo.Failed{Err: fail})
			f.emit(molo.StateChanged{From: molo.Idle, To: molo.Stopped})
		}()

		return nil
	}

	if autoEnd {
		go f.playThrough(paths, delay)

		return nil
	}

	go func() {
		f.setSnapshot(func(s *molo.Snapshot) {
			s.State = molo.Playing
			s.Path = paths[0]
			s.QueueIndex = 0
			s.QueueLen = len(paths)
		})
		f.emit(molo.StateChanged{From: molo.Idle, To: molo.Playing})
		f.emit(molo.TrackChanged{Index: 0, Path: paths[0]})
	}()

	return nil
}

func (f *fakePlayer) playThrough(paths []string, delay time.Duration) {
	for i, path := range paths {
		f.setSnapshot(func(s *molo.Snapshot) {
			s.State = molo.Playing
			s.Path = path
			s.QueueIndex = i
			s.QueueLen = len(paths)
		})
		f.emit(molo.TrackChanged{Index: i, Path: path})
		if delay > 0 {
			time.Sleep(delay)
		}
		f.setSnapshot(func(s *molo.Snapshot) {
			s.Position = 0
			if i == len(paths)-1 {
				s.State = molo.Stopped
			}
		})
		f.emit(molo.TrackEnded{})
		if i == len(paths)-1 {
			f.emit(molo.StateChanged{From: molo.Playing, To: molo.Stopped})
		}
	}
}

func (f *fakePlayer) setSnapshot(fn func(*molo.Snapshot)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.snap)
}

func (f *fakePlayer) Next() error {
	f.mu.Lock()
	f.record("Next")
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) PlayIndex(index int) error {
	f.mu.Lock()
	f.record("PlayIndex")
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Prev() error {
	f.mu.Lock()
	f.record("Prev")
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) SetShuffle(on bool) error { return nil }

func (f *fakePlayer) Shuffled() bool { return false }

// InsertQueue records the call and splices the refs into the fake queue, so a
// test can assert where a "play next" landed.
func (f *fakePlayer) InsertQueue(index int, refs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("InsertQueue")

	at := index
	if at < 0 || at > len(f.queue) {
		at = len(f.queue)
	}
	next := make([]string, 0, len(f.queue)+len(refs))
	next = append(next, f.queue[:at]...)
	next = append(next, refs...)
	next = append(next, f.queue[at:]...)
	f.queue = next
	f.snap.QueueLen = len(next)

	return nil
}

// InsertQueueAndPlay splices the refs and, when nothing is live, starts the
// first of them. A live track is refused, mirroring the engine.
func (f *fakePlayer) InsertQueueAndPlay(index int, refs []string) error {
	f.mu.Lock()
	live := f.snap.State == molo.Playing || f.snap.State == molo.Paused
	f.mu.Unlock()
	if live {
		return molo.ErrNoLiveTrack
	}

	if err := f.InsertQueue(index, refs); err != nil {
		return err
	}
	f.mu.Lock()
	f.record("InsertQueueAndPlay")
	at := index
	if at < 0 || at > len(f.queue)-len(refs) {
		at = len(f.queue) - len(refs)
	}
	f.snap.State = molo.Playing
	f.snap.Path = f.queue[at]
	f.snap.QueueIndex = at
	f.mu.Unlock()
	f.emit(molo.StateChanged{From: molo.Idle, To: molo.Playing})
	f.emit(molo.TrackChanged{Index: at, Path: refs[0]})

	return nil
}

func (f *fakePlayer) Queue() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.queue...)
}

// Providers satisfies the facade. The CLI has no source chooser yet, so the
// local-only default is the whole list.
func (f *fakePlayer) Providers() []string { return nil }

func (f *fakePlayer) Pause() error {
	f.mu.Lock()
	f.record("Pause")
	f.snap.State = molo.Paused
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Resume() error {
	f.mu.Lock()
	f.record("Resume")
	f.snap.State = molo.Playing
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

func (f *fakePlayer) SwapDecoder(name string) error {
	f.mu.Lock()
	f.record("SwapDecoder")
	f.settings.Decoder = name
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) SwapBackend(name string) error {
	f.mu.Lock()
	f.record("SwapBackend")
	f.settings.Backend = name
	f.mu.Unlock()

	return nil
}

// Tap satisfies the facade. The CLI never reads the visualizer feed; Phase 6
// owns that.
func (f *fakePlayer) Tap() molo.Tap { return nil }

// Settings and ApplySettings mirror the real facade's contract closely enough
// for the CLI tests: validation is refused, a valid update is remembered.
func (f *fakePlayer) Settings() molo.Settings {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.settings
}

func (f *fakePlayer) ApplySettings(s molo.Settings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.Decoder != "" && s.Decoder != "opus-pion" && s.Decoder != "opus-libopusfile" {
		return fmt.Errorf("%w: decoder", molo.ErrInvalidSetting)
	}
	f.settings = s

	return nil
}

// Pipeline and the rest of the effect surface satisfy the facade. The CLI has
// no pipeline editor yet, so these are inert.
func (f *fakePlayer) Pipeline() dsp.Pipeline { return f.settings.Pipeline }

func (f *fakePlayer) ApplyPipeline(p dsp.Pipeline) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings.Pipeline = p

	return nil
}

func (f *fakePlayer) EffectSchema(kind string) ([]dsp.Param, error) {
	return dsp.Default.Schema(kind)
}

func (f *fakePlayer) EffectKinds() []string { return dsp.Default.Kinds() }

// Effects satisfies the editor surface the Player interface embeds. The CLI
// never opens the editor, so these are inert and change no state.
func (f *fakePlayer) Effects() molo.Effects                          { return f }
func (f *fakePlayer) EffectKindList() []molo.EffectKind              { return nil }
func (f *fakePlayer) EffectChain() molo.EffectChain                  { return molo.EffectChain{} }
func (f *fakePlayer) EffectMeters() []molo.EffectMeters              { return nil }
func (f *fakePlayer) AddEffect(kind, impl string) (string, error)    { return "", nil }
func (f *fakePlayer) RemoveEffect(id string) error                   { return nil }
func (f *fakePlayer) MoveEffect(id string, to int) error             { return nil }
func (f *fakePlayer) SetEffectParam(id, key string, value any) error { return nil }
func (f *fakePlayer) SetEffectBypass(id string, bypassed bool) error { return nil }

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

var _ molo.Player = (*fakePlayer)(nil)
