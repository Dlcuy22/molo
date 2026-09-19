package tui

import (
	"testing"
	"time"
)

// TestMeterReleaseIsTimeBased pins the property that makes the meter usable at
// any poll rate: the release must depend on elapsed time, not on how many times
// push was called. Two pushes separated by one half-life must halve the level,
// whether they are two calls apart or many.
func TestMeterReleaseIsTimeBased(t *testing.T) {
	t0 := time.Unix(0, 0)

	var slow meter
	slow.push(1, t0)
	slow.push(0, t0.Add(meterHalfLife))
	if slow.level < 0.45 || slow.level > 0.55 {
		t.Fatalf("after one half-life the level is %v, want about 0.5", slow.level)
	}

	// The same elapsed time split into many polls must give the same answer:
	// this is the regression that a per-push decay factor cannot satisfy.
	var fast meter
	fast.push(1, t0)
	for i := 1; i <= 10; i++ {
		fast.push(0, t0.Add(time.Duration(i)*meterHalfLife/10))
	}
	if d := fast.level - slow.level; d < -0.02 || d > 0.02 {
		t.Fatalf("release depended on poll rate: fast=%v slow=%v", fast.level, slow.level)
	}
}

// TestMeterAttackSnapsUp keeps the fast attack: a louder reading must replace
// the held level immediately, or a transient between polls is lost.
func TestMeterAttackSnapsUp(t *testing.T) {
	t0 := time.Unix(0, 0)

	var m meter
	m.push(0.2, t0)
	m.push(0.9, t0.Add(time.Millisecond))
	if m.level != 0.9 {
		t.Fatalf("attack set the level to %v, want 0.9", m.level)
	}
}

// TestMeterZeroNowDoesNotDecay covers a caller with no clock: a zero instant
// means no elapsed time, so it must hold rather than divide by it.
func TestMeterZeroNowDoesNotDecay(t *testing.T) {
	var m meter
	m.push(1, time.Time{})
	if m.level != 1 {
		t.Fatalf("zero instant decayed the level to %v, want 1", m.level)
	}
}

// TestDrainTapReadsEverythingBuffered is the point of the change: one meter
// tick must consume the whole feed, not one block, so no audio is skipped
// between samples and a chunk larger than one read still registers.
func TestDrainTapReadsEverythingBuffered(t *testing.T) {
	tap := &fakeTap{}
	// Three full reads plus the remainder; a single-block reader would see only
	// the first block.
	samples := make([]float32, 0, 3000)
	for range 3000 {
		samples = append(samples, 0.5)
	}
	tap.setSamples(samples)

	level, frames := drainTap(tap, 1024)
	if frames != 3000 {
		t.Fatalf("drained %d frames, want all 3000", frames)
	}
	// A constant 0.5 has RMS 0.5 regardless of how it is chunked.
	if level < 0.499 || level > 0.501 {
		t.Fatalf("level = %v, want about 0.5", level)
	}
	// Three reads return data (1024, 1024, 952) and a fourth returns zero to
	// discover the feed is empty; draining cannot know it is done without it.
	if got := tap.readCount(); got != 4 {
		t.Fatalf("tap read %d times, want 4 (3 with data plus the empty probe)", got)
	}
}

// TestDrainTapEmptyIsZeroAndNoDecaySignal covers the no-new-audio case: zero
// frames is reported distinctly so the model can hold the level.
func TestDrainTapEmptyIsZeroAndNoDecaySignal(t *testing.T) {
	tap := &fakeTap{}

	level, frames := drainTap(tap, 1024)
	if level != 0 || frames != 0 {
		t.Fatalf("empty tap reported level=%v frames=%d, want 0, 0", level, frames)
	}
}

// TestDrainTapNilIsSafe covers a model built without an engine.
func TestDrainTapNilIsSafe(t *testing.T) {
	if level, frames := drainTap(nil, 1024); level != 0 || frames != 0 {
		t.Fatalf("nil tap reported %v, %d", level, frames)
	}
	if level, frames := drainTap(&fakeTap{}, 0); level != 0 || frames != 0 {
		t.Fatalf("zero block reported %v, %d", level, frames)
	}
}
