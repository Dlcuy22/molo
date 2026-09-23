package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/dsp"
)

// fakePlayer is a player.Player that never touches audio or the file system.
// It exists so the CLI's key dispatch, event drain and exit-code logic can be
// exercised in a unit test.
type fakePlayer struct {
	mu       sync.Mutex
	snap     player.Snapshot
	events   chan player.Event
	queue    []string
	calls    []string
	settings player.Settings
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
		events: make(chan player.Event, 64),
		snap:   player.Snapshot{Volume: 1, QueueIndex: -1},
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
	fail := f.fail
	autoEnd := f.autoEnd
	delay := f.endDelay
	f.mu.Unlock()

	if fail != nil {
		go func() {
			f.emit(player.Failed{Err: fail})
			f.emit(player.StateChanged{From: player.Idle, To: player.Stopped})
		}()

		return nil
	}

	if autoEnd {
		go f.playThrough(paths, delay)

		return nil
	}

	go func() {
		f.setSnapshot(func(s *player.Snapshot) {
			s.State = player.Playing
			s.Path = paths[0]
			s.QueueIndex = 0
			s.QueueLen = len(paths)
		})
		f.emit(player.StateChanged{From: player.Idle, To: player.Playing})
		f.emit(player.TrackChanged{Index: 0, Path: paths[0]})
	}()

	return nil
}

func (f *fakePlayer) playThrough(paths []string, delay time.Duration) {
	for i, path := range paths {
		f.setSnapshot(func(s *player.Snapshot) {
			s.State = player.Playing
			s.Path = path
			s.QueueIndex = i
			s.QueueLen = len(paths)
		})
		f.emit(player.TrackChanged{Index: i, Path: path})
		if delay > 0 {
			time.Sleep(delay)
		}
		f.setSnapshot(func(s *player.Snapshot) {
			s.Position = 0
			if i == len(paths)-1 {
				s.State = player.Stopped
			}
		})
		f.emit(player.TrackEnded{})
		if i == len(paths)-1 {
			f.emit(player.StateChanged{From: player.Playing, To: player.Stopped})
		}
	}
}

func (f *fakePlayer) setSnapshot(fn func(*player.Snapshot)) {
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
func (f *fakePlayer) Tap() player.Tap { return nil }

// Settings and ApplySettings mirror the real facade's contract closely enough
// for the CLI tests: validation is refused, a valid update is remembered.
func (f *fakePlayer) Settings() player.Settings {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.settings
}

func (f *fakePlayer) ApplySettings(s player.Settings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.Decoder != "" && s.Decoder != "opus-pion" && s.Decoder != "opus-libopusfile" {
		return fmt.Errorf("%w: decoder", player.ErrInvalidSetting)
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

var _ player.Player = (*fakePlayer)(nil)
