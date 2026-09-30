package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/meta"
	"github.com/dlcuy22/player/playback"
	"github.com/dlcuy22/player/provider"
)

// fakeProvider is a remote-style provider driven by a scheme prefix. It records
// every Open and every opener call, so the tests can prove routing, that the
// opener is a factory for seek, and that a ctx-honouring Open does not hang
// shutdown.
type fakeProvider struct {
	name   string
	prefix string

	mu        sync.Mutex
	opens     int
	openCalls int
	matched   []string
	// openBlock, when non-nil, parks Open until the channel closes or ctx is
	// cancelled, which is how the shutdown test builds a provider mid-open.
	openBlock chan struct{}
	// nilOpener makes Open return a Source with no Opener, to check the guard.
	nilOpener bool
	// nilDecoder makes the Opener return (nil, nil), to check the guard.
	nilDecoder bool
	// local marks the Source local, so the session keeps its own opener,
	// resolver and prober, as LocalAudio does.
	local bool

	// source describes what Open returns. probe and meta let a test check the
	// description path; total sets the decoder's frame count.
	source fakeSource
}

// fakeSource is the Source a fakeProvider builds.
type fakeSource struct {
	meta  meta.Meta
	probe func(mode core.DurationMode) (core.StreamInfo, error)
	total int64
	// value is the audio the decoder emits.
	value float32
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Match(ref string) bool {
	if p.prefix == "" {
		return true
	}

	return len(ref) >= len(p.prefix) && ref[:len(p.prefix)] == p.prefix
}

func (p *fakeProvider) Open(ctx context.Context, ref string) (provider.Source, error) {
	p.mu.Lock()
	p.opens++
	p.matched = append(p.matched, ref)
	block := p.openBlock
	p.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return provider.Source{}, ctx.Err()
		}
	}

	src := p.source
	if p.nilOpener {
		return provider.Source{Local: p.local, Meta: src.meta, Probe: src.probe}, nil
	}
	opener := func(<-chan struct{}) (decode.Decoder, error) {
		p.mu.Lock()
		p.openCalls++
		p.mu.Unlock()

		if p.nilDecoder {
			return nil, nil
		}

		return &plainDecoder{dec: &toneDecoder{value: src.value, total: src.total}}, nil
	}

	return provider.Source{Opener: opener, Local: p.local, Meta: src.meta, Probe: src.probe}, nil
}

func (p *fakeProvider) openCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.openCalls
}

func (p *fakeProvider) sourceOpenCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.opens
}

// plainDecoder is a tone decoder without SeekFrame, so the streamer must use the
// reopen-and-discard fallback, which is the path that calls the provider's
// Opener again. It holds the toneDecoder by field rather than embedding it,
// because embedding would promote SeekFrame and make it a decode.Seeker.
type plainDecoder struct {
	dec *toneDecoder
}

func (d *plainDecoder) Info() core.StreamInfo { return d.dec.Info() }

func (d *plainDecoder) ReadFrames(dst []float32) (int, error) { return d.dec.ReadFrames(dst) }

func (d *plainDecoder) Close() error { return d.dec.Close() }

// TestPlainDecoderIsNotASeeker guards the seek test's premise: if plainDecoder
// ever grew a native seek, the streamer would stop calling the opener factory
// and TestProviderSeekReopensThroughTheOpener would prove nothing.
func TestPlainDecoderIsNotASeeker(t *testing.T) {
	var d decode.Decoder = &plainDecoder{}
	if _, isSeeker := d.(decode.Seeker); isSeeker {
		t.Fatal("plainDecoder implements decode.Seeker, so the seek test no longer exercises the opener")
	}
}

// TestProviderRoutesByFirstMatch proves the session asks providers in order and
// the first Match wins, even when a later provider would also claim the ref.
func TestProviderRoutesByFirstMatch(t *testing.T) {
	first := &fakeProvider{name: "first", prefix: "x:"}
	second := &fakeProvider{name: "second", prefix: "x:"}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{first, second}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if got := first.sourceOpenCount(); got != 1 {
		t.Fatalf("first provider opens = %d, want 1", got)
	}
	if got := second.sourceOpenCount(); got != 0 {
		t.Fatalf("second provider opens = %d, want 0 (first match must win)", got)
	}
	if got := s.Snapshot().Path; got != "x:track" {
		t.Fatalf("Snapshot.Path = %q, want the ref", got)
	}
}

// TestProviderNoMatchIsAnError proves a non-empty provider list with no match
// fails the track instead of silently opening the ref as a file path.
func TestProviderNoMatchIsAnError(t *testing.T) {
	p := &fakeProvider{name: "remote", prefix: "x:"}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	// A working legacy opener would happily open the ref as a file; the point
	// is that it must never be consulted.
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		t.Fatal("the legacy openDecoder was used for an unclaimed ref")

		return nil, errors.New("unreachable")
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("song.flac"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	ev := waitEvent[Failed](t, s, 2*time.Second)
	if !errors.Is(ev.Err, ErrNoProvider) {
		t.Fatalf("Failed.Err = %v, want ErrNoProvider", ev.Err)
	}
}

// TestProviderSuppliesMetaAndSkipsTheLocalResolver proves a provider ref uses
// the provider's description and never runs meta.Default(), which would invent
// a title from the ref string.
func TestProviderSuppliesMetaAndSkipsTheLocalResolver(t *testing.T) {
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{
			total: 1 << 40,
			value: 0.5,
			meta: meta.Meta{
				Codec:     "opus",
				Container: "webm",
				Tags:      meta.Tags{Title: "Remote Song", Artist: "Remote Artist"},
			},
		},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.Resolver = &spyResolver{inner: meta.Default()}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:gnkOESS2qs8"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	eventually(t, 2*time.Second, "the provider meta to land", func() bool {
		snap := s.Snapshot()

		return snap.Meta.Tags.Title == "Remote Song" && snap.Meta.Tags.Artist == "Remote Artist"
	})

	snap := s.Snapshot()
	if snap.Meta.Path != "x:gnkOESS2qs8" {
		t.Fatalf("Meta.Path = %q, want the ref", snap.Meta.Path)
	}
	if snap.Meta.Codec != "opus" || snap.Meta.Container != "webm" {
		t.Fatalf("Meta = %+v, want the provider's codec and container", snap.Meta)
	}
	if got := cfg.Resolver.(*spyResolver).calls(); got != 0 {
		t.Fatalf("the local resolver ran %d times for a provider ref, want 0", got)
	}
}

// TestLocalRefStillUsesTheDefaultResolver proves the local path is untouched:
// with providers configured but a local ref claimed by the local passthrough,
// the session's own resolver still describes the track.
func TestLocalRefStillUsesTheDefaultResolver(t *testing.T) {
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{
		&fakeProvider{name: "remote", prefix: "x:"},
		provider.LocalAudio{},
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	path := fixture(t, "stereo_2s.opus")
	if err := s.Play(path); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	eventually(t, 3*time.Second, "the local resolver to answer", func() bool {
		snap := s.Snapshot()

		return snap.Meta.Path == path && snap.Meta.Tags.Title != ""
	})
}

// wrapperProvider embeds LocalAudio and marks the Source local, the shape a
// third-party provider that wraps a filesystem source would have. It is what
// proves transparency is not identity-based: the session must treat it exactly
// like LocalAudio.
type wrapperProvider struct {
	provider.LocalAudio
}

func (wrapperProvider) Name() string { return "wrapper" }

func (wrapperProvider) Open(ctx context.Context, ref string) (provider.Source, error) {
	src, err := wrapperProvider{}.LocalAudio.Open(ctx, ref)
	if err != nil {
		return provider.Source{}, err
	}
	src.Local = true

	return src, nil
}

// TestLocalTransparencyIsNotIdentityBased is the M1 regression: any provider
// that marks its Source local must get the session's own resolver, prober and
// openDecoder, whatever its concrete type. Identity inspection used to break
// &LocalAudio{} and wrappers.
func TestLocalTransparencyIsNotIdentityBased(t *testing.T) {
	cases := map[string]provider.AudioProvider{
		"LocalAudio value":   provider.LocalAudio{},
		"LocalAudio pointer": &provider.LocalAudio{},
		"wrapper":            wrapperProvider{},
	}

	for name, local := range cases {
		t.Run(name, func(t *testing.T) {
			var probed, opened bool
			var mu sync.Mutex

			cfg := testConfig()
			cfg.Providers = []provider.AudioProvider{
				&fakeProvider{name: "remote", prefix: "x:"},
				local,
			}
			cfg.Resolver = &spyResolver{inner: meta.Default()}
			cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
				mu.Lock()
				opened = true
				mu.Unlock()

				return &toneDecoder{value: 0.5, total: 1 << 40}, nil
			}
			cfg.probeStream = func(string, decode.ProbeOptions) (core.StreamInfo, error) {
				mu.Lock()
				probed = true
				mu.Unlock()

				return core.StreamInfo{Format: canonical, TotalFrames: 96000}, nil
			}
			cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
			s := newSession(t, cfg)

			path := fixture(t, "stereo_2s.opus")
			if err := s.Play(path); err != nil {
				t.Fatalf("Play: %v", err)
			}
			waitState(t, s, StatePlaying, 3*time.Second)

			// The local resolver must describe the track, which only happens on
			// the transparent path.
			eventually(t, 3*time.Second, "the local resolver to answer", func() bool {
				return s.Snapshot().Meta.Tags.Title != ""
			})
			// The local prober must supply the duration.
			eventually(t, 2*time.Second, "the local probe duration", func() bool {
				return s.Snapshot().Duration == 2*time.Second
			})

			mu.Lock()
			defer mu.Unlock()
			if !opened {
				t.Fatal("cfg.openDecoder was not used for a local Source")
			}
			if !probed {
				t.Fatal("cfg.probeStream was not used for a local Source")
			}
			if got := cfg.Resolver.(*spyResolver).calls(); got == 0 {
				t.Fatal("the local resolver was not consulted for a local Source")
			}
		})
	}
}

// TestProviderMetaStreamIsReplacedByTheDecoder is the m4 regression: only the
// decoder can report the true stream shape, so a provider's Meta.Stream must not
// reach the snapshot.
func TestProviderMetaStreamIsReplacedByTheDecoder(t *testing.T) {
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{
			total: 1 << 40,
			value: 0.5,
			meta: meta.Meta{
				Codec: "opus",
				// Deliberately wrong: a rate and channel count the decoder
				// never produced. The snapshot must not show these.
				Stream: core.StreamInfo{
					Format:      core.FrameFormat{Rate: 11111, Ch: 7, Fmt: core.F32},
					TotalFrames: 123456,
				},
			},
		},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the provider meta to land", func() bool {
		return s.Snapshot().Meta.Codec == "opus"
	})

	got := s.Snapshot().Meta.Stream
	// canonicalFormat is the format the fake decoder reports through Info.
	if got.Format != canonicalFormat {
		t.Fatalf("Meta.Stream.Format = %+v, want the decoder's %+v", got.Format, canonicalFormat)
	}
	if got.TotalFrames == 123456 {
		t.Fatal("the provider's fake Meta.Stream.TotalFrames survived into the snapshot")
	}
	if got.TotalFrames != 1<<40 {
		t.Fatalf("Meta.Stream.TotalFrames = %d, want the decoder's %d", got.TotalFrames, int64(1)<<40)
	}
}

// TestProviderProbeSuppliesTheDuration proves Source.Probe is used when present.
func TestProviderProbeSuppliesTheDuration(t *testing.T) {
	var probeMu sync.Mutex
	var probedCalls int
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{
			total: 1 << 40,
			value: 0.5,
			probe: func(core.DurationMode) (core.StreamInfo, error) {
				probeMu.Lock()
				probedCalls++
				probeMu.Unlock()

				return core.StreamInfo{Format: canonical, TotalFrames: 96000}, nil
			},
		},
	}

	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	// The local prober must not run for a provider ref.
	cfg.probeStream = func(string, decode.ProbeOptions) (core.StreamInfo, error) {
		t.Fatal("the local prober ran for a provider ref with a Probe")

		return core.StreamInfo{}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	eventually(t, 2*time.Second, "the provider probe duration", func() bool {
		return s.Snapshot().Duration == 2*time.Second
	})
	probeMu.Lock()
	defer probeMu.Unlock()
	if probedCalls != 1 {
		t.Fatalf("Source.Probe calls = %d, want 1", probedCalls)
	}
}

// TestProviderWithoutProbeLeavesDurationUnknown proves a provider ref with no
// Probe is not probed by the local prober and stays unknown.
func TestProviderWithoutProbeLeavesDurationUnknown(t *testing.T) {
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{total: 1 << 40, value: 0.5},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.probeStream = func(string, decode.ProbeOptions) (core.StreamInfo, error) {
		t.Fatal("the local prober ran for a provider ref without a Probe")

		return core.StreamInfo{}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	time.Sleep(100 * time.Millisecond)
	if got := s.Snapshot().Duration; got != 0 {
		t.Fatalf("Duration = %v, want 0 (unknown)", got)
	}
}

// TestProviderSeekReopensThroughTheOpener proves the Opener is a factory: a
// seek on a provider decoder without a native seek goes through the
// reopen-and-discard fallback, which calls the provider's Opener again. It also
// pins that the same decoder is not reused.
func TestProviderSeekReopensThroughTheOpener(t *testing.T) {
	p := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{total: 1 << 40, value: 0.5},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the first decoder to build", func() bool { return p.openCount() == 1 })

	if err := s.Seek(500 * time.Millisecond); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if ev := waitEvent[Seeked](t, s, 2*time.Second); ev.Position != 500*time.Millisecond {
		t.Fatalf("Seeked.Position = %v, want 500ms", ev.Position)
	}
	// A native-seek decoder would never reopen; the plainDecoder has no
	// SeekFrame, so the fallback must have built a fresh decoder through the
	// provider's Opener.
	if got := p.openCount(); got < 2 {
		t.Fatalf("opener calls = %d, want at least 2 (a fresh decoder via reopen-and-discard)", got)
	}
	if got := p.sourceOpenCount(); got != 1 {
		t.Fatalf("Source.Open calls = %d, want 1 (only the build resolves the source)", got)
	}
}

// TestCatchAllBeforeSpecificShadowsIt documents the ordering footgun: a
// catch-all placed first claims every ref, so a specific provider behind it is
// never reached.
func TestCatchAllBeforeSpecificShadowsIt(t *testing.T) {
	catchAll := &fakeProvider{name: "local", prefix: ""}
	specific := &fakeProvider{name: "remote", prefix: "x:"}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{catchAll, specific}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	if got := catchAll.sourceOpenCount(); got != 1 {
		t.Fatalf("catch-all opens = %d, want 1", got)
	}
	if got := specific.sourceOpenCount(); got != 0 {
		t.Fatalf("shadowed provider opens = %d, want 0", got)
	}
}

// TestProviderOpenHonoursContextOnClose proves a provider Open that respects
// ctx does not hang Close, even when it is parked mid-open.
func TestProviderOpenHonoursContextOnClose(t *testing.T) {
	release := make(chan struct{})
	p := &fakeProvider{
		name:      "remote",
		prefix:    "x:",
		openBlock: release,
		source:    fakeSource{total: 1 << 40, value: 0.5},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	eventually(t, 2*time.Second, "Open to be entered", func() bool { return p.sourceOpenCount() == 1 })

	done := make(chan struct{})
	go func() {
		_ = s.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		// Unblock Open so a failed assertion does not leak the worker.
		close(release)
		t.Fatal("Close hung on a provider Open that honours ctx")
	}
	close(release)
}

// TestSwapDecoderOnProviderRefIsRefused proves a provider ref cannot switch
// decoder, while a local ref still can.
func TestSwapDecoderOnProviderRefIsRefused(t *testing.T) {
	log := newOpenerLog()
	remote := &fakeProvider{
		name:   "remote",
		prefix: "x:",
		source: fakeSource{total: 1 << 40, value: 0.5},
	}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{remote, provider.LocalAudio{}}
	cfg.Decoder = "codec-a"
	cfg.openDecoder = log.openers()
	cfg.validateDecoder = swapCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	// A provider ref: the swap is refused synchronously and nothing reopens.
	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play provider ref: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	if err := s.SwapDecoder("codec-b"); !errors.Is(err, ErrProviderNoDecoderSwap) {
		t.Fatalf("SwapDecoder on a provider ref = %v, want ErrProviderNoDecoderSwap", err)
	}
	if got := openerCount(log); got != 0 {
		t.Fatalf("a refused provider swap reopened the decoder: %d opens, want 0", got)
	}
	assertNoEvent[Swapped](t, s, 100*time.Millisecond)

	// A local ref with the same configuration: the swap still works. Readiness
	// must key off the track being live (Path + Decoder published by activate),
	// not off the opener count: openDecoder runs inside build's stream.New,
	// before the worker delivers the result, so an opener-count signal fires
	// while s.live is still nil and the swap would only record a preference.
	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play local ref: %v", err)
	}
	eventually(t, 2*time.Second, "the local track to activate", func() bool {
		snap := s.Snapshot()

		return snap.Path == "tone" && snap.Decoder == "codec-a"
	})
	if err := s.SwapDecoder("codec-b"); err != nil {
		t.Fatalf("SwapDecoder on a local ref: %v", err)
	}
	ev := waitEvent[Swapped](t, s, 2*time.Second)
	if ev.Kind != "decoder" || ev.Name != "codec-b" {
		t.Fatalf("Swapped = %+v, want kind decoder name codec-b", ev)
	}
	if got := openerCount(log); got < 2 {
		t.Fatalf("local swap reopened %d times, want at least 2", got)
	}
}

// TestProvidersListsNamesInOrder proves the facade's Providers query reports
// the configured order.
func TestProvidersListsNamesInOrder(t *testing.T) {
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{
		&fakeProvider{name: "remote", prefix: "x:"},
		provider.LocalAudio{},
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	got := s.Providers()
	want := []string{"remote", "local"}
	if len(got) != len(want) {
		t.Fatalf("Providers() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Providers() = %v, want %v", got, want)
		}
	}
}

// TestNoProvidersListsEmpty pins the default: the legacy local setup has no
// explicit provider names.
func TestNoProvidersListsEmpty(t *testing.T) {
	cfg := testConfig()
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if got := s.Providers(); len(got) != 0 {
		t.Fatalf("Providers() = %v, want empty", got)
	}
}

// TestProviderSourceWithoutOpenerIsRejected proves a provider that returns a
// Source with no Opener fails the track cleanly rather than panicking the
// producer goroutine.
func TestProviderSourceWithoutOpenerIsRejected(t *testing.T) {
	p := &fakeProvider{name: "broken", prefix: "x:", nilOpener: true}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	ev := waitEvent[Failed](t, s, 2*time.Second)
	if !errors.Is(ev.Err, ErrProviderNoOpener) {
		t.Fatalf("Failed.Err = %v, want ErrProviderNoOpener", ev.Err)
	}
}

// TestProviderOpenerReturningNilDecoderIsRejected proves an Opener that returns
// (nil, nil) fails the track rather than panicking Info on a nil decoder.
func TestProviderOpenerReturningNilDecoderIsRejected(t *testing.T) {
	p := &fakeProvider{name: "broken", prefix: "x:", nilDecoder: true, source: fakeSource{total: 1 << 40, value: 0.5}}
	cfg := testConfig()
	cfg.Providers = []provider.AudioProvider{p}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("x:track"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	ev := waitEvent[Failed](t, s, 2*time.Second)
	if !errors.Is(ev.Err, ErrProviderNoDecoder) {
		t.Fatalf("Failed.Err = %v, want ErrProviderNoDecoder", ev.Err)
	}
}

// spyResolver wraps a resolver and counts how many times it was consulted, so a
// test can prove the local resolver was skipped for a provider ref.
type spyResolver struct {
	inner meta.Resolver

	mu    sync.Mutex
	count int
}

func (r *spyResolver) Name() string           { return "spy" }
func (r *spyResolver) Priority() int          { return r.inner.Priority() }
func (r *spyResolver) Match(path string) bool { return r.inner.Match(path) }
func (r *spyResolver) Resolve(ctx context.Context, path string) (*meta.Meta, error) {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()

	return r.inner.Resolve(ctx, path)
}

func (r *spyResolver) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.count
}
