// Crossfeed implementation.
//
// Crossfeed mixes a low-passed copy of each channel into the other, the way a
// pair of speakers leaks across the room, so stereo material played on
// headphones stops sounding trapped inside the head. This is the Bauer
// stereophonic-to-binaural (bs2b) design: a one-pole low-pass per channel, a
// high-boost shelf that replaces the direct path, the two crossed low-pass
// outputs, and a normalising gain that compensates the bass lift the shelf
// would otherwise add.
//
// The coefficients follow the published bs2b design equations, and the
// processing order matches the reference so the two are sample-comparable:
//
//	lo  = a0_lo * x + b1_lo * lo
//	hi  = a0_hi * x + a1_hi * x[-1] + b1_hi * hi
//	out = (hi + lo_other) * gain
//
// The arithmetic is float64 with float32 buffers, because the one-pole
// recurrence is where a float32 state would drift and the engine's samples are
// float32 anyway. The state persists across parameter changes, as it does in
// the reference: only Configure or Reset clears it.
//
// Clean-room note: this file implements the algorithm from its published
// description. It is not a translation of the C reference, and it does not
// reproduce the reference's tuning range clamps in the same place. It exposes
// the cutoff that the current Easy Effects crossfeed accidentally ignores, so
// the control behaves as its label says.

package dsp

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/dlcuy22/player/core"
)

// init registers the crossfeed implementation, matching the decode and meta
// pattern where a new implementation is one file plus one init.
func init() {
	Register(NewCrossfeedFactory())
}

// Crossfeed parameter keys. The names match the preset vocabulary so a stored
// preset maps one to one.
const (
	CrossfeedCutoff = "cutoff"
	CrossfeedFeed   = "feed"
)

// Crossfeed tuning range. The cutoff is the low-pass corner; the feed is a
// level in dB whose tenths select how much signal is sent across, so 4.5 sends
// the reference's 45 tenths.
const (
	crossfeedMinCutoff = 300.0
	crossfeedMaxCutoff = 2000.0
	crossfeedMinFeed   = 1.0
	crossfeedMaxFeed   = 15.0
)

// CrossfeedFactory registers this implementation.
type CrossfeedFactory struct{}

// NewCrossfeedFactory returns a factory for the crossfeed effect.
func NewCrossfeedFactory() *CrossfeedFactory { return &CrossfeedFactory{} }

func (f *CrossfeedFactory) Kind() string { return "crossfeed" }

// Impl names the reference algorithm this implementation follows.
func (f *CrossfeedFactory) Impl() string { return "crossfeed-bs2b" }

func (f *CrossfeedFactory) FriendlyName() string { return "Bauer Crossfeed" }

// Weight is the only implementation of this kind, so the value is a formality
// today. It follows decode's convention: higher wins the automatic choice.
func (f *CrossfeedFactory) Weight() int { return 90 }

func (f *CrossfeedFactory) Placement() Placement { return Post }

func (f *CrossfeedFactory) Schema() []Param {
	return withCommon([]Param{
		{Key: CrossfeedCutoff, Kind: Float, Min: crossfeedMinCutoff, Max: crossfeedMaxCutoff, Step: 1, Unit: "Hz", Default: 700.0},
		{Key: CrossfeedFeed, Kind: Float, Min: crossfeedMinFeed, Max: crossfeedMaxFeed, Step: 0.1, Unit: "dB", Default: 4.5},
	})
}

func (f *CrossfeedFactory) New(values Values) (Effect, error) {
	store, err := newParamStore(f.Schema(), values)
	if err != nil {
		return nil, err
	}

	return &Crossfeed{store: store}, nil
}

// crossfeedState is the immutable snapshot the audio thread reads: the design
// coefficients for one (rate, cutoff, feed) plus the channel count, the bypass
// flag and the standard gains. It is rebuilt whole and published atomically, so
// Process never takes a lock and never sees a half-updated configuration.
type crossfeedState struct {
	a0Lo, b1Lo       float64
	a0Hi, a1Hi, b1Hi float64
	gain             float64

	ch         int
	bypassed   bool
	gains      gainState
	cutoffHz   float64
	feedTenths int
}

// Crossfeed is the effect. Its state is published atomically; its filter state
// is touched only by the audio thread, so the two never race.
type Crossfeed struct {
	store *paramStore

	state atomic.Pointer[crossfeedState]

	// mu serialises the control side (Set, Configure, Reset) so a rebuild
	// never interleaves with another. It is never held by Process.
	mu sync.Mutex

	rate  int
	ch    int
	isSet bool

	// Filter state, one set of four poles per channel plus the high-boost
	// one-sample history. Only Process writes these; Reset zeroes them.
	loL, loR     float64
	hiL, hiR     float64
	asisL, asisR float64

	// in measures the buffer at Process entry, out at exit. A bypassed effect
	// still measures: it is passing audio, and a fixed UI meter that vanished
	// on bypass would be worse than one that keeps showing the level.
	in  Meter
	out Meter
}

var _ Effect = (*Crossfeed)(nil)
var _ Metered = (*Crossfeed)(nil)

func (c *Crossfeed) Name() string { return "crossfeed" }

func (c *Crossfeed) Schema() []Param { return c.store.Schema() }

func (c *Crossfeed) Get(key string) (any, error) { return c.store.Get(key) }

// Set validates the value, stores it, and republishes the state. The design
// equations are cheap here, so there is no work to move off-thread; a heavier
// effect would build its state on a worker and swap it the same way.
func (c *Crossfeed) Set(key string, value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.store.Set(key, value); err != nil {
		return err
	}
	c.publishLocked()

	return nil
}

// Configure records the format and rebuilds for the new rate. It is the only
// place the rate changes, so a saved state always belongs to the rate the
// effect is running at.
func (c *Crossfeed) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Rate <= 0 || in.Ch < 1 {
		return in, fmt.Errorf("dsp: crossfeed needs a positive rate and channel count, got %+v", in)
	}
	if in.Fmt != core.F32 {
		return in, fmt.Errorf("dsp: crossfeed needs float32 samples, got format %d", in.Fmt)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.rate = in.Rate
	c.ch = in.Ch
	c.isSet = true
	c.publishLocked()
	c.clearStateLocked()

	return in, nil
}

// Process applies the crossfeed in place. It reads the state pointer once and
// takes no lock, so a concurrent Set cannot make one buffer use two coefficient
// sets and the audio thread never waits on a control goroutine. The standard
// gains wrap the effect's own work.
func (c *Crossfeed) Process(buf []float32, frames int) error {
	st := c.state.Load()
	if st == nil {
		return nil
	}
	// The buffer is measured at entry and exit whether or not the effect does
	// anything: the meters report what the stage carried, not what it changed.
	c.in.Push(buf, frames, c.meterCh(st))
	if st.bypassed {
		c.out.Push(buf, frames, c.meterCh(st))

		return nil
	}
	// Stereo is the format the design is defined for. Mono has nothing to
	// cross, so it passes through with only the standard gains applied.
	if st.ch < 2 {
		applyGains(buf, frames, st.ch, st.gains, func() {})
		c.out.Push(buf, frames, c.meterCh(st))

		return nil
	}

	applyGains(buf, frames, st.ch, st.gains, func() {
		n := frames * st.ch
		if n > len(buf) {
			n = len(buf)
		}
		n -= n % st.ch

		for i := 0; i < n; i += 2 {
			l := float64(buf[i])
			r := float64(buf[i+1])

			c.loL = st.a0Lo*l + st.b1Lo*c.loL
			c.loR = st.a0Lo*r + st.b1Lo*c.loR

			c.hiL = st.a0Hi*l + st.a1Hi*c.asisL + st.b1Hi*c.hiL
			c.hiR = st.a0Hi*r + st.a1Hi*c.asisR + st.b1Hi*c.hiR

			c.asisL = l
			c.asisR = r

			buf[i] = float32((c.hiL + c.loR) * st.gain)
			buf[i+1] = float32((c.hiR + c.loL) * st.gain)
		}
	})
	c.out.Push(buf, frames, c.meterCh(st))

	return nil
}

// meterCh is the channel count Push should read. The published state always
// carries a positive width: Process returns early when st is nil, and st is only
// published after Configure has validated ch >= 1, so the fallback is never
// reached. It is kept so a future call site that measures before Configure does
// not read as mono.
func (c *Crossfeed) meterCh(st *crossfeedState) int {
	if st.ch > 0 {
		return st.ch
	}

	return defaultChannels
}

// Reset clears the filter state so a seek cannot carry the previous position's
// tail into the new one. The published state stays as it is.
func (c *Crossfeed) Reset() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearStateLocked()
	c.in.Reset()
	c.out.Reset()

	return nil
}

// Meters reports the last input and output levels in dBFS. It is read from a
// control goroutine, which is why building the map here is harmless.
func (c *Crossfeed) Meters() map[string]float32 {
	return meterValues(c.in.Read(), c.out.Read())
}

// Bypassed reports the standard bypass parameter. It reads the published state
// rather than the store, so the chain can ask without taking a lock the audio
// thread might contend on.
func (c *Crossfeed) Bypassed() bool {
	st := c.state.Load()

	return st != nil && st.bypassed
}

func (c *Crossfeed) clearStateLocked() {
	c.loL, c.loR = 0, 0
	c.hiL, c.hiR = 0, 0
	c.asisL, c.asisR = 0, 0
}

// publishLocked rebuilds the audio-side snapshot. The caller holds mu.
func (c *Crossfeed) publishLocked() {
	if !c.isSet {
		return
	}
	feedTenths := int(math.Round(c.store.Float(CrossfeedFeed) * 10))
	cutoff := c.store.Float(CrossfeedCutoff)
	st := designCrossfeed(c.rate, cutoff, feedTenths)
	st.ch = c.ch
	st.bypassed = c.store.Bypassed()
	st.gains = compileGains(c.store)
	c.state.Store(st)
}

// designCrossfeed runs the bs2b design equations for one configuration.
//
// level is the feed in tenths of a dB. The equations below split it into a
// low-pass gain and a high-boost gain, place the high-boost corner so the two
// meet at a constant slope, and choose a normalising gain that cancels the
// bass lift. The result is a filter whose direct path stays flat while the
// crossed path is rolled off.
func designCrossfeed(rate int, cutoffHz float64, feedTenths int) *crossfeedState {
	level := float64(feedTenths) / 10.0

	gbLo := level*-5.0/6.0 - 3.0
	gbHi := level/6.0 - 3.0

	gLo := math.Pow(10, gbLo/20.0)
	gHi := 1.0 - math.Pow(10, gbHi/20.0)

	// The high-boost corner is placed so its slope through the band matches
	// the low-pass, which is what keeps the sum flat.
	fcHi := cutoffHz * math.Pow(2, (gbLo-20.0*math.Log10(gHi))/12.0)

	sr := float64(rate)

	x := math.Exp(-2.0 * math.Pi * cutoffHz / sr)
	b1Lo := x
	a0Lo := gLo * (1.0 - x)

	x = math.Exp(-2.0 * math.Pi * fcHi / sr)
	b1Hi := x
	a0Hi := 1.0 - gHi*(1.0-x)
	a1Hi := -x

	gain := 1.0 / (1.0 - gHi + gLo)

	return &crossfeedState{
		a0Lo:       a0Lo,
		b1Lo:       b1Lo,
		a0Hi:       a0Hi,
		a1Hi:       a1Hi,
		b1Hi:       b1Hi,
		gain:       gain,
		cutoffHz:   cutoffHz,
		feedTenths: feedTenths,
	}
}
