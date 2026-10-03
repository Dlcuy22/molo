package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/dsp"
	"github.com/dlcuy22/molo/playback"
	"github.com/dlcuy22/molo/provider"
	"github.com/dlcuy22/molo/stream"
)

// upgradedDecoderOpener builds a named, seekable decoder that reports distinct
// labels, the shape a real Source.Upgrade would return. It is a decode.Seeker
// because the seamless swap requires one to place it at the frontier.
func upgradedDecoderOpener() stream.Opener {
	return func(<-chan struct{}) (decode.Decoder, error) {
		return &namedDecoder{
			toneDecoder: toneDecoder{value: 0.25, total: 1 << 40},
			codec:       "upgraded",
		}, nil
	}
}

// TestSeamlessUpgradeSwapsTheLiveDecoder is the load-bearing test: with the flag
// on, a provider Source.Upgrade is called once and the live streamer ends up on
// the upgraded decoder, which the snapshot reports through its labels, while the
// position keeps advancing.
func TestSeamlessUpgradeSwapsTheLiveDecoder(t *testing.T) {
	p := &fakeProvider{
		name:    "remote",
		prefix:  "x:",
		source:  fakeSource{total: 1 << 40, value: 0.5},
		upgrade: func(context.Context) (stream.Opener, error) { return upgradedDecoderOpener(), nil },
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.Experimental.SourceUpgrade = true
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	eventually(t, 2*time.Second, "the upgrade to be requested", func() bool { return p.upgradeCount() == 1 })
	eventually(t, 2*time.Second, "the upgraded decoder labels", func() bool {
		snap := s.Snapshot()

		return snap.Decoder == "upgraded" && snap.Parser == "upgraded-parser"
	})

	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after the upgrade = %s, want Playing", stateName(got))
	}
	after := s.Snapshot().Position
	eventually(t, 2*time.Second, "audio to keep moving on the upgraded decoder", func() bool {
		return s.Snapshot().Position > after
	})
	assertNoEvent[Failed](t, s, 50*time.Millisecond)
}

// TestSeamlessUpgradeIsGatedByTheFlag proves the flag off leaves the source
// untouched: Upgrade is never called and the decoder keeps its labels.
func TestSeamlessUpgradeIsGatedByTheFlag(t *testing.T) {
	p := &fakeProvider{
		name:    "remote",
		prefix:  "x:",
		source:  fakeSource{total: 1 << 40, value: 0.5},
		upgrade: func(context.Context) (stream.Opener, error) { return upgradedDecoderOpener(), nil },
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.Experimental.SourceUpgrade = false
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Give a goroutine that should not exist the same window the success test
	// gives the one that should.
	time.Sleep(200 * time.Millisecond)
	if got := p.upgradeCount(); got != 0 {
		t.Fatalf("Upgrade was called %d times with the flag off, want 0", got)
	}
	if got := s.Snapshot().Decoder; got != "" {
		t.Fatalf("Snapshot.Decoder = %q, want empty (no upgrade)", got)
	}
}

// TestSeamlessUpgradeFailureKeepsPlaying proves a failed Upgrade is a silent
// no-op: no Failed event, the built decoder keeps its labels, and audio keeps
// flowing. An upgrade failure is a lost optimisation, not a track failure.
func TestSeamlessUpgradeFailureKeepsPlaying(t *testing.T) {
	boom := errors.New("upgrade download failed")
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{total: 1 << 40, value: 0.5},
		upgrade: func(context.Context) (stream.Opener, error) {
			return nil, boom
		},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.Experimental.SourceUpgrade = true
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the upgrade to be requested", func() bool { return p.upgradeCount() == 1 })

	assertNoEvent[Failed](t, s, 200*time.Millisecond)
	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a failed upgrade = %s, want Playing", stateName(got))
	}
	if got := s.Snapshot().Decoder; got != "" {
		t.Fatalf("Snapshot.Decoder = %q, want the unchanged (empty) labels", got)
	}
	after := s.Snapshot().Position
	eventually(t, 2*time.Second, "audio to keep moving", func() bool { return s.Snapshot().Position > after })
}

// TestSeamlessUpgradeNonSeekableKeepsPlaying proves the streamer's refusal of a
// non-seekable replacement (ErrSeamlessNotSeekable) does not kill playback: the
// upgrade ran, but the labels stay on the built decoder and no Failed arrives.
func TestSeamlessUpgradeNonSeekableKeepsPlaying(t *testing.T) {
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{total: 1 << 40, value: 0.5},
		upgrade: func(context.Context) (stream.Opener, error) {
			// plainDecoder is not a decode.Seeker, so the streamer refuses it.
			return func(<-chan struct{}) (decode.Decoder, error) {
				return &plainDecoder{dec: &toneDecoder{value: 0.25, total: 1 << 40}}, nil
			}, nil
		},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.Experimental.SourceUpgrade = true
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the upgrade to be requested", func() bool { return p.upgradeCount() == 1 })

	assertNoEvent[Failed](t, s, 200*time.Millisecond)
	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a refused upgrade = %s, want Playing", stateName(got))
	}
	if got := s.Snapshot().Decoder; got != "" {
		t.Fatalf("Snapshot.Decoder = %q, want the unchanged (empty) labels", got)
	}
	after := s.Snapshot().Position
	eventually(t, 2*time.Second, "audio to keep moving", func() bool { return s.Snapshot().Position > after })
}

// gatedProvider is a remote provider whose decoder is seekable and whose native
// seek parks on a gate, so a test can hold a user seek in flight while an
// upgrade completes. It also offers an Upgrade, since the seamless path needs
// the Source that owns the ref to carry it.
type gatedProvider struct {
	gate    *seekGate
	codec   string
	upgrade func(context.Context) (stream.Opener, error)
}

func (p *gatedProvider) Name() string { return "gated" }

func (p *gatedProvider) Match(string) bool { return true }

func (p *gatedProvider) Open(context.Context, string) (provider.Source, error) {
	return provider.Source{
		Opener: func(<-chan struct{}) (decode.Decoder, error) {
			return &gatedNamedDecoder{
				gatedDecoder: gatedDecoder{toneDecoder: toneDecoder{value: 0.5, total: 1 << 40}, gate: p.gate},
				codec:        p.codec,
			}, nil
		},
		Upgrade: p.upgrade,
	}, nil
}

// TestSeamlessUpgradeDoesNotClobberAPendingSeek is the priority guard: an
// upgrade that becomes ready while a user seek is in flight must wait, and only
// run once the seek has landed, so the seek is never displaced.
func TestSeamlessUpgradeDoesNotClobberAPendingSeek(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	release := make(chan struct{})
	ready := make(chan struct{})
	gated := &gatedProvider{
		gate:  gate,
		codec: "initial",
		upgrade: func(ctx context.Context) (stream.Opener, error) {
			select {
			case <-release:
				close(ready)

				return upgradedDecoderOpener(), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}

	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{gated}
	cfg.Experimental.SourceUpgrade = true
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the initial decoder labels", func() bool {
		return s.Snapshot().Decoder == "initial"
	})

	// Park a user seek in the gate, then let the upgrade finish behind it.
	if err := s.Seek(time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the user seek never started")
	}
	close(release)
	<-ready

	// The upgrade is ready now but the seek owns the worker, so the upgraded
	// labels must not appear until the seek is released and lands. Without the
	// guard the seamless request would have overwritten the pending seek slot.
	time.Sleep(100 * time.Millisecond)
	if got := s.Snapshot().Decoder; got != "initial" {
		t.Fatalf("the upgrade displaced the in-flight seek: Decoder = %q, want initial", got)
	}

	gate.open()
	waitEvent[Seeked](t, s, 3*time.Second)

	eventually(t, 2*time.Second, "the upgrade to be promoted after the seek", func() bool {
		return s.Snapshot().Decoder == "upgraded"
	})
	// The seek target survived: the upgrade did not reset the position.
	if got := s.Snapshot().Position; got < 900*time.Millisecond {
		t.Fatalf("Position after the upgraded seek = %v, want near 1s", got)
	}
	assertNoEvent[Failed](t, s, 100*time.Millisecond)
}

// seamlessResetKind is the pipeline stage the no-reset test installs. It is a
// kind no other test uses, so the reset-counting effect below is that test's
// alone.
const seamlessResetKind = "seamless-reset-test"

// resetCountEffect counts its Reset calls, which is the only way to observe that
// the seamless path did not clear the effect chain: the chain generation alone
// would not change on a Reset.
type resetCountEffect struct {
	mu     sync.Mutex
	resets int
}

func (e *resetCountEffect) Name() string            { return seamlessResetKind }
func (e *resetCountEffect) Schema() []dsp.Param     { return nil }
func (e *resetCountEffect) Get(string) (any, error) { return nil, dsp.ErrUnknownParam }
func (e *resetCountEffect) Set(string, any) error   { return dsp.ErrUnknownParam }

func (e *resetCountEffect) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	return in, nil
}

func (e *resetCountEffect) Process([]float32, int) error { return nil }

func (e *resetCountEffect) Reset() error {
	e.mu.Lock()
	e.resets++
	e.mu.Unlock()

	return nil
}

func (e *resetCountEffect) resetCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.resets
}

// resetCountFactory hands out one shared effect so the test holds a handle to
// the instance the session actually built and can read its Reset count.
type resetCountFactory struct {
	effect dsp.Effect
}

func (f *resetCountFactory) Kind() string             { return seamlessResetKind }
func (f *resetCountFactory) Impl() string             { return seamlessResetKind + "-v1" }
func (f *resetCountFactory) FriendlyName() string     { return seamlessResetKind }
func (f *resetCountFactory) Weight() int              { return 1 }
func (f *resetCountFactory) Placement() dsp.Placement { return dsp.Post }
func (f *resetCountFactory) Schema() []dsp.Param      { return nil }
func (f *resetCountFactory) New(dsp.Values) (dsp.Effect, error) {
	return f.effect, nil
}

var (
	seamlessResetEffect = &resetCountEffect{}
	seamlessResetOnce   sync.Once
)

func registerSeamlessResetEffect() {
	seamlessResetOnce.Do(func() {
		dsp.Register(&resetCountFactory{effect: seamlessResetEffect})
	})
}

// TestSeamlessUpgradeDoesNotParkOrReset is the central promise of the path: the
// swap keeps the ring, so it must not park the device (no pause/flush/resume)
// and must not reset the effect chain. The device log records the device
// lifecycle calls and a reset-counting effect records chain resets; both are
// frozen while the upgrade is parked, then released and checked once it lands.
// A regression that copied requestSeek's park/reset would fail here.
func TestSeamlessUpgradeDoesNotParkOrReset(t *testing.T) {
	registerSeamlessResetEffect()

	devLog := &orderLog{}
	release := make(chan struct{})
	started := make(chan struct{})
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{total: 1 << 40, value: 0.5},
		upgrade: func(ctx context.Context) (stream.Opener, error) {
			close(started)
			select {
			case <-release:
				return upgradedDecoderOpener(), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.Experimental.SourceUpgrade = true
	cfg.Pipeline = dsp.Pipeline{Post: []dsp.Spec{{Kind: seamlessResetKind}}}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(devLog) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the upgrade never ran")
	}

	// Freeze what the upgrade must leave alone, then let it land.
	devLog.reset()
	resetsBefore := seamlessResetEffect.resetCount()
	genBefore := s.installed.Load().gen

	close(release)
	eventually(t, 2*time.Second, "the upgraded decoder labels", func() bool {
		return s.Snapshot().Decoder == "upgraded"
	})

	if ops := devLog.snapshot(); len(ops) != 0 {
		t.Fatalf("a seamless upgrade touched the device: %v", ops)
	}
	if got := seamlessResetEffect.resetCount(); got != resetsBefore {
		t.Fatalf("reset count changed across the upgrade: %d -> %d", resetsBefore, got)
	}
	if got := s.installed.Load().gen; got != genBefore {
		t.Fatalf("pipeline generation changed across the upgrade: %d -> %d", genBefore, got)
	}
}

// TestSeamlessUpgradeStopsOnTrackChange proves the upgrade is scoped to its
// track: advancing the queue cancels the pending download instead of letting it
// run until session Close.
func TestSeamlessUpgradeStopsOnTrackChange(t *testing.T) {
	stopped := make(chan struct{}, 4)
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{total: 1 << 40, value: 0.5},
		upgrade: func(ctx context.Context) (stream.Opener, error) {
			<-ctx.Done()
			stopped <- struct{}{}

			return nil, ctx.Err()
		},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.Experimental.SourceUpgrade = true
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"x:one", "x:two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 2*time.Second, "the retired track's upgrade to be cancelled", func() bool {
		return len(stopped) > 0
	})
}
