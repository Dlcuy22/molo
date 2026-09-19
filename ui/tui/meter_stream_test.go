package tui

import (
	"testing"
	"time"
)

// TestMeterStreamTracksAudioWithoutMainTick is the end-to-end evidence for the
// real-time meter: the level must follow the audio purely on the meter's own
// stream, with no main tick and no engine event in between. It feeds a loud
// stretch, then silence, through successive meter ticks and checks the level
// rises at once and releases over later ticks.
func TestMeterStreamTracksAudioWithoutMainTick(t *testing.T) {
	f := newFakePlayer()

	// Every meter tick drains the fake tap once. Alternating a loud and a quiet
	// feed between ticks is what a device publishing in chunks looks like to a
	// reader that runs faster than the publishes. The clock is injected so the
	// release is measured in real time, not in how fast the test runs.
	m := newModel(f)
	clock := time.Unix(0, 0)
	m.now = func() time.Time { return clock }

	// One loud tick: the level must jump to the loud RMS immediately.
	f.tap.setSamples([]float32{1, -1, 1, -1})
	m = stepMeter(t, m)
	if m.meter.level < 0.9 {
		t.Fatalf("level after a loud tick = %v, want about 1", m.meter.level)
	}

	// A silent publish (real frames, all zero) half a half-life later: the level
	// must start releasing.
	clock = clock.Add(meterHalfLife / 2)
	f.tap.setSamples([]float32{0, 0, 0, 0})
	m = stepMeter(t, m)
	released := m.meter.level
	if released >= 0.9 || released <= 0 {
		t.Fatalf("silence did not release the level: %v", released)
	}

	// No publish at all, another half-life on: the level must hold, not decay as
	// if the sound stopped.
	clock = clock.Add(meterHalfLife / 2)
	m = stepMeter(t, m)
	if m.meter.level < released-0.01 {
		t.Fatalf("an empty tick decayed the meter from %v to %v", released, m.meter.level)
	}

	// And it must climb back on the next loud tick.
	f.tap.setSamples([]float32{1, -1, 1, -1})
	m = stepMeter(t, m)
	if m.meter.level < 0.9 {
		t.Fatalf("level did not recover on a loud tick: %v", m.meter.level)
	}
}

// stepMeter runs one meter tick through Update and returns the resulting model.
func stepMeter(t *testing.T, m model) model {
	t.Helper()

	next, cmd := m.Update(meterTick{})
	got := next.(model)
	if cmd == nil {
		t.Fatal("meter tick did not re-arm the meter stream")
	}
	// Apply the read command the tick produced, as the runtime would.
	if cmd := readTap(got.tap); cmd != nil {
		msg, ok := cmd().(meterMsg)
		if !ok {
			t.Fatalf("readTap produced %T", cmd())
		}
		next, _ = got.Update(msg)

		return next.(model)
	}

	return got
}
