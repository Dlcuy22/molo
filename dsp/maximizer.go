// Lookahead peak maximizer implementation.
//
// Clean-room note: this file implements the lookahead peak limiter concept
// described in the DAFX'02 dynamics processing literature. It is not a
// translation of zam-plugins or any GPL/LGPL codebase.

package dsp

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlcuy22/molo/core"
)

func init() {
	Register(NewMaximizerFactory())
}

const (
	MaximizerThreshold = "threshold"
	MaximizerRelease   = "release"
	MaximizerCeiling   = "ceiling"
	MeterReduction     = "reduction"
)

const (
	maximizerMinThreshold = -36.0
	maximizerMaxThreshold = 0.0
	maximizerMinRelease   = 1.0
	maximizerMaxRelease   = 1000.0
	maximizerMinCeiling   = -12.0
	maximizerMaxCeiling   = 0.0

	maximizerLookaheadSec = 0.005
)

// MaximizerFactory creates instances of the lookahead peak maximizer.
type MaximizerFactory struct{}

func NewMaximizerFactory() *MaximizerFactory { return &MaximizerFactory{} }

func (f *MaximizerFactory) Kind() string { return "maximizer" }

func (f *MaximizerFactory) Impl() string { return "maximizer-dafx" }

func (f *MaximizerFactory) FriendlyName() string { return "Maximizer" }

func (f *MaximizerFactory) Weight() int { return 50 }

func (f *MaximizerFactory) Placement() Placement { return Post }

func (f *MaximizerFactory) Schema() []Param {
	return withCommon([]Param{
		{Key: MaximizerThreshold, Kind: Float, Min: maximizerMinThreshold, Max: maximizerMaxThreshold, Step: 0.1, Unit: "dB", Default: -3.0},
		{Key: MaximizerRelease, Kind: Float, Min: maximizerMinRelease, Max: maximizerMaxRelease, Step: 0.05, Unit: "ms", Default: 3.15},
		{Key: MaximizerCeiling, Kind: Float, Min: maximizerMinCeiling, Max: maximizerMaxCeiling, Step: 0.1, Unit: "dB", Default: 0.0},
	})
}

func (f *MaximizerFactory) New(values Values) (Effect, error) {
	store, err := newParamStore(f.Schema(), values)
	if err != nil {
		return nil, err
	}

	return &Maximizer{store: store}, nil
}

type maximizerState struct {
	ch              int
	rate            int
	bypassed        bool
	gains           gainState
	thresholdDB     float64
	thresholdLinear float64
	ceilingDB       float64
	ceilingLinear   float64
	releaseMs       float64
	lookahead       int
}

// Maximizer provides lookahead peak limiting with release smoothing and a hard ceiling clamp.
type Maximizer struct {
	store *paramStore

	state atomic.Pointer[maximizerState]

	mu sync.Mutex

	rate      int
	ch        int
	lookahead int
	isSet     bool

	delay     *delayLine
	gainDelay *delayLine
	env       *envState
	frameIn   []float64
	frameOut  []float64
	gainIn    []float64
	gainOut   []float64

	lastReleaseMs float64
	reduction     atomic.Uint32

	in  Meter
	out Meter
}

var _ Effect = (*Maximizer)(nil)
var _ Latent = (*Maximizer)(nil)
var _ Bypassable = (*Maximizer)(nil)
var _ Metered = (*Maximizer)(nil)

func (m *Maximizer) Name() string { return "maximizer" }

func (m *Maximizer) Schema() []Param { return m.store.Schema() }

func (m *Maximizer) Get(key string) (any, error) { return m.store.Get(key) }

func (m *Maximizer) Set(key string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.store.Set(key, value); err != nil {
		return err
	}
	m.publishLocked()

	return nil
}

func (m *Maximizer) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Rate <= 0 || in.Ch < 1 {
		return in, fmt.Errorf("dsp: maximizer needs a positive rate and channel count, got %+v", in)
	}
	if in.Fmt != core.F32 {
		return in, fmt.Errorf("dsp: maximizer needs float32 samples, got format %d", in.Fmt)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	lookahead := int(math.Round(maximizerLookaheadSec * float64(in.Rate)))
	if lookahead < 1 {
		lookahead = 1
	}

	m.rate = in.Rate
	m.ch = in.Ch
	m.lookahead = lookahead
	m.isSet = true

	m.delay = newDelayLine(lookahead+1, in.Ch)
	m.frameIn = make([]float64, in.Ch)
	m.frameOut = make([]float64, in.Ch)

	m.gainDelay = newDelayLine(lookahead+1, 1)
	m.resetGainDelayLocked()
	m.gainIn = make([]float64, 1)
	m.gainOut = make([]float64, 1)

	relMs := m.store.Float(MaximizerRelease)
	m.lastReleaseMs = relMs
	m.env = newEnvState(0, relMs/1000.0, in.Rate, envPeak)

	m.publishLocked()
	m.delay.reset()
	m.env.reset()

	return in, nil
}

func (m *Maximizer) Process(buf []float32, frames int) error {
	st := m.state.Load()
	if st == nil {
		return nil
	}
	ch := m.meterCh(st)
	m.in.Push(buf, frames, ch)
	if st.bypassed {
		m.out.Push(buf, frames, ch)
		m.reduction.Store(0)
		return nil
	}

	// Update release smoothing on the audio thread when control values change.
	if st.releaseMs != m.lastReleaseMs {
		m.env.setAttackRelease(0, st.releaseMs/1000.0, st.rate)
		m.lastReleaseMs = st.releaseMs
	}

	applyGains(buf, frames, st.ch, st.gains, func() {
		n := frames * st.ch
		if n > len(buf) {
			n = len(buf)
		}
		n -= n % st.ch

		minGain := 1.0
		for i := 0; i < n; i += st.ch {
			var inPeak float64
			for c := 0; c < st.ch; c++ {
				s := float64(buf[i+c])
				if math.IsNaN(s) || math.IsInf(s, 0) {
					s = 0
				}
				m.frameIn[c] = s
				ax := math.Abs(s)
				if ax > inPeak {
					inPeak = ax
				}
			}

			m.delay.process(m.frameIn, m.frameOut, st.lookahead)

			envVal := m.env.step(inPeak)
			gain := 1.0
			if envVal > st.thresholdLinear && envVal > 0 {
				gain = st.thresholdLinear / envVal
			}

			m.gainIn[0] = gain
			m.gainDelay.process(m.gainIn, m.gainOut, st.lookahead)
			gDelayed := m.gainOut[0]
			if gDelayed < minGain {
				minGain = gDelayed
			}

			for c := 0; c < st.ch; c++ {
				out := m.frameOut[c] * gDelayed
				if out > st.ceilingLinear {
					out = st.ceilingLinear
				} else if out < -st.ceilingLinear {
					out = -st.ceilingLinear
				}
				buf[i+c] = float32(out)
			}
		}

		var redDB float32
		if minGain < 1.0 && minGain > 0 {
			redDB = float32(20.0 * math.Log10(minGain))
		}
		m.reduction.Store(math.Float32bits(redDB))
	})

	m.out.Push(buf, frames, ch)
	return nil
}

func (m *Maximizer) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.delay != nil {
		m.delay.reset()
	}
	if m.gainDelay != nil {
		m.gainDelay.reset()
		m.resetGainDelayLocked()
	}
	if m.env != nil {
		m.env.reset()
	}
	m.in.Reset()
	m.out.Reset()
	m.reduction.Store(0)

	return nil
}

func (m *Maximizer) Latency() time.Duration {
	st := m.state.Load()
	if st == nil || st.rate <= 0 {
		return 0
	}

	return time.Duration(st.lookahead) * time.Second / time.Duration(st.rate)
}

func (m *Maximizer) Bypassed() bool {
	st := m.state.Load()

	return st != nil && st.bypassed
}

func (m *Maximizer) Meters() map[string]float32 {
	res := meterValues(m.in.Read(), m.out.Read())
	res[MeterReduction] = math.Float32frombits(m.reduction.Load())
	return res
}

func (m *Maximizer) meterCh(st *maximizerState) int {
	if st.ch > 0 {
		return st.ch
	}

	return defaultChannels
}

func (m *Maximizer) resetGainDelayLocked() {
	for i := range m.gainDelay.buf {
		m.gainDelay.buf[i] = 1.0
	}
}

func (m *Maximizer) publishLocked() {
	if !m.isSet {
		return
	}
	threshDB := m.store.Float(MaximizerThreshold)
	relMs := m.store.Float(MaximizerRelease)
	ceilDB := m.store.Float(MaximizerCeiling)

	st := &maximizerState{
		ch:              m.ch,
		rate:            m.rate,
		bypassed:        m.store.Bypassed(),
		gains:           compileGains(m.store),
		thresholdDB:     threshDB,
		thresholdLinear: dbToLinear(threshDB),
		ceilingDB:       ceilDB,
		ceilingLinear:   dbToLinear(ceilDB),
		releaseMs:       relMs,
		lookahead:       m.lookahead,
	}
	m.state.Store(st)
}
