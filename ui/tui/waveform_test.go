package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestLoadWaveReturnsBuckets proves the waveform command carries the computed
// wave and the sequence it was started for.
func TestLoadWaveReturnsBuckets(t *testing.T) {
	want := &Wave{Buckets: []float32{0, 0.5, 1}, Frames: 100}
	fn := func(context.Context, string, int) (*Wave, error) { return want, nil }

	cmd := loadWave(context.Background(), fn, 7, "/music/a.opus", 3)
	if cmd == nil {
		t.Fatal("loadWave returned nil")
	}

	msg, ok := cmd().(waveMsg)
	if !ok {
		t.Fatalf("loadWave produced %T, want waveMsg", cmd())
	}
	if msg.seq != 7 || msg.wave != want {
		t.Fatalf("waveMsg = %+v, want seq 7 and the computed wave", msg)
	}
}

func TestLoadWaveCarriesError(t *testing.T) {
	boom := errors.New("decode failed")
	fn := func(context.Context, string, int) (*Wave, error) { return nil, boom }

	msg := loadWave(context.Background(), fn, 1, "x", 3)().(waveMsg)
	if !errors.Is(msg.err, boom) {
		t.Fatalf("waveMsg lost the error: %v", msg.err)
	}
}

// TestStartWaveCancelsPrevious is the load-bearing cancellation assertion: when
// a new track starts, the in-flight waveform pass must observe a cancelled
// context instead of decoding a file the user already left.
func TestStartWaveCancelsPrevious(t *testing.T) {
	started := make(chan context.Context, 1)
	fn := func(ctx context.Context, _ string, _ int) (*Wave, error) {
		started <- ctx
		<-ctx.Done()

		return nil, ctx.Err()
	}

	m := newModel(newFakePlayer())
	m.waveFn = fn

	cmd := m.startWave("/music/a.opus")
	if cmd == nil {
		t.Fatal("startWave returned nil with a producer")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd()
	}()

	ctx := <-started

	// Starting the next track must cancel the previous pass.
	if next := m.startWave("/music/b.opus"); next == nil {
		t.Fatal("second startWave returned nil")
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("a new track did not cancel the in-flight waveform pass")
	}
	<-done
}

// TestUpdateTrackChangeStartsWave proves the track handler wires the waveform
// pass up, so startWave is reachable from real messages and not just tests.
func TestUpdateTrackChangeStartsWave(t *testing.T) {
	m := newModel(newFakePlayer())
	m.waveFn = func(context.Context, string, int) (*Wave, error) { return &Wave{}, nil }

	next, cmd := m.Update(trackMsg{index: 0, path: "/music/a.opus"})
	m = next.(model)

	if m.waveSeq != 1 {
		t.Fatalf("wave sequence = %d, want 1", m.waveSeq)
	}
	if cmd == nil {
		t.Fatal("track change returned no command")
	}
	if m.waveFn == nil {
		t.Fatal("wave producer lost")
	}
}

// TestQuitCancelsWave proves quitting stops an in-flight decode instead of
// leaving it to run out the process.
func TestQuitCancelsWave(t *testing.T) {
	started := make(chan context.Context, 1)
	fn := func(ctx context.Context, _ string, _ int) (*Wave, error) {
		started <- ctx
		<-ctx.Done()

		return nil, ctx.Err()
	}

	m := newModel(newFakePlayer())
	m.waveFn = fn
	cmd := m.startWave("/music/a.opus")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd()
	}()

	ctx := <-started

	next, _ := m.Update(keyPress("q"))
	_ = next

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("quit did not cancel the in-flight waveform pass")
	}
	<-done
}

// TestStartWaveNilProducerIsNilCommand keeps a plain model free of the
// analysis dependency.
func TestStartWaveNilProducerIsNilCommand(t *testing.T) {
	m := newModel(newFakePlayer())
	if cmd := m.startWave("/music/a.opus"); cmd != nil {
		t.Fatalf("nil producer returned a command: %T", cmd)
	}
	if cmd := m.startWave(""); cmd != nil {
		t.Fatalf("empty path returned a command: %T", cmd)
	}
}

// TestStaleWaveIsDiscarded proves a result from a superseded pass cannot
// overwrite the waveform of the track that replaced it.
func TestStaleWaveIsDiscarded(t *testing.T) {
	m := newModel(newFakePlayer())
	m.waveSeq = 5
	m.wave = &Wave{Buckets: []float32{1}}

	next, _ := m.Update(waveMsg{seq: 4, wave: &Wave{Buckets: []float32{0}}})
	got := next.(model)

	if len(got.wave.Buckets) != 1 || got.wave.Buckets[0] != 1 {
		t.Fatalf("a stale wave overwrote the current one: %+v", got.wave)
	}
}

func TestRenderWaveShowsOnlyKnownBuckets(t *testing.T) {
	if got := renderWave(nil, 8); got != "" {
		t.Fatalf("nil wave rendered %q, want empty", got)
	}

	out := renderWave(&Wave{Buckets: []float32{0, 1, 0.5, 0.25}}, 4)
	if got := []rune(out); len(got) != 4 {
		t.Fatalf("wave rendered %d cells for 4 buckets: %q", len(got), out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("non-silent wave rendered blank: %q", out)
	}
}
