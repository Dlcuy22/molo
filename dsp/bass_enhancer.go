// Bass enhancer implementation.
//
// Bass enhancer generates higher harmonics from the low-frequency band based on
// the missing-fundamental psychoacoustic principle, allowing low bass to be
// perceived on speakers and headphones that cannot reproduce deep fundamentals.
//
// Clean-room note: this file implements the missing-fundamental harmonic
// synthesis concept from published psychoacoustic literature. It is not derived
// from Calf, TAP Tubewarmth, or any other GPL/LGPL source.

package dsp

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/dlcuy22/molo/core"
)

func init() {
	Register(NewBassEnhancerFactory())
}

const (
	BassEnhancerAmount      = "amount"
	BassEnhancerHarmonics   = "harmonics"
	BassEnhancerScope       = "scope"
	BassEnhancerFloor       = "floor"
	BassEnhancerFloorActive = "floor-active"
	BassEnhancerBlend       = "blend"
	BassEnhancerListen      = "listen"
)

const (
	bassEnhancerMinAmount    = -30.0
	bassEnhancerMaxAmount    = 20.0
	bassEnhancerMinHarmonics = 0.0
	bassEnhancerMaxHarmonics = 20.0
	bassEnhancerMinScope     = 50.0
	bassEnhancerMaxScope     = 500.0
	bassEnhancerMinFloor     = 10.0
	bassEnhancerMaxFloor     = 200.0
	bassEnhancerMinBlend     = 0.0
	bassEnhancerMaxBlend     = 1.0
)

type BassEnhancerFactory struct{}

func NewBassEnhancerFactory() *BassEnhancerFactory { return &BassEnhancerFactory{} }

func (f *BassEnhancerFactory) Kind() string { return "bass-enhancer" }

func (f *BassEnhancerFactory) Impl() string { return "bass-enhancer-harmonic" }

func (f *BassEnhancerFactory) FriendlyName() string { return "Bass Enhancer" }

func (f *BassEnhancerFactory) Weight() int { return 50 }

func (f *BassEnhancerFactory) Placement() Placement { return Post }

func (f *BassEnhancerFactory) Schema() []Param {
	return withCommon([]Param{
		{Key: BassEnhancerAmount, Kind: Float, Min: bassEnhancerMinAmount, Max: bassEnhancerMaxAmount, Step: 0.1, Unit: "dB", Default: 5.0, Label: "Amount", Widget: WidgetSlider},
		{Key: BassEnhancerHarmonics, Kind: Float, Min: bassEnhancerMinHarmonics, Max: bassEnhancerMaxHarmonics, Step: 0.1, Unit: "dB", Default: 8.5, Label: "Harmonics", Widget: WidgetSlider},
		{Key: BassEnhancerScope, Kind: Float, Min: bassEnhancerMinScope, Max: bassEnhancerMaxScope, Step: 1, Unit: "Hz", Default: 80.0, Label: "Scope", Widget: WidgetSlider},
		{Key: BassEnhancerFloor, Kind: Float, Min: bassEnhancerMinFloor, Max: bassEnhancerMaxFloor, Step: 1, Unit: "Hz", Default: 20.0, Label: "Floor", Widget: WidgetSlider},
		{Key: BassEnhancerFloorActive, Kind: Bool, Default: false, Label: "Floor Active", Widget: WidgetSwitch},
		{Key: BassEnhancerBlend, Kind: Float, Min: bassEnhancerMinBlend, Max: bassEnhancerMaxBlend, Step: 0.01, Default: 0.0, Label: "Blend", Widget: WidgetSlider},
		{Key: BassEnhancerListen, Kind: Bool, Default: false, Label: "Listen", Widget: WidgetSwitch},
	})
}

func (f *BassEnhancerFactory) New(values Values) (Effect, error) {
	store, err := newParamStore(f.Schema(), values)
	if err != nil {
		return nil, err
	}

	return &BassEnhancer{store: store}, nil
}

type bassEnhancerState struct {
	ch          int
	bypassed    bool
	gains       gainState
	amount      float64
	amountGain  float64
	drive       float64
	blend       float64
	listen      bool
	floorActive bool
	lpCoeffs    [5]float64
	hpCoeffs    [5]float64
	coeffsGen   uint64
}

type BassEnhancer struct {
	store *paramStore
	state atomic.Pointer[bassEnhancerState]
	mu    sync.Mutex

	rate  int
	ch    int
	isSet bool

	activeGen uint64
	genSeq    uint64

	lpFilter *biquadState
	hpFilter *biquadState

	in  Meter
	out Meter
}

var _ Effect = (*BassEnhancer)(nil)
var _ Metered = (*BassEnhancer)(nil)
var _ Bypassable = (*BassEnhancer)(nil)

func (b *BassEnhancer) Name() string { return "bass-enhancer" }

func (b *BassEnhancer) Schema() []Param { return b.store.Schema() }

func (b *BassEnhancer) Get(key string) (any, error) { return b.store.Get(key) }

func (b *BassEnhancer) Set(key string, value any) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.store.Set(key, value); err != nil {
		return err
	}
	b.publishLocked()

	return nil
}

func (b *BassEnhancer) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Rate <= 0 || in.Ch < 1 {
		return in, fmt.Errorf("dsp: bass-enhancer needs a positive rate and channel count, got %+v", in)
	}
	if in.Fmt != core.F32 {
		return in, fmt.Errorf("dsp: bass-enhancer needs float32 samples, got format %d", in.Fmt)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.rate = in.Rate
	b.ch = in.Ch
	b.isSet = true

	b.lpFilter = newBiquadState(b.ch)
	b.hpFilter = newBiquadState(b.ch)
	b.activeGen = 0

	b.publishLocked()

	return in, nil
}

func (b *BassEnhancer) Process(buf []float32, frames int) error {
	st := b.state.Load()
	if st == nil {
		return nil
	}

	b.in.Push(buf, frames, b.meterCh(st))
	if st.bypassed {
		b.out.Push(buf, frames, b.meterCh(st))

		return nil
	}

	if b.activeGen != st.coeffsGen && b.lpFilter != nil && b.hpFilter != nil {
		b.lpFilter.setCoeffs(st.lpCoeffs[0], st.lpCoeffs[1], st.lpCoeffs[2], st.lpCoeffs[3], st.lpCoeffs[4])
		b.hpFilter.setCoeffs(st.hpCoeffs[0], st.hpCoeffs[1], st.hpCoeffs[2], st.hpCoeffs[3], st.hpCoeffs[4])
		b.activeGen = st.coeffsGen
	}

	applyGains(buf, frames, st.ch, st.gains, func() {
		n := frames * st.ch
		if n > len(buf) {
			n = len(buf)
		}
		n -= n % st.ch

		for i := 0; i < n; i += st.ch {
			for c := 0; c < st.ch; c++ {
				orig := float64(buf[i+c])
				bass := b.lpFilter.step(orig, c)

				harm := bassEnhanceShaper(bass, st.drive, st.blend)

				if st.floorActive {
					harm = b.hpFilter.step(harm, c)
				}

				var out float64
				if st.listen {
					out = harm * st.amountGain
				} else {
					out = orig + harm*st.amountGain
				}
				buf[i+c] = float32(out)
			}
		}
	})

	b.out.Push(buf, frames, b.meterCh(st))

	return nil
}

func (b *BassEnhancer) meterCh(st *bassEnhancerState) int {
	if st.ch > 0 {
		return st.ch
	}

	return defaultChannels
}

func (b *BassEnhancer) Reset() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.lpFilter != nil {
		b.lpFilter.reset()
	}
	if b.hpFilter != nil {
		b.hpFilter.reset()
	}
	b.in.Reset()
	b.out.Reset()

	return nil
}

func (b *BassEnhancer) Meters() map[string]float32 {
	return meterValues(b.in.Read(), b.out.Read())
}

func (b *BassEnhancer) Bypassed() bool {
	st := b.state.Load()

	return st != nil && st.bypassed
}

func (b *BassEnhancer) publishLocked() {
	if !b.isSet {
		return
	}

	amount := b.store.Float(BassEnhancerAmount)
	harmonics := b.store.Float(BassEnhancerHarmonics)
	scope := b.store.Float(BassEnhancerScope)
	floor := b.store.Float(BassEnhancerFloor)
	floorActive := b.store.Bool(BassEnhancerFloorActive)
	blend := b.store.Float(BassEnhancerBlend)
	listen := b.store.Bool(BassEnhancerListen)

	lpCoeffs, err := designBiquad("lowpass", b.rate, scope, 0.707, 0)
	if err != nil {
		lpCoeffs = [5]float64{1, 0, 0, 0, 0}
	}

	hpCoeffs, err := designBiquad("highpass", b.rate, floor, 0.707, 0)
	if err != nil {
		hpCoeffs = [5]float64{1, 0, 0, 0, 0}
	}

	b.genSeq++

	amountGain := dbToLinear(amount)

	st := &bassEnhancerState{
		ch:          b.ch,
		bypassed:    b.store.Bypassed(),
		gains:       compileGains(b.store),
		amount:      amount,
		amountGain:  amountGain,
		drive:       dbToLinear(harmonics),
		blend:       blend,
		listen:      listen,
		floorActive: floorActive,
		lpCoeffs:    lpCoeffs,
		hpCoeffs:    hpCoeffs,
		coeffsGen:   b.genSeq,
	}

	b.state.Store(st)
}

func bassEnhanceShaper(bass, drive, blend float64) float64 {
	x := bass * drive
	// Clamp x into a reasonable range for numerical stability.
	if x > 1.0 {
		x = 1.0
	} else if x < -1.0 {
		x = -1.0
	}

	// Even harmonic generator producing 2x fundamental with zero fundamental leakage.
	evenTerm := 2.0*x*x - math.Abs(x)

	// Chebyshev polynomial generating 3x harmonic with minimal fundamental leakage.
	oddTerm := 4.0*x*x*x - 3.0*x

	// Tanh saturation keeps both harmonic streams bounded.
	hEven := math.Tanh(evenTerm)
	hOdd := math.Tanh(oddTerm)

	wEven := 1.0 - 0.5*blend
	wOdd := 0.5 + 0.5*blend

	return 0.5 * (wEven*hEven + wOdd*hOdd)
}
