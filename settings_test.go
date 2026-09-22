package player_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/dsp"
)

// newTestPlayer builds a facade backed by the passive device registered in this
// package, so none of these tests touch an audio server.
func newTestPlayer(t *testing.T, opts ...player.Option) player.Player {
	t.Helper()

	all := append([]player.Option{player.WithBackend("facade-test")}, opts...)
	p, err := player.New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	return p
}

// settingsEqual compares two Settings. Settings carries a Pipeline, whose
// stage slices make the struct non-comparable, so a field-by-field comparison
// is the only way to tell whether a rejected update really changed nothing.
func settingsEqual(a, b player.Settings) bool {
	return a.Volume == b.Volume &&
		a.Decoder == b.Decoder &&
		a.Backend == b.Backend &&
		a.ProbeMode == b.ProbeMode &&
		reflect.DeepEqual(a.Pipeline, b.Pipeline)
}

// TestWithDecoderIsReflectedInSettings is the construction-path check for the
// decoder preference. It also proves a pipeline supplied through WithPipeline
// is in force before anything plays.
func TestWithDecoderIsReflectedInSettings(t *testing.T) {
	pipeline := dsp.Pipeline{Post: []dsp.Spec{{Kind: "crossfeed"}}}
	p := newTestPlayer(t, player.WithDecoder("opus-pion"), player.WithPipeline(pipeline))

	if got := p.Settings().Decoder; got != "opus-pion" {
		t.Fatalf("Settings().Decoder = %q, want opus-pion", got)
	}
	if got := p.Pipeline(); !reflect.DeepEqual(got, pipeline) {
		t.Fatalf("Pipeline() = %+v, want the configured %+v", got, pipeline)
	}
}

// TestApplyPipelineRejectsAnUnknownKindAndChangesNothing proves the facade
// validates every stage before applying any of them: a rejected pipeline leaves
// the chain in force untouched.
func TestApplyPipelineRejectsAnUnknownKindAndChangesNothing(t *testing.T) {
	good := dsp.Pipeline{Post: []dsp.Spec{{Kind: "crossfeed"}}}
	p := newTestPlayer(t, player.WithPipeline(good))

	err := p.ApplyPipeline(dsp.Pipeline{Post: []dsp.Spec{{Kind: "not-a-kind"}}})
	if err == nil {
		t.Fatal("ApplyPipeline(unknown kind) returned nil, want a validation error")
	}
	if got := p.Pipeline(); !reflect.DeepEqual(got, good) {
		t.Fatalf("a rejected pipeline changed state: %+v", got)
	}
}

// TestApplyPipelineAcceptsAValidChain proves the facade round-trips a valid
// pipeline and confirms it with a PipelineChanged event.
func TestApplyPipelineAcceptsAValidChain(t *testing.T) {
	p := newTestPlayer(t)

	next := dsp.Pipeline{Post: []dsp.Spec{{
		ID:     "xf",
		Kind:   "crossfeed",
		Params: dsp.Values{dsp.CrossfeedCutoff: 700.0, dsp.CrossfeedFeed: 4.5},
	}}}
	if err := p.ApplyPipeline(next); err != nil {
		t.Fatalf("ApplyPipeline: %v", err)
	}
	if got := p.Pipeline(); !reflect.DeepEqual(got, next) {
		t.Fatalf("Pipeline() = %+v, want %+v", got, next)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-p.Events():
			if changed, ok := ev.(player.PipelineChanged); ok {
				if changed.Stages != 1 {
					t.Fatalf("PipelineChanged.Stages = %d, want 1", changed.Stages)
				}

				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for a PipelineChanged through the facade")
		}
	}
}

// TestEffectSchemaAndKindsAreReExported proves the schema accessors work
// through the facade without building an effect, and that an unknown kind is an
// error.
func TestEffectSchemaAndKindsAreReExported(t *testing.T) {
	p := newTestPlayer(t)

	kinds := p.EffectKinds()
	if !containsString(kinds, "crossfeed") {
		t.Fatalf("EffectKinds() = %v, want it to include crossfeed", kinds)
	}

	schema, err := p.EffectSchema("crossfeed")
	if err != nil {
		t.Fatalf("EffectSchema(crossfeed): %v", err)
	}
	leading := []string{dsp.ParamBypass, dsp.ParamInputGain, dsp.ParamOutputGain}
	if len(schema) < len(leading) {
		t.Fatalf("EffectSchema has %d params, want at least %d", len(schema), len(leading))
	}
	for i, key := range leading {
		if schema[i].Key != key {
			t.Fatalf("EffectSchema[%d].Key = %q, want %q", i, schema[i].Key, key)
		}
	}

	if _, err := p.EffectSchema("not-a-kind"); err == nil {
		t.Fatal("EffectSchema(unknown) returned nil, want an error")
	}
}

// containsString reports whether want is in the slice.
func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}

	return false
}

func TestApplySettingsAcceptsAKnownDecoder(t *testing.T) {
	p := newTestPlayer(t)

	next := p.Settings()
	next.Decoder = "opus-libopusfile"
	if err := p.ApplySettings(next); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if got := p.Settings().Decoder; got != "opus-libopusfile" {
		t.Fatalf("Settings().Decoder = %q, want opus-libopusfile", got)
	}
}

func TestApplySettingsRejectsAnUnknownDecoderAndChangesNothing(t *testing.T) {
	p := newTestPlayer(t, player.WithDecoder("opus-pion"))
	before := p.Settings()

	next := before
	next.Decoder = "not-a-codec"
	err := p.ApplySettings(next)
	if !errors.Is(err, player.ErrInvalidSetting) {
		t.Fatalf("ApplySettings error = %v, want ErrInvalidSetting", err)
	}
	if got := p.Settings(); !settingsEqual(got, before) {
		t.Fatalf("a rejected update changed state: before %+v, after %+v", before, got)
	}
}

func TestApplySettingsRejectsAnUnknownBackend(t *testing.T) {
	p := newTestPlayer(t)
	before := p.Settings()

	next := before
	next.Backend = "not-a-backend"
	if err := p.ApplySettings(next); !errors.Is(err, player.ErrInvalidSetting) {
		t.Fatalf("ApplySettings error = %v, want ErrInvalidSetting", err)
	}
	if got := p.Settings(); !settingsEqual(got, before) {
		t.Fatalf("a rejected update changed state: %+v", got)
	}
}

func TestApplySettingsRejectsAnOutOfRangeVolume(t *testing.T) {
	p := newTestPlayer(t)
	before := p.Settings()

	next := before
	next.Volume = 2.5
	if err := p.ApplySettings(next); !errors.Is(err, player.ErrInvalidSetting) {
		t.Fatalf("ApplySettings error = %v, want ErrInvalidSetting", err)
	}
	if got := p.Settings(); !settingsEqual(got, before) {
		t.Fatalf("a rejected update changed state: %+v", got)
	}
}

func TestApplySettingsRejectsAnUnknownProbeMode(t *testing.T) {
	p := newTestPlayer(t)
	before := p.Settings()

	next := before
	next.ProbeMode = core.DurationMode(99)
	if err := p.ApplySettings(next); !errors.Is(err, player.ErrInvalidSetting) {
		t.Fatalf("ApplySettings error = %v, want ErrInvalidSetting", err)
	}
	if got := p.Settings(); !settingsEqual(got, before) {
		t.Fatalf("a rejected update changed state: %+v", got)
	}
}

// TestApplySettingsVolumeTakesEffectImmediately pins the one field that is not
// deferred: the gain is post-ring state, so it applies to the running audio.
func TestApplySettingsVolumeTakesEffectImmediately(t *testing.T) {
	p := newTestPlayer(t, player.WithVolume(1))

	next := p.Settings()
	next.Volume = 0.25
	if err := p.ApplySettings(next); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for p.Snapshot().Volume != 0.25 {
		if time.Now().After(deadline) {
			t.Fatalf("Snapshot().Volume = %v, want 0.25", p.Snapshot().Volume)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestFacadeSwapDecoderAndBackendRejectBadNames proves the facade re-exports
// the live-swap commands and validates their names synchronously, matching the
// non-blocking command contract.
func TestFacadeSwapDecoderAndBackendRejectBadNames(t *testing.T) {
	p := newTestPlayer(t, player.WithDecoder("opus-pion"))

	if err := p.SwapDecoder("not-a-codec"); err == nil {
		t.Fatal("SwapDecoder(not-a-codec) returned nil, want a validation error")
	}
	if err := p.SwapBackend("not-a-backend"); err == nil {
		t.Fatal("SwapBackend(not-a-backend) returned nil, want a validation error")
	}
	// A rejected swap changes nothing.
	if got := p.Settings(); got.Decoder != "opus-pion" {
		t.Fatalf("a rejected swap changed the decoder preference: %q", got.Decoder)
	}

	// A valid but non-live swap is accepted and only touches the preference.
	if err := p.SwapDecoder("opus-libopusfile"); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	if err := p.SwapBackend("facade-test"); err != nil {
		t.Fatalf("SwapBackend: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for p.Settings().Decoder != "opus-libopusfile" || p.Settings().Backend != "facade-test" {
		if time.Now().After(deadline) {
			t.Fatalf("swap preferences = %q/%q, want the requested pair", p.Settings().Decoder, p.Settings().Backend)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestFacadeSwapDecoderDoesNotBlock proves the live-swap commands are
// non-blocking like every other command: validation happens on the caller's
// goroutine and the engine step is queued.
func TestFacadeSwapDecoderDoesNotBlock(t *testing.T) {
	p := newTestPlayer(t)

	done := make(chan error, 1)
	go func() { done <- p.SwapDecoder("opus-pion") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SwapDecoder: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SwapDecoder blocked; it must not wait on the engine")
	}
}

// TestFacadeSwappedEventIsReExported proves the facade re-exports the swap
// completion event with the documented fields, so a UI can type-switch on it
// without importing internal/session.
func TestFacadeSwappedEventIsReExported(t *testing.T) {
	var ev player.Event = player.Swapped{Kind: "backend", Name: "oto", Elapsed: time.Millisecond}

	s, ok := ev.(player.Swapped)
	if !ok {
		t.Fatalf("player.Swapped did not satisfy player.Event: %T", ev)
	}
	if s.Kind != "backend" || s.Name != "oto" || s.Elapsed != time.Millisecond {
		t.Fatalf("Swapped = %+v, want the constructed value", s)
	}
}

func TestApplySettingsDoesNotBlock(t *testing.T) {
	// The contract is that a settings change is synchronous validation only: no
	// disk, no device, no control-loop round trip. A generous bound catches a
	// regression that made it wait on the engine.
	p := newTestPlayer(t)

	next := p.Settings()
	next.Decoder = "opus-pion"
	next.Volume = 0.5

	done := make(chan error, 1)
	go func() { done <- p.ApplySettings(next) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ApplySettings: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ApplySettings blocked; it must not wait on the engine")
	}
}
