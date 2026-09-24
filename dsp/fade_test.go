package dsp

import (
	"math"
	"testing"

	"github.com/dlcuy22/player/core"
)

// newTestFade builds a fade at the canonical format with the given duration and
// mode, failing the test if the schema rejects them.
func newTestFade(t *testing.T, durationMs float64, mode string) *Fade {
	t.Helper()

	e, err := NewFadeFactory().New(Values{FadeDuration: durationMs, FadeMode: mode})
	if err != nil {
		t.Fatalf("build fade: %v", err)
	}
	f := e.(*Fade)
	if _, err := f.Configure(core.CanonicalFormat); err != nil {
		t.Fatalf("configure fade: %v", err)
	}

	return f
}

// TestFadeInRampsFromSilenceToUnity checks the ramp endpoints and monotonicity:
// the first sample is silent, the last ramped sample approaches unity, and the
// gain never decreases across the ramp.
func TestFadeInRampsFromSilenceToUnity(t *testing.T) {
	const rate = 48000
	f := newTestFade(t, 10, FadeModeIn)

	// 10 ms at 48 kHz is 480 frames. Process them in two buffers to prove the
	// ramp position survives across Process calls.
	buf := make([]float32, 240*2)
	for i := range buf {
		buf[i] = 1
	}
	if err := f.Process(buf, 240); err != nil {
		t.Fatalf("Process: %v", err)
	}
	first := buf[0]
	if first != 0 {
		t.Fatalf("first sample = %v, want 0 (silence at ramp start)", first)
	}

	// Every subsequent sample must be >= the previous one (monotone ramp).
	prev := first
	for i := 0; i < 240; i++ {
		g := buf[i*2]
		if g < prev {
			t.Fatalf("frame %d gain %v dropped below previous %v", i, g, prev)
		}
		prev = g
	}

	// Second buffer continues toward unity.
	for i := range buf {
		buf[i] = 1
	}
	if err := f.Process(buf, 240); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// Sample 479 (the last before the 480-sample ramp completes) should be
	// just under unity, and the following sample (index 480) is past the ramp
	// so it holds at unity. Here we only have 480 samples total, so the last
	// is 479/480.
	want := float32(479.0 / 480.0)
	if math.Abs(float64(buf[len(buf)-2]-want)) > 1e-4 {
		t.Fatalf("last ramped sample = %v, want ~%v", buf[len(buf)-2], want)
	}

	// A third buffer is past the ramp and must be unity.
	third := make([]float32, 10*2)
	for i := range third {
		third[i] = 1
	}
	if err := f.Process(third, 10); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if third[0] != 1 {
		t.Fatalf("post-ramp sample = %v, want 1 (hold at unity)", third[0])
	}
}

// TestFadeOutRampsToSilence is the mirror: starts near unity and ends at zero.
func TestFadeOutRampsToSilence(t *testing.T) {
	f := newTestFade(t, 10, FadeModeOut)

	buf := make([]float32, 480*2)
	for i := range buf {
		buf[i] = 1
	}
	if err := f.Process(buf, 480); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if buf[0] != 1 {
		t.Fatalf("first sample = %v, want 1 (unity at ramp start)", buf[0])
	}
	// The ramp reaches zero at sample N; the last sample of this buffer is
	// 1/N above it, and the next buffer starts at exactly zero.
	if buf[len(buf)-2] != 1.0/480.0 {
		t.Fatalf("last ramped sample = %v, want %v", buf[len(buf)-2], 1.0/480.0)
	}

	next := make([]float32, 4*2)
	for i := range next {
		next[i] = 1
	}
	if err := f.Process(next, 4); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if next[0] != 0 {
		t.Fatalf("sample after the ramp = %v, want 0", next[0])
	}
}

// TestFadeResetRearmsRamp pins the property the loop relies on: after Reset the
// next Process starts from the ramp's beginning again, so a loop seam fades in
// rather than jumping.
func TestFadeResetRearmsRamp(t *testing.T) {
	f := newTestFade(t, 10, FadeModeIn)

	buf := make([]float32, 480*2)
	for i := range buf {
		buf[i] = 1
	}
	if err := f.Process(buf, 480); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := f.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	again := make([]float32, 4*2)
	for i := range again {
		again[i] = 1
	}
	if err := f.Process(again, 4); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if again[0] != 0 {
		t.Fatalf("first sample after Reset = %v, want 0", again[0])
	}
}

// TestFadeZeroDurationIsAnInstantCut keeps the degenerate case honest: a zero
// length is a hard cut, not a division by zero.
func TestFadeZeroDurationIsAnInstantCut(t *testing.T) {
	in := newTestFade(t, 0, FadeModeIn)
	buf := []float32{1, 1, 1, 1}
	if err := in.Process(buf, 2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if buf[0] != 1 {
		t.Fatalf("zero-length fade in first sample = %v, want 1", buf[0])
	}

	out := newTestFade(t, 0, FadeModeOut)
	buf2 := []float32{1, 1, 1, 1}
	if err := out.Process(buf2, 2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if buf2[0] != 0 {
		t.Fatalf("zero-length fade out first sample = %v, want 0", buf2[0])
	}
}

// TestFadeBypassPassesThrough confirms the standard bypass skips the ramp.
func TestFadeBypassPassesThrough(t *testing.T) {
	e, err := NewFadeFactory().New(Values{FadeDuration: 10.0, FadeMode: FadeModeIn, ParamBypass: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	f := e.(*Fade)
	if _, err := f.Configure(core.CanonicalFormat); err != nil {
		t.Fatalf("configure: %v", err)
	}

	buf := []float32{1, 1, 1, 1}
	if err := f.Process(buf, 2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if buf[0] != 1 {
		t.Fatalf("bypassed fade changed a sample: %v", buf[0])
	}
	if !f.Bypassed() {
		t.Fatal("Bypassed() = false after bypass=true")
	}
}

// TestFadeSchemaRejectsUnknownMode keeps a typo loud instead of silently
// selecting a direction.
func TestFadeSchemaRejectsUnknownMode(t *testing.T) {
	if _, err := NewFadeFactory().New(Values{FadeMode: "sideways"}); err == nil {
		t.Fatal("New accepted an unknown mode")
	}
}
