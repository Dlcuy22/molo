package dsp

import (
	"sync/atomic"
	"time"

	"github.com/dlcuy22/molo/core"
)

// Chain is an ordered set of effects that processes a buffer in place. It is
// the stage the session inserts between the streamer and the device.
//
// The audio thread reads the effect list through one atomic pointer, so a
// control goroutine can rebuild and swap the whole chain without ever making
// Process wait. A swap is the unit of change: adding, removing or reordering
// an effect publishes a new list, while changing one effect's parameter goes
// through that effect's own Set.
type Chain struct {
	effects atomic.Pointer[[]Effect]
}

// NewChain returns an empty chain. An empty chain passes audio through.
func NewChain() *Chain {
	c := &Chain{}
	empty := []Effect{}
	c.effects.Store(&empty)

	return c
}

// Set replaces the chain's effects. It is safe while Process is running: the
// new list is stored atomically and the audio thread picks it up on its next
// call. The caller owns configuring and resetting the effects before this.
func (c *Chain) Set(effects []Effect) {
	list := slicesClone(effects)
	c.effects.Store(&list)
}

// Effects returns the current list. It is a snapshot: a later Set does not
// change it, and it must not be mutated.
func (c *Chain) Effects() []Effect {
	list := c.effects.Load()
	if list == nil {
		return nil
	}

	return slicesClone(*list)
}

// Process runs every effect in order on the first frames interleaved samples
// of buf. It loads the list once, so a swap mid-buffer cannot make it run a
// mix of the old and new chains. It allocates nothing and takes no lock.
func (c *Chain) Process(buf []float32, frames int) error {
	list := c.effects.Load()
	if list == nil {
		return nil
	}
	for _, e := range *list {
		if err := e.Process(buf, frames); err != nil {
			return err
		}
	}

	return nil
}

// Configure negotiates the format through every effect in order and returns
// what the chain produces. It runs off the audio thread, before the chain is
// installed.
func (c *Chain) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	list := c.effects.Load()
	if list == nil {
		return in, nil
	}
	out := in
	for _, e := range *list {
		next, err := e.Configure(out)
		if err != nil {
			return out, err
		}
		out = next
	}

	return out, nil
}

// Reset clears the state of every effect, so a seek or a format change cannot
// blend pre-jump samples into post-jump output. It runs while the audio thread
// is parked, the same guarantee the streamer gives its pre-ring modules.
func (c *Chain) Reset() error {
	list := c.effects.Load()
	if list == nil {
		return nil
	}
	for _, e := range *list {
		if err := e.Reset(); err != nil {
			return err
		}
	}

	return nil
}

// Latency is the total delay the chain adds. A bypassed effect contributes
// nothing, which is what keeps the reported play position correct when a
// lookahead stage is switched off.
func (c *Chain) Latency() time.Duration {
	list := c.effects.Load()
	if list == nil {
		return 0
	}
	var total time.Duration
	for _, e := range *list {
		if b, ok := e.(Bypassable); ok && b.Bypassed() {
			continue
		}
		if l, ok := e.(Latent); ok {
			total += l.Latency()
		}
	}

	return total
}

// slicesClone copies an effect list without importing slices into the hot
// path's file for one call.
func slicesClone(in []Effect) []Effect {
	if in == nil {
		return nil
	}
	out := make([]Effect, len(in))
	copy(out, in)

	return out
}
