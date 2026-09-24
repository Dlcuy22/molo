// Fade implementation.
//
// A linear per-sample fade in or out, used to ease the edges of a preview
// window so starting one, switching between them and looping never clicks. It
// is a post-ring effect, so it runs on the real-time path and must not
// allocate or block.
//
// The ramp position is owned by the audio thread. Reset re-arms it, which is
// what makes the effect fade in again after a seek: the session resets the
// chain on every reposition, so a looping preview breathes back in at the loop
// seam instead of jumping.

package dsp

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/dlcuy22/player/core"
)

// init registers the fade implementation, matching the one-file-plus-one-init
// pattern the other effects use.
func init() {
	Register(NewFadeFactory())
}

// Fade parameter keys.
const (
	FadeDuration = "duration"
	FadeMode     = "mode"
)

// Fade modes. "in" ramps 0 to 1, "out" ramps 1 to 0.
const (
	FadeModeIn  = "in"
	FadeModeOut = "out"
)

// FadeFactory registers this implementation.
type FadeFactory struct{}

func NewFadeFactory() *FadeFactory { return &FadeFactory{} }

func (f *FadeFactory) Kind() string { return "fade" }

func (f *FadeFactory) Impl() string { return "fade-linear" }

func (f *FadeFactory) FriendlyName() string { return "Fade" }

// Weight is the only implementation of this kind, so the value is a formality.
func (f *FadeFactory) Weight() int { return 50 }

func (f *FadeFactory) Placement() Placement { return Post }

func (f *FadeFactory) Schema() []Param {
	return withCommon([]Param{
		{Key: FadeDuration, Kind: Float, Min: 0, Max: 10000, Step: 10, Unit: "ms", Default: 300.0},
		{Key: FadeMode, Kind: Enum, Options: []string{FadeModeIn, FadeModeOut}, Default: FadeModeIn},
	})
}

func (f *FadeFactory) New(values Values) (Effect, error) {
	store, err := newParamStore(f.Schema(), values)
	if err != nil {
		return nil, err
	}

	return &Fade{store: store}, nil
}

// fadeState is the immutable snapshot the audio thread reads: the ramp length
// in samples, the direction, the channel count and the standard gains. It is
// rebuilt whole and published atomically, so Process never takes a lock.
type fadeState struct {
	ch              int
	bypassed        bool
	gains           gainState
	durationSamples int
	mode            string
}

// Fade is the effect. Its published state is atomic; its ramp position is
// touched only by the audio thread.
type Fade struct {
	store *paramStore

	state atomic.Pointer[fadeState]

	// mu serialises the control side (Set, Configure, Reset) so a rebuild
	// never interleaves with another. It is never held by Process.
	mu sync.Mutex

	rate  int
	ch    int
	isSet bool

	// pos counts samples since the ramp was armed. Only Process writes it and
	// only Reset zeroes it.
	pos int
}

var _ Effect = (*Fade)(nil)

func (f *Fade) Name() string { return "fade" }

func (f *Fade) Schema() []Param { return f.store.Schema() }

func (f *Fade) Get(key string) (any, error) { return f.store.Get(key) }

func (f *Fade) Set(key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.store.Set(key, value); err != nil {
		return err
	}
	f.publishLocked()

	return nil
}

func (f *Fade) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Rate <= 0 || in.Ch < 1 {
		return in, fmt.Errorf("dsp: fade needs a positive rate and channel count, got %+v", in)
	}
	if in.Fmt != core.F32 {
		return in, fmt.Errorf("dsp: fade needs float32 samples, got format %d", in.Fmt)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.rate = in.Rate
	f.ch = in.Ch
	f.isSet = true
	f.pos = 0
	f.publishLocked()

	return in, nil
}

// Process applies the ramp in place, advancing one step per frame. Once the
// ramp is done the gain holds at its target (1 for in, 0 for out).
func (f *Fade) Process(buf []float32, frames int) error {
	st := f.state.Load()
	if st == nil || st.bypassed {
		return nil
	}

	applyGains(buf, frames, st.ch, st.gains, func() {
		n := frames * st.ch
		if n > len(buf) {
			n = len(buf)
		}
		n -= n % st.ch
		if n <= 0 {
			return
		}

		for i := 0; i < n; i += st.ch {
			g := fadeGain(st, f.pos)
			f.pos++
			for c := 0; c < st.ch; c++ {
				buf[i+c] = float32(float64(buf[i+c]) * g)
			}
		}
	})

	return nil
}

// fadeGain is the ramp value at sample pos: 0 to 1 for a fade in, 1 to 0 for a
// fade out, holding at the target once the ramp is complete.
func fadeGain(st *fadeState, pos int) float64 {
	if st.mode == FadeModeOut {
		if st.durationSamples <= 0 || pos >= st.durationSamples {
			return 0
		}

		return 1 - float64(pos)/float64(st.durationSamples)
	}
	if st.durationSamples <= 0 || pos >= st.durationSamples {
		return 1
	}

	return float64(pos) / float64(st.durationSamples)
}

// Reset re-arms the ramp so a seek fades in from silence rather than jumping to
// wherever the previous position left the gain.
func (f *Fade) Reset() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pos = 0

	return nil
}

// Bypassed reports the standard bypass parameter from the published state, so
// the chain can ask without taking a lock.
func (f *Fade) Bypassed() bool {
	st := f.state.Load()

	return st != nil && st.bypassed
}

// publishLocked rebuilds the audio-side snapshot. The caller holds mu.
func (f *Fade) publishLocked() {
	if !f.isSet {
		return
	}
	mode, _ := f.store.Get(FadeMode)
	modeStr, _ := mode.(string)
	st := &fadeState{
		ch:              f.ch,
		bypassed:        f.store.Bypassed(),
		gains:           compileGains(f.store),
		durationSamples: int(math.Round(f.store.Float(FadeDuration) * float64(f.rate) / 1000.0)),
		mode:            modeStr,
	}
	f.state.Store(st)
}
