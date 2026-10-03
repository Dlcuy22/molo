package dsp

import (
	"math"
	"sync/atomic"

	"github.com/dlcuy22/molo/core"
)

// MinVolume and MaxVolume bound the gain. The range is closed on both ends:
// 0 is silence and 1 is unity. Values above unity are not accepted because a
// post-ring gain has no limiter behind it, so amplifying only moves the
// clipping point, it cannot prevent it. When a limiter stage lands, raising
// MaxVolume is that stage's decision, not this one's.
const (
	MinVolume = 0.0
	MaxVolume = 1.0
)

// defaultChannels is the layout assumed before Configure runs. The post-ring
// path is always the canonical 48 kHz stereo ring, so this is the frame width
// the module sees in practice.
const defaultChannels = 2

// Gain is a post-ring volume stage. It scales interleaved float32 samples in
// place and never allocates, which keeps it legal on the real-time path.
//
// Gain is deliberately the single place a track's amplitude is changed, so the
// fade-in and fade-out that track transitions will need can be built on top of
// it (a ramp driven by the same atomic volume) rather than beside it. Nothing
// here may grow a lock, a channel, or an allocation without breaking that plan.
type Gain struct {
	// volume holds math.Float64bits so a live SetVolume is a single atomic
	// store; an atomic float64 is unavailable before Go 1.19 and this is the
	// portable equivalent. SetVolume is the only writer.
	volume atomic.Uint64

	channels int
}

var _ core.Module = (*Gain)(nil)

// NewGain returns a gain stage at v, clamped to [MinVolume, MaxVolume]. The
// caller may keep a reference and call SetVolume while audio is running.
func NewGain(v float64) *Gain {
	g := &Gain{channels: defaultChannels}
	g.SetVolume(v)

	return g
}

// Name identifies the module in the streamer's error messages.
func (g *Gain) Name() string { return "gain" }

// Configure accepts any format unchanged: scaling samples cannot alter their
// layout. It only records the channel count, because Process needs it to know
// how many samples the declared frame count covers. Like every core.Module,
// Configure runs at setup time; only SetVolume is meant to be called while
// Process is live.
func (g *Gain) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Ch > 0 {
		g.channels = in.Ch
	}

	return in, nil
}

// Process multiplies the first frames*channels samples of buf by the current
// volume. It is safe to call concurrently with SetVolume; the load is atomic.
func (g *Gain) Process(buf []float32, frames int) error {
	v := float32(g.Volume())

	n := frames * g.channels
	if n > len(buf) {
		n = len(buf)
	}
	if n <= 0 {
		return nil
	}

	// A unity volume would otherwise still cost one multiply per sample; the
	// branch is predictable and pins the "1.0 is a no-op" contract.
	if v == 1 {
		return nil
	}
	// Skip the loop entirely at silence rather than multiply by zero, so a
	// muted stream costs nothing beyond the clear.
	if v == 0 {
		clear(buf[:n])

		return nil
	}

	for i := range buf[:n] {
		buf[i] *= v
	}

	return nil
}

// Reset satisfies core.Module. Gain carries no state across buffers, so volume
// intentionally survives a seek or a track change.
func (g *Gain) Reset() error { return nil }

// Volume reports the current value, clamped.
func (g *Gain) Volume() float64 {
	return math.Float64frombits(g.volume.Load())
}

// SetVolume changes the gain for subsequent Process calls. It is safe to call
// from a control goroutine while the audio thread is in Process. Out-of-range
// values, including NaN, clamp into [MinVolume, MaxVolume] instead of being
// ignored, so a bad caller cannot leave the previous volume silently in force.
func (g *Gain) SetVolume(v float64) {
	switch {
	case math.IsNaN(v) || v < MinVolume:
		v = MinVolume
	case v > MaxVolume:
		v = MaxVolume
	}
	g.volume.Store(math.Float64bits(v))
}
