package dsp

import (
	"time"

	"github.com/dlcuy22/player/core"
)

// Placement says where an effect may run. A pipeline stage is either pre-ring
// (in the streamer's producer, where allocation is allowed) or post-ring (the
// device's real-time path, where it is not), and an effect can be built for
// either.
type Placement uint8

const (
	// Post is the real-time path and the default: gain, EQ, crossfeed, and
	// every stage that has to hear the canonical 48 kHz stereo buffer.
	Post Placement = iota
	// Pre runs in the producer before the ring, for a stage that wants the
	// stream as decoded.
	Pre
	// Any runs in either slot.
	Any
)

// Effect is one insertable stage of a pipeline. It is core.Module plus the
// parameter surface a UI and a preset need, so a chain stays a list of
// modules while an editor can still reach every knob.
//
// An effect is configured once when it is built and then processes buffers on
// the audio thread. Set is the only method a control goroutine calls while
// audio is running, and an implementation must publish the new state
// atomically rather than mutate what Process reads.
type Effect interface {
	// Name identifies the instance in a chain, for diagnostics.
	Name() string

	// Schema describes every parameter, including the standard bypass. The
	// returned slice is a copy and may be kept.
	Schema() []Param

	// Get returns the current value of key, or an error when it is unknown.
	Get(key string) (any, error)

	// Set validates and applies one parameter. It is safe to call while
	// Process is running: the implementation builds the new state and swaps it
	// in, so the audio thread keeps reading the old one until the swap.
	Set(key string, value any) error

	// Configure negotiates the format and rebuilds internal state for it. It
	// runs before Process, off the audio thread.
	Configure(in core.FrameFormat) (out core.FrameFormat, err error)

	// Process transforms the first frames interleaved samples of buf in
	// place. It must not allocate or block, and it must ignore everything past
	// the declared frame count.
	Process(buf []float32, frames int) error

	// Reset clears state that must not survive a seek or a format change.
	Reset() error
}

// Latent is implemented by an effect that delays its output, such as a
// lookahead limiter or a partitioned convolver. The chain sums the latencies
// of the effects that report it so the play position can be corrected.
type Latent interface {
	Latency() time.Duration
}

// Bypassable is implemented by an effect that can be switched off without
// being rebuilt. The chain consults it so a bypassed effect contributes no
// latency and can be skipped entirely.
type Bypassable interface {
	Bypassed() bool
}

// Metered is implemented by an effect that exposes live readings, such as a
// gain-reduction figure. It is read from a control goroutine, never the audio
// thread.
type Metered interface {
	Meters() map[string]float32
}
