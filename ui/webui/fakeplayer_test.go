package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/dsp"
)

// fakePlayer is a scriptable molo.Player for the preview session tests. It
// records the commands the previewer issues and lets a test drive the state
// machine without an audio device.
//
// Play and Seek are deliberately asynchronous, like the real engine: Play does
// not set StatePlaying until a test releases it, and Seek does not move the
// position until released. The synchronous version hid the exact bug where the
// previewer read "not playing yet" as "exhausted".
type fakePlayer struct {
	mu sync.Mutex

	state molo.State
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

	// queue is the ref list the facade reads back for LoadPaths' append case.
	queue []string

	calls []string
}

var _ molo.Player = (*fakePlayer)(nil)

func newFakePlayer() *fakePlayer {
	return &fakePlayer{state: molo.Idle, autoActivate: true, autoSeek: true}
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

// called reports whether a call was recorded, for tests that assert a command
// reached the engine without caring about the rest of the sequence.
func (f *fakePlayer) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}

	return false
}

func (f *fakePlayer) resetCalls() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func (f *fakePlayer) stateNow() molo.State {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.state
}

// activate completes a deferred Play, modelling the engine's build worker.
func (f *fakePlayer) activate() {
	f.mu.Lock()
	if f.state == molo.Idle || f.state == molo.Stopped {
		f.state = molo.Playing
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

func (f *fakePlayer) Snapshot() molo.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()

	return molo.Snapshot{State: f.state, Path: f.path, Position: f.pos}
}

func (f *fakePlayer) Events() <-chan molo.Event { return nil }

func (f *fakePlayer) DebugEvents() <-chan molo.DebugEvent { return nil }

func (f *fakePlayer) DebugDropped() int64 { return 0 }

func (f *fakePlayer) Play(path string) error {
	f.record("play:" + path)
	f.mu.Lock()
	f.path = path
	if f.autoActivate {
		f.state = molo.Playing
		f.pos = 0
	} else {
		f.state = molo.Idle
	}
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) PlayQueue(paths []string) error {
	f.mu.Lock()
	f.queue = append([]string(nil), paths...)
	f.mu.Unlock()
	f.record("playQueue:" + fmt.Sprint(len(paths)))

	return nil
}

func (f *fakePlayer) PlayIndex(index int) error { return nil }

func (f *fakePlayer) Next() error { return nil }

func (f *fakePlayer) Prev() error { return nil }

func (f *fakePlayer) SetShuffle(on bool) error {
	f.record(fmt.Sprintf("shuffle:%v", on))

	return nil
}

func (f *fakePlayer) Shuffled() bool { return false }

func (f *fakePlayer) Queue() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.queue...)
}

func (f *fakePlayer) Providers() []string { return nil }

// InsertQueue and InsertQueueAndPlay satisfy the facade for the YouTube Music
// command tests; the queue edit itself is covered by the engine's own tests.
// They keep the fake's queue in step so LoadPaths' append path is assertable.
func (f *fakePlayer) InsertQueue(index int, refs []string) error {
	f.record("insert:" + refs[0])
	f.mu.Lock()
	at := index
	if at < 0 || at > len(f.queue) {
		at = len(f.queue)
	}
	next := append([]string(nil), f.queue[:at]...)
	next = append(next, refs...)
	next = append(next, f.queue[at:]...)
	f.queue = next
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) InsertQueueAndPlay(index int, refs []string) error {
	f.mu.Lock()
	live := f.state == molo.Playing || f.state == molo.Paused
	f.mu.Unlock()
	if live {
		return molo.ErrNoLiveTrack
	}
	f.record("insertAndPlay:" + refs[0])
	f.mu.Lock()
	next := append([]string(nil), f.queue...)
	next = append(next, refs...)
	f.queue = next
	f.mu.Unlock()

	return nil
}
func (f *fakePlayer) Pause() error {
	f.record("pause")
	f.mu.Lock()
	f.state = molo.Paused
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Resume() error {
	f.record("resume")
	f.mu.Lock()
	f.state = molo.Playing
	f.mu.Unlock()

	return nil
}

func (f *fakePlayer) Stop() error {
	f.record("stop")
	f.mu.Lock()
	f.state = molo.Stopped
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

func (f *fakePlayer) Settings() molo.Settings { return molo.Settings{} }

func (f *fakePlayer) ApplySettings(s molo.Settings) error { return nil }

func (f *fakePlayer) Pipeline() dsp.Pipeline { return dsp.Pipeline{} }

func (f *fakePlayer) ApplyPipeline(p dsp.Pipeline) error {
	f.record("pipeline")

	return nil
}

func (f *fakePlayer) EffectSchema(kind string) ([]dsp.Param, error) { return nil, nil }

func (f *fakePlayer) EffectKinds() []string { return nil }

// Effects satisfies the editor surface the Player interface embeds. The
// preview session tests never open the editor, so these are inert.
func (f *fakePlayer) Effects() molo.Effects                          { return f }
func (f *fakePlayer) EffectKindList() []molo.EffectKind              { return nil }
func (f *fakePlayer) EffectChain() molo.EffectChain                  { return molo.EffectChain{} }
func (f *fakePlayer) EffectMeters() []molo.EffectMeters              { return nil }
func (f *fakePlayer) AddEffect(kind, impl string) (string, error)    { return "", nil }
func (f *fakePlayer) RemoveEffect(id string) error                   { return nil }
func (f *fakePlayer) MoveEffect(id string, to int) error             { return nil }
func (f *fakePlayer) SetEffectParam(id, key string, value any) error { return nil }
func (f *fakePlayer) SetEffectBypass(id string, bypassed bool) error { return nil }

func (f *fakePlayer) Tap() molo.Tap { return nil }

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
