// Parametric equalizer implementation using RBJ cookbook biquad filters.
//
// Clean-room note: filter equations follow Robert Bristow-Johnson's public
// Audio EQ Cookbook and match EasyEffects APO/DR mode. This does not use or
// translate LSP RLC(BT) curve code, so response curves differ from RLC(BT).
// Split-channel processing is not implemented; all bands apply to both channels.

package dsp

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/dlcuy22/molo/core"
)

func init() {
	Register(NewEqualizerFactory())
}

// Equalizer parameter keys.
const (
	EqualizerNumBands = "num-bands"
)

const (
	equalizerMinBands     = 1
	equalizerMaxBands     = equalizerBandCount
	equalizerDefaultBands = 10
	equalizerBandCount    = 10
	equalizerDefaultQ     = 1.504760237537245

	equalizerMinFreq = 20.0
	equalizerMaxFreq = 20000.0
	equalizerMinGain = -20.0
	equalizerMaxGain = 20.0
	equalizerMinQ    = 0.1
	equalizerMaxQ    = 10.0
)

var defaultBandFrequencies = [equalizerBandCount]float64{
	32.0, 64.0, 125.0, 250.0, 500.0, 1000.0, 2000.0, 4000.0, 8000.0, 16000.0,
}

// EqualizerBandFreqKey formats the frequency parameter key for a band index.
func EqualizerBandFreqKey(i int) string {
	return fmt.Sprintf("band%d-frequency", i)
}

// EqualizerBandGainKey formats the gain parameter key for a band index.
func EqualizerBandGainKey(i int) string {
	return fmt.Sprintf("band%d-gain", i)
}

// EqualizerBandQKey formats the Q parameter key for a band index.
func EqualizerBandQKey(i int) string {
	return fmt.Sprintf("band%d-q", i)
}

// EqualizerFactory registers the equalizer effect implementation.
type EqualizerFactory struct{}

// NewEqualizerFactory returns a factory for the equalizer effect.
func NewEqualizerFactory() *EqualizerFactory { return &EqualizerFactory{} }

func (f *EqualizerFactory) Kind() string { return "equalizer" }

// Impl identifies the RBJ cookbook biquad implementation.
func (f *EqualizerFactory) Impl() string { return "equalizer-rbj" }

func (f *EqualizerFactory) FriendlyName() string { return "Equalizer" }

func (f *EqualizerFactory) Weight() int { return 50 }

func (f *EqualizerFactory) Placement() Placement { return Post }

func (f *EqualizerFactory) Schema() []Param {
	params := make([]Param, 0, 1+equalizerBandCount*3)
	params = append(params, Param{
		Key:     EqualizerNumBands,
		Kind:    Int,
		Min:     equalizerMinBands,
		Max:     equalizerMaxBands,
		Step:    1,
		Default: equalizerDefaultBands,
		Label:   "Bands",
		Group:   "Equalizer",
		Widget:  WidgetSlider,
	})

	for i := 0; i < equalizerBandCount; i++ {
		group := fmt.Sprintf("Band %d", i+1)
		params = append(params,
			Param{
				Key:     EqualizerBandFreqKey(i),
				Kind:    Float,
				Min:     equalizerMinFreq,
				Max:     equalizerMaxFreq,
				Step:    1.0,
				Unit:    "Hz",
				Default: defaultBandFrequencies[i],
				Label:   fmt.Sprintf("Band %d Frequency", i+1),
				Group:   group,
				Widget:  WidgetKnob,
			},
			Param{
				Key:     EqualizerBandGainKey(i),
				Kind:    Float,
				Min:     equalizerMinGain,
				Max:     equalizerMaxGain,
				Step:    0.1,
				Unit:    "dB",
				Default: 0.0,
				Label:   fmt.Sprintf("Band %d Gain", i+1),
				Group:   group,
				Widget:  WidgetKnob,
			},
			Param{
				Key:     EqualizerBandQKey(i),
				Kind:    Float,
				Min:     equalizerMinQ,
				Max:     equalizerMaxQ,
				Step:    0.01,
				Default: equalizerDefaultQ,
				Label:   fmt.Sprintf("Band %d Q", i+1),
				Group:   group,
				Widget:  WidgetKnob,
			},
		)
	}

	return withCommon(params)
}

func (f *EqualizerFactory) New(values Values) (Effect, error) {
	store, err := newParamStore(f.Schema(), values)
	if err != nil {
		return nil, err
	}

	return &Equalizer{store: store}, nil
}

// equalizerState is the immutable snapshot read by the audio path.
type equalizerState struct {
	coeffs    [equalizerBandCount][5]float64
	coeffsGen uint64
	numBands  int
	ch        int
	bypassed  bool
	gains     gainState
}

// Equalizer cascades peaking biquad filters in series.
type Equalizer struct {
	store *paramStore

	state atomic.Pointer[equalizerState]

	mu sync.Mutex

	rate  int
	ch    int
	isSet bool
	gen   uint64

	bands      []*biquadState
	appliedGen uint64

	in  Meter
	out Meter
}

var _ Effect = (*Equalizer)(nil)
var _ Metered = (*Equalizer)(nil)
var _ Bypassable = (*Equalizer)(nil)

func (e *Equalizer) Name() string { return "equalizer" }

func (e *Equalizer) Schema() []Param { return e.store.Schema() }

func (e *Equalizer) Get(key string) (any, error) { return e.store.Get(key) }

// Set stores one parameter and publishes a new snapshot for the audio thread.
func (e *Equalizer) Set(key string, value any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.store.Set(key, value); err != nil {
		return err
	}
	e.publishLocked()

	return nil
}

// Configure sets format and allocates biquad filters once.
func (e *Equalizer) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Rate <= 0 || in.Ch < 1 {
		return in, fmt.Errorf("dsp: equalizer needs a positive rate and channel count, got %+v", in)
	}
	if in.Fmt != core.F32 {
		return in, fmt.Errorf("dsp: equalizer needs float32 samples, got format %d", in.Fmt)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.rate = in.Rate
	e.ch = in.Ch
	e.isSet = true

	e.bands = make([]*biquadState, equalizerBandCount)
	for i := 0; i < equalizerBandCount; i++ {
		e.bands[i] = newBiquadState(e.ch)
	}
	e.publishLocked()

	return in, nil
}

// Process passes samples through active peaking filters without allocations or locks.
func (e *Equalizer) Process(buf []float32, frames int) error {
	st := e.state.Load()
	if st == nil {
		return nil
	}
	e.in.Push(buf, frames, e.meterCh(st))
	if st.bypassed {
		e.out.Push(buf, frames, e.meterCh(st))

		return nil
	}

	if e.appliedGen != st.coeffsGen {
		for b := 0; b < len(e.bands); b++ {
			c := st.coeffs[b]
			e.bands[b].setCoeffs(c[0], c[1], c[2], c[3], c[4])
		}
		e.appliedGen = st.coeffsGen
	}

	applyGains(buf, frames, st.ch, st.gains, func() {
		n := frames * st.ch
		if n > len(buf) {
			n = len(buf)
		}
		n -= n % st.ch

		active := st.numBands
		if active > len(e.bands) {
			active = len(e.bands)
		}

		if st.ch == 2 {
			for i := 0; i < n; i += 2 {
				l := float64(buf[i])
				r := float64(buf[i+1])
				for b := 0; b < active; b++ {
					band := e.bands[b]
					l = band.step(l, 0)
					r = band.step(r, 1)
				}
				buf[i] = float32(l)
				buf[i+1] = float32(r)
			}
		} else {
			for i := 0; i < n; i += st.ch {
				for c := 0; c < st.ch; c++ {
					x := float64(buf[i+c])
					for b := 0; b < active; b++ {
						x = e.bands[b].step(x, c)
					}
					buf[i+c] = float32(x)
				}
			}
		}
	})
	e.out.Push(buf, frames, e.meterCh(st))

	return nil
}

// Reset clears filter delay history across all bands.
func (e *Equalizer) Reset() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, b := range e.bands {
		if b != nil {
			b.reset()
		}
	}
	e.in.Reset()
	e.out.Reset()

	return nil
}

func (e *Equalizer) Meters() map[string]float32 {
	return meterValues(e.in.Read(), e.out.Read())
}

func (e *Equalizer) Bypassed() bool {
	st := e.state.Load()

	return st != nil && st.bypassed
}

func (e *Equalizer) meterCh(st *equalizerState) int {
	if st.ch > 0 {
		return st.ch
	}

	return defaultChannels
}

func (e *Equalizer) computeBandCoeffsLocked(idx int) [5]float64 {
	if idx < 0 || idx >= equalizerBandCount || e.rate <= 0 {
		return [5]float64{1, 0, 0, 0, 0}
	}
	freq := e.store.Float(EqualizerBandFreqKey(idx))
	gainDB := e.store.Float(EqualizerBandGainKey(idx))
	q := e.store.Float(EqualizerBandQKey(idx))

	maxFreq := float64(e.rate) * 0.499
	if freq > maxFreq {
		freq = maxFreq
	}
	if freq < equalizerMinFreq {
		freq = equalizerMinFreq
	}
	if q <= 0 {
		q = equalizerDefaultQ
	}

	coeffs, err := designBiquad("peaking", e.rate, freq, q, gainDB)
	if err != nil {
		return [5]float64{1, 0, 0, 0, 0}
	}

	return coeffs
}

func (e *Equalizer) publishLocked() {
	if !e.isSet {
		return
	}
	numBands := int(e.store.Float(EqualizerNumBands))
	if numBands < 1 {
		numBands = 1
	}
	if numBands > len(e.bands) {
		numBands = len(e.bands)
	}

	var coeffs [equalizerBandCount][5]float64
	for i := 0; i < equalizerBandCount; i++ {
		coeffs[i] = e.computeBandCoeffsLocked(i)
	}

	e.gen++
	st := &equalizerState{
		coeffs:    coeffs,
		coeffsGen: e.gen,
		numBands:  numBands,
		ch:        e.ch,
		bypassed:  e.store.Bypassed(),
		gains:     compileGains(e.store),
	}
	e.state.Store(st)
}
