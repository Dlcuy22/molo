package main

import (
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/dsp"
)

// fakePlayer is a scriptable player.Player for the preview session tests. It
// records the commands the previewer issues and lets a test drive the state
// machine without an audio device.
//
// Play and Seek are deliberately asynchronous, like the real engine: Play does
// not set StatePlaying until a test releases it, and Seek does not move the
// position until released. The synchronous version hid the exact bug where the
// previewer read "not playing yet" as "exhausted".
type fakePlayer struct {
	mu sync.Mutex

	state player.State
	path  string
	pos   time.Duration

	// autoActivate makes Play transition to Playing immediately. Tests that are
	// not about the async build window set it; the ones that are leave it false
	// and call activate themselves.
	autoActivate bool
	// autoSeek makes Seek land immediately. Tests about the positioning gate
	// leave it false and call landSeek.
	autoSeek bool
	// pendingSeek is the target a non-auto Seek is waiting to land.
	pendingSeek time.Duration

	calls []string
}

var _ player.Player = (*fakePlayer)(nil)

func newFakePlayer() *fakePlayer {
	return &fakePlayer{state: player.Idle, autoActivate: true, autoSeek: true}
}

func (f *fakePlayer) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakePlayer) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fakePlayer) resetCalls() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func (f *fakePlayer) stateNow() player.State {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.state
}

// activate completes a deferred Play, modelling the engine's build worker.
func (f *fakePlayer) activate() {
	f.mu.Lock()
	if f.state == player.Idle || f.state == player.Stopped {
		f.state = player.Playing
		f.pos = 0
	}
	f.mu.Unlock()
}

// landSeek completes a deferred Seek, modelling the engine's seek worker.
func (f *fakePlayer) landSeek() {
	f.mu.Lock()
	f.pos = f.pendingSeek
	f.pendingSeek = 0
	f.mu.Unlock()
}

func (f *fakePlayer) Snapshot() player.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()

	return player.Snapshot{State: f.state, Path: f.path, Position: f.pos}
}

func (f *fakePlayer) Events() <-chan player.Event { return nil }

func (f *fakePlayer) Play(path string) error {
	f.record("play:" + path)
	f.mu.Lock()
	f.path = path
	if f.autoActivate {
		f.state = player.Playing
		f.pos = 0
	} else {
		f.state = player.Idle
	}
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) PlayQueue(paths []string) error { return nil }

func (f *fakePlayer) PlayIndex(index int) error { return nil }

func (f *fakePlayer) Next() error { return nil }

func (f *fakePlayer) Prev() error { return nil }

func (f *fakePlayer) Queue() []string { return nil }

func (f *fakePlayer) Providers() []string { return nil }

// InsertQueue and InsertQueueAndPlay satisfy the facade for the YouTube Music
// command tests; the queue edit itself is covered by the engine's own tests.
func (f *fakePlayer) InsertQueue(index int, refs []string) error {
	f.record("insert:" + refs[0])

	return nil
}

func (f *fakePlayer) InsertQueueAndPlay(index int, refs []string) error {
	f.record("insertAndPlay:" + refs[0])

	return nil
}

func (f *fakePlayer) Pause() error {
	f.record("pause")
	f.mu.Lock()
	f.state = player.Paused
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Resume() error {
	f.record("resume")
	f.mu.Lock()
	f.state = player.Playing
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Stop() error {
	f.record("stop")
	f.mu.Lock()
	f.state = player.Stopped
	f.pos = 0
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Seek(d time.Duration) error {
	f.record("seek")
	f.mu.Lock()
	if f.autoSeek {
		f.pos = d
	} else {
		f.pendingSeek = d
	}
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) SetVolume(v float64) { f.record("volume") }

func (f *fakePlayer) SwapDecoder(name string) error { return nil }

func (f *fakePlayer) SwapBackend(name string) error { return nil }

func (f *fakePlayer) Settings() player.Settings { return player.Settings{} }

func (f *fakePlayer) ApplySettings(s player.Settings) error { return nil }

func (f *fakePlayer) Pipeline() dsp.Pipeline { return dsp.Pipeline{} }

func (f *fakePlayer) ApplyPipeline(p dsp.Pipeline) error {
	f.record("pipeline")

	return nil
}

func (f *fakePlayer) EffectSchema(kind string) ([]dsp.Param, error) { return nil, nil }

func (f *fakePlayer) EffectKinds() []string { return nil }

func (f *fakePlayer) Tap() player.Tap { return nil }

func (f *fakePlayer) Close() error { return nil }

// newPreviewHarness builds a previewer over a fake player, with a zero fade so
// the tests do not wait on a fade-out, and starts its goroutine.
func newPreviewHarness(t *testing.T, main *fakePlayer) (*previewer, *fakePlayer) {
	t.Helper()

	svc := &PlayerService{player: main}
	second := newFakePlayer()
	pv := newPreviewer(svc, second)
	pv.cfg.FadeMs = 0
	pv.start()
	t.Cleanup(pv.close)

	return pv, second
}
