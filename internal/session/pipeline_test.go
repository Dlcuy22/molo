package session

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/dsp"
	"github.com/dlcuy22/player/playback"
	"github.com/dlcuy22/player/stream"
)

// The test effects below register under kinds the real registry does not own.
// They exist so the post-ring wiring can be proven with a stage whose output is
// known exactly, without adding anything to the shipped dsp package.
const (
	testConstKind  = "test-const"
	testConst2Kind = "test-const2"
	testLatentKind = "test-latent"
)

// effectRunLog records the effects that ran on the audio path, in order. The
// writer is the device's consumer goroutine and the reader is the test, so the
// log is guarded.
var effectRunLog = &orderLog{}

func recordEffectRun(name string) { effectRunLog.add(name) }

// pipelineTestFactory builds a synthetic effect. constant, when non-zero, makes
// Process overwrite every sample with that value, which is how the gain-order
// test tells "effects before gain" from "gain before effects": a constant 1
// scaled by a half-volume master gain lands on 0.5, and a gain applied first
// would be erased by the constant.
type pipelineTestFactory struct {
	kind     string
	impl     string
	latency  time.Duration
	constant float32
}

func (f *pipelineTestFactory) Kind() string         { return f.kind }
func (f *pipelineTestFactory) Impl() string         { return f.impl }
func (f *pipelineTestFactory) FriendlyName() string { return f.impl }
func (f *pipelineTestFactory) Weight() int          { return 1 }
func (f *pipelineTestFactory) Placement() dsp.Placement {
	return dsp.Post
}

func (f *pipelineTestFactory) Schema() []dsp.Param {
	return []dsp.Param{{Key: dsp.ParamBypass, Kind: dsp.Bool, Default: false}}
}

func (f *pipelineTestFactory) New(dsp.Values) (dsp.Effect, error) {
	return &pipelineTestEffect{name: f.impl, latency: f.latency, constant: f.constant}, nil
}

// pipelineTestEffect is the stage those factories build. It records every
// Process call and optionally stamps a constant, and it reports latency when
// the factory configured one.
type pipelineTestEffect struct {
	name     string
	latency  time.Duration
	constant float32
	ch       int
}

func (e *pipelineTestEffect) Name() string        { return e.name }
func (e *pipelineTestEffect) Schema() []dsp.Param { return (&pipelineTestFactory{}).Schema() }
func (e *pipelineTestEffect) Get(string) (any, error) {
	return nil, dsp.ErrUnknownParam
}
func (e *pipelineTestEffect) Set(string, any) error { return dsp.ErrUnknownParam }
func (e *pipelineTestEffect) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	e.ch = in.Ch

	return in, nil
}
func (e *pipelineTestEffect) Reset() error { return nil }

func (e *pipelineTestEffect) Process(buf []float32, frames int) error {
	recordEffectRun(e.name)
	if e.constant == 0 {
		return nil
	}
	ch := e.ch
	if ch < 1 {
		ch = 1
	}
	n := frames * ch
	if n > len(buf) {
		n = len(buf)
	}
	for i := range buf[:n] {
		buf[i] = e.constant
	}

	return nil
}

// Latency makes the effect satisfy dsp.Latent, so the chain sums it and the
// reported position has to correct for it.
func (e *pipelineTestEffect) Latency() time.Duration { return e.latency }

// pipelineFactories is a sync.Once so a test binary that re-runs init cannot
// register the same (kind, impl) twice, which dsp.Register would panic on.
var pipelineFactories sync.Once

func registerPipelineFactories() {
	pipelineFactories.Do(func() {
		dsp.Register(&pipelineTestFactory{kind: testConstKind, impl: testConstKind + "-v1", constant: 1})
		dsp.Register(&pipelineTestFactory{kind: testConst2Kind, impl: testConst2Kind + "-v1", constant: 0.25})
		dsp.Register(&pipelineTestFactory{kind: testLatentKind, impl: testLatentKind + "-v1", latency: 40 * time.Millisecond})
	})
}

// pipelineSession wires a session over a passive recording device, so no audio
// server is touched and no read happens unless the test asks for one.
func pipelineSession(t *testing.T, volume float64, pipeline dsp.Pipeline, opts func(*Config)) *Session {
	t.Helper()
	registerPipelineFactories()

	cfg := testConfig()
	cfg.Volume = volume
	cfg.Pipeline = pipeline
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 1.0, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	if opts != nil {
		opts(&cfg)
	}

	return newSession(t, cfg)
}

// TestPipelineEffectsRunBeforeTheMasterGain is the ordering test: the chain
// runs first and the master gain scales what the chain produced. The effect
// stamps a constant 1, so a half-volume gain must turn that into 0.5. Were the
// gain first, the effect would overwrite the scaled sample and the output would
// be 1.
func TestPipelineEffectsRunBeforeTheMasterGain(t *testing.T) {
	effectRunLog.reset()

	pipeline := dsp.Pipeline{Post: []dsp.Spec{{Kind: testConstKind}}}
	s := pipelineSession(t, 0.5, pipeline, nil)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return s.Snapshot().Stats.Buffered > 0 })

	buf := make([]float32, 256*canonical.Ch)
	if _, err := s.provider.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames: %v", err)
	}
	for i, v := range buf {
		if v != 0.5 {
			t.Fatalf("sample %d = %v, want 0.5 (effect ran, then the half gain scaled it)", i, v)
		}
	}

	if got := effectRunLog.snapshot(); len(got) == 0 {
		t.Fatal("the effect never ran on the post-ring path")
	}
}

// TestPipelineSetAtConstructionIsInstalled proves the chain configured through
// Config is actually in force once playback starts, with no ApplyPipeline call,
// and that its stages run in the order the pipeline lists them. The second
// stage stamps 0.25, so a reversed order would leave the first stage's 1 in the
// buffer; the recorded run log names the order explicitly.
func TestPipelineSetAtConstructionIsInstalled(t *testing.T) {
	effectRunLog.reset()

	pipeline := dsp.Pipeline{Post: []dsp.Spec{{Kind: testConstKind}, {Kind: testConst2Kind}}}
	s := pipelineSession(t, 1, pipeline, nil)

	if got := s.Pipeline(); !reflect.DeepEqual(got.Post, pipeline.Post) {
		t.Fatalf("Pipeline() = %+v, want the configured %+v", got, pipeline)
	}

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return s.Snapshot().Stats.Buffered > 0 })

	buf := make([]float32, 64*canonical.Ch)
	if _, err := s.provider.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames: %v", err)
	}
	if buf[0] != 0.25 {
		t.Fatalf("first sample = %v, want 0.25 from the last stage; the chain is out of order", buf[0])
	}

	got := effectRunLog.snapshot()
	if len(got) < 2 || got[0] != testConstKind+"-v1" || got[1] != testConst2Kind+"-v1" {
		t.Fatalf("effect run order = %v, want [%s %s] at the front", got, testConstKind+"-v1", testConst2Kind+"-v1")
	}
}

// TestPipelineRunsARealEffectAlongsideATestEffect proves a shipped effect takes
// part in the same chain on the real-time path: the crossfeed's leak is audible
// and the recording stage beside it still ran.
func TestPipelineRunsARealEffectAlongsideATestEffect(t *testing.T) {
	effectRunLog.reset()

	pipeline := dsp.Pipeline{Post: []dsp.Spec{
		{Kind: "crossfeed", Params: dsp.Values{dsp.CrossfeedCutoff: 700.0, dsp.CrossfeedFeed: 4.5}},
		{Kind: testLatentKind},
	}}
	s := pipelineSession(t, 1, pipeline, nil)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the ring to fill", func() bool { return s.Snapshot().Stats.Buffered > 0 })

	buf := make([]float32, 64*canonical.Ch)
	if _, err := s.provider.ReadFrames(buf); err != nil {
		t.Fatalf("ReadFrames: %v", err)
	}

	if got := effectRunLog.snapshot(); !containsString(got, testLatentKind+"-v1") {
		t.Fatalf("the second stage never ran: %v", got)
	}
}

// TestApplyPipelineWhilePlayingReplacesTheChain proves a live change installs
// the new chain, reports it, and leaves audio flowing and the state Playing.
func TestApplyPipelineWhilePlayingReplacesTheChain(t *testing.T) {
	devLog := &orderLog{}
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(devLog) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	next := dsp.Pipeline{Post: []dsp.Spec{{
		ID:     "xf",
		Kind:   "crossfeed",
		Params: dsp.Values{dsp.CrossfeedCutoff: 700.0, dsp.CrossfeedFeed: 4.5},
	}}}
	if err := s.ApplyPipeline(next); err != nil {
		t.Fatalf("ApplyPipeline: %v", err)
	}
	ev := waitEvent[PipelineChanged](t, s, 2*time.Second)
	if ev.Stages != 1 {
		t.Fatalf("PipelineChanged.Stages = %d, want 1", ev.Stages)
	}

	// Audio keeps flowing and the session never stopped.
	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a live pipeline change = %s, want Playing", stateName(got))
	}
	after := s.Snapshot().Position
	eventually(t, 2*time.Second, "audio to keep moving", func() bool { return s.Snapshot().Position > after })

	// Clearing the chain is the same operation with zero stages.
	if err := s.ApplyPipeline(dsp.Pipeline{}); err != nil {
		t.Fatalf("ApplyPipeline(empty): %v", err)
	}
	if ev := waitEvent[PipelineChanged](t, s, 2*time.Second); ev.Stages != 0 {
		t.Fatalf("PipelineChanged.Stages = %d, want 0 after clearing", ev.Stages)
	}
}

// TestApplyPipelineRejectsAnUnknownKindAndChangesNothing pins the atomic
// rejection: a bad kind is an error and the chain in force is untouched.
func TestApplyPipelineRejectsAnUnknownKindAndChangesNothing(t *testing.T) {
	good := dsp.Pipeline{Post: []dsp.Spec{{Kind: "crossfeed"}}}
	s := pipelineSession(t, 1, good, nil)

	err := s.ApplyPipeline(dsp.Pipeline{Post: []dsp.Spec{{Kind: "no-such-effect"}}})
	if !errors.Is(err, dsp.ErrUnknownKind) {
		t.Fatalf("ApplyPipeline(unknown kind) = %v, want ErrUnknownKind", err)
	}
	if got := s.Pipeline(); !reflect.DeepEqual(got, good) {
		t.Fatalf("a rejected pipeline changed state: %+v", got)
	}
	assertNoEvent[PipelineChanged](t, s, 100*time.Millisecond)
}

// TestApplyPipelineRejectsAnUnknownParamAndChangesNothing is the other half of
// validation: the kind exists but a parameter key does not, and the whole
// update is refused rather than half-applied.
func TestApplyPipelineRejectsAnUnknownParamAndChangesNothing(t *testing.T) {
	good := dsp.Pipeline{Post: []dsp.Spec{{Kind: "crossfeed"}}}
	s := pipelineSession(t, 1, good, nil)

	bad := dsp.Pipeline{Post: []dsp.Spec{{
		Kind:   "crossfeed",
		Params: dsp.Values{"not-a-param": 1.0},
	}}}
	err := s.ApplyPipeline(bad)
	if !errors.Is(err, dsp.ErrUnknownParam) {
		t.Fatalf("ApplyPipeline(unknown param) = %v, want ErrUnknownParam", err)
	}
	if got := s.Pipeline(); !reflect.DeepEqual(got, good) {
		t.Fatalf("a rejected pipeline changed state: %+v", got)
	}
	assertNoEvent[PipelineChanged](t, s, 100*time.Millisecond)
}

// TestApplyPipelineRoundTripsTheSameSpec proves a pipeline can be applied and
// read back unchanged, and that re-applying it is an accepted no-op.
func TestApplyPipelineRoundTripsTheSameSpec(t *testing.T) {
	s := pipelineSession(t, 1, dsp.Pipeline{}, nil)

	want := dsp.Pipeline{Post: []dsp.Spec{
		{ID: "a", Kind: "crossfeed", Params: dsp.Values{dsp.CrossfeedCutoff: 900.0, dsp.CrossfeedFeed: 3.0}},
		{ID: "b", Kind: testConstKind},
	}}
	if err := s.ApplyPipeline(want); err != nil {
		t.Fatalf("ApplyPipeline: %v", err)
	}
	waitEvent[PipelineChanged](t, s, 2*time.Second)

	if got := s.Pipeline(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Pipeline() = %+v, want %+v", got, want)
	}

	// Applying the identical pipeline again is accepted and installs nothing:
	// there is no change for the engine to confirm.
	if err := s.ApplyPipeline(want); err != nil {
		t.Fatalf("re-ApplyPipeline: %v", err)
	}
	assertNoEvent[PipelineChanged](t, s, 100*time.Millisecond)
}

// TestEffectSchemaLeadsWithTheStandardParams pins the schema a UI renders: the
// three framework parameters first, then the effect's own, and an error for an
// unknown kind.
func TestEffectSchemaLeadsWithTheStandardParams(t *testing.T) {
	s := pipelineSession(t, 1, dsp.Pipeline{}, nil)

	schema, err := s.EffectSchema("crossfeed")
	if err != nil {
		t.Fatalf("EffectSchema(crossfeed): %v", err)
	}

	leading := []string{dsp.ParamBypass, dsp.ParamInputGain, dsp.ParamOutputGain}
	if len(schema) < len(leading) {
		t.Fatalf("Schema has %d params, want at least %d", len(schema), len(leading))
	}
	for i, key := range leading {
		if schema[i].Key != key {
			t.Fatalf("Schema[%d].Key = %q, want %q", i, schema[i].Key, key)
		}
	}
	if schema[len(leading)].Key != dsp.CrossfeedCutoff {
		t.Fatalf("Schema[%d].Key = %q, want the effect's own %q", len(leading), schema[len(leading)].Key, dsp.CrossfeedCutoff)
	}

	if _, err := s.EffectSchema("no-such-effect"); !errors.Is(err, dsp.ErrUnknownKind) {
		t.Fatalf("EffectSchema(unknown) = %v, want ErrUnknownKind", err)
	}
}

// TestEffectKindsIncludesCrossfeed proves the registry a UI uses to offer
// stages is reachable through the session.
func TestEffectKindsIncludesCrossfeed(t *testing.T) {
	s := pipelineSession(t, 1, dsp.Pipeline{}, nil)

	kinds := s.EffectKinds()
	if !containsString(kinds, "crossfeed") {
		t.Fatalf("EffectKinds() = %v, want it to include crossfeed", kinds)
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

// TestPipelineLatencyCorrectsTheReportedPosition installs a stage that reports
// a known latency and proves the snapshot's position is the heard position, not
// the streamer's read-ahead. The device is passive, so the test controls exactly
// how many frames the streamer has produced and the arithmetic is exact.
func TestPipelineLatencyCorrectsTheReportedPosition(t *testing.T) {
	latency := 40 * time.Millisecond
	pipeline := dsp.Pipeline{Post: []dsp.Spec{{Kind: testLatentKind}}}
	s := pipelineSession(t, 1, pipeline, nil)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	const frames = 9600 // 200 ms at 48 kHz
	eventually(t, 2*time.Second, "the ring to buffer the frames to read", func() bool {
		return s.Snapshot().Stats.Buffered >= frames
	})

	// The recording device consumes nothing, so the only frames the streamer
	// has produced are the ones read here. Reading exactly frames advances its
	// position to frames, which makes the arithmetic exact.
	buf := make([]float32, frames*canonical.Ch)
	read := 0
	for read < frames {
		want := frames - read
		n, err := s.provider.ReadFrames(buf[:want*canonical.Ch])
		if err != nil {
			t.Fatalf("ReadFrames at frame %d: %v", read, err)
		}
		read += n
	}
	if got := s.provider.current().Position(); got != frames {
		t.Fatalf("streamer position = %d, want %d after reading exactly that many", got, frames)
	}

	raw := stream.FramesToDuration(frames)
	want := raw - latency
	if got := s.Snapshot().Position; got != want {
		t.Fatalf("Snapshot.Position = %v, want %v (streamer %v minus %v chain latency)", got, want, raw, latency)
	}
}

// TestPipelineLatencyDoesNotGoNegative proves the correction clamps rather than
// reporting a negative position at the very start of a track, before the chain
// latency has been read.
func TestPipelineLatencyDoesNotGoNegative(t *testing.T) {
	pipeline := dsp.Pipeline{Post: []dsp.Spec{{Kind: testLatentKind}}}
	s := pipelineSession(t, 1, pipeline, nil)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)

	// Nothing has been read yet, so the streamer position is 0 and the latency
	// correction would go negative without the clamp.
	if got := s.Snapshot().Position; got != 0 {
		t.Fatalf("Snapshot.Position at track start = %v, want 0", got)
	}
}

// TestApplyPipelineDuringSeekCoexists proves a live pipeline change and an
// in-flight seek, which share the control loop, do not corrupt each other: the
// seek lands on its target, the chain is the one applied, and the session is
// still playing.
func TestApplyPipelineDuringSeekCoexists(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &gatedDecoder{toneDecoder: toneDecoder{value: 0.5, total: 1 << 40}, gate: gate}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	if err := s.Seek(2 * time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the reposition never started")
	}

	// The seek is parked on the worker; the control loop is free, so the
	// pipeline change must be accepted and confirmed while it runs.
	next := dsp.Pipeline{Post: []dsp.Spec{{Kind: "crossfeed"}}}
	if err := s.ApplyPipeline(next); err != nil {
		t.Fatalf("ApplyPipeline during seek: %v", err)
	}
	if ev := waitEvent[PipelineChanged](t, s, 2*time.Second); ev.Stages != 1 {
		t.Fatalf("PipelineChanged.Stages = %d, want 1", ev.Stages)
	}

	gate.open()
	if ev := waitEvent[Seeked](t, s, 2*time.Second); ev.Position != 2*time.Second {
		t.Fatalf("Seeked.Position = %v, want 2s", ev.Position)
	}

	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a seek and a pipeline change = %s, want Playing", stateName(got))
	}
	if got := s.Pipeline(); !reflect.DeepEqual(got, next) {
		t.Fatalf("Pipeline() = %+v, want the applied %+v", got, next)
	}
}

// TestApplyPipelineDuringDecoderSwapCoexists is the swap half of the
// coexistence test: the decoder reopens on the worker while the chain changes
// on the control loop, and both must land cleanly.
func TestApplyPipelineDuringDecoderSwapCoexists(t *testing.T) {
	gate := newSeekGate()
	defer gate.open()

	log := newOpenerLog()
	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = func(name, path string) (decode.Decoder, error) {
		if name == "codec-b" {
			return &gatedDecoder{toneDecoder: toneDecoder{value: 0.25, total: 1 << 40}, gate: gate}, nil
		}

		return log.openers()(name, path)
	}
	cfg.validateDecoder = swapCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("tone"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "audio to start moving", func() bool { return s.Snapshot().Position > 0 })

	if err := s.SwapDecoder("codec-b"); err != nil {
		t.Fatalf("SwapDecoder: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the swap never started")
	}

	next := dsp.Pipeline{Post: []dsp.Spec{{Kind: "crossfeed"}}}
	if err := s.ApplyPipeline(next); err != nil {
		t.Fatalf("ApplyPipeline during a decoder swap: %v", err)
	}
	waitEvent[PipelineChanged](t, s, 2*time.Second)

	gate.open()
	if ev := waitEvent[Swapped](t, s, 2*time.Second); ev.Kind != "decoder" || ev.Name != "codec-b" {
		t.Fatalf("Swapped = %+v, want kind decoder name codec-b", ev)
	}

	if got := s.Snapshot().State; got != StatePlaying {
		t.Fatalf("state after a decoder swap and a pipeline change = %s, want Playing", stateName(got))
	}
	if got := s.Pipeline(); !reflect.DeepEqual(got, next) {
		t.Fatalf("Pipeline() = %+v, want the applied %+v", got, next)
	}
}
