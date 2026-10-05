// Lookahead limiter implementation.
//
// Clean-room note: this file implements the algorithm from public dynamics
// processing concepts. It is not a translation of LSP or any GPL/LGPL code.
//
// Limitation note: mode and oversampling parameters are accepted for preset
// compatibility but are not implemented; preset values are inert.

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
	Register(NewLimiterFactory())
}

// Limiter parameter keys matching preset names.
const (
	LimiterThreshold    = "threshold"
	LimiterAttack       = "attack"
	LimiterRelease      = "release"
	LimiterLookahead    = "lookahead"
	LimiterStereoLink   = "stereo-link"
	LimiterMode         = "mode"
	LimiterOversampling = "oversampling"
)

const (
	limiterMinThreshold = -30.0
	limiterMaxThreshold = 0.0

	limiterMinAttack = 0.1
	limiterMaxAttack = 50.0

	limiterMinRelease = 1.0
	limiterMaxRelease = 500.0

	limiterMinLookahead = 0.0
	limiterMaxLookahead = 20.0

	limiterMinStereoLink = 0.0
	limiterMaxStereoLink = 100.0
)

var limiterModeOptions = []string{
	"Herm Thin", "Herm Wide", "Herm Tail", "Herm Duck",
	"Exp Thin", "Exp Wide", "Exp Tail", "Exp Duck",
	"Line Thin", "Line Wide", "Line Tail", "Line Duck",
}

var limiterOversamplingOptions = []string{
	"None", "Half x2", "Half x4", "Full x2", "Full x4",
}

// LimiterFactory registers the lookahead limiter implementation.
type LimiterFactory struct{}

func NewLimiterFactory() *LimiterFactory { return &LimiterFactory{} }

func (f *LimiterFactory) Kind() string { return "limiter" }

func (f *LimiterFactory) Impl() string { return "limiter-lookahead" }

func (f *LimiterFactory) FriendlyName() string { return "Limiter" }

func (f *LimiterFactory) Weight() int { return 50 }

func (f *LimiterFactory) Placement() Placement { return Post }

func (f *LimiterFactory) Schema() []Param {
	return withCommon([]Param{
		{Key: LimiterThreshold, Kind: Float, Min: limiterMinThreshold, Max: limiterMaxThreshold, Step: 0.1, Unit: "dB", Default: 0.0, Label: "Threshold"},
		{Key: LimiterAttack, Kind: Float, Min: limiterMinAttack, Max: limiterMaxAttack, Step: 0.1, Unit: "ms", Default: 5.0, Label: "Attack"},
		{Key: LimiterRelease, Kind: Float, Min: limiterMinRelease, Max: limiterMaxRelease, Step: 1.0, Unit: "ms", Default: 5.0, Label: "Release"},
		{Key: LimiterLookahead, Kind: Float, Min: limiterMinLookahead, Max: limiterMaxLookahead, Step: 0.1, Unit: "ms", Default: 5.0, Label: "Lookahead"},
		{Key: LimiterStereoLink, Kind: Float, Min: limiterMinStereoLink, Max: limiterMaxStereoLink, Step: 1.0, Unit: "%", Default: 100.0, Label: "Stereo Link"},
		{Key: LimiterMode, Kind: Enum, Options: limiterModeOptions, Default: "Herm Thin", Label: "Mode"},
		{Key: LimiterOversampling, Kind: Enum, Options: limiterOversamplingOptions, Default: "None", Label: "Oversampling"},
	})
}

func (f *LimiterFactory) New(values Values) (Effect, error) {
	store, err := newParamStore(f.Schema(), values)
	if err != nil {
		return nil, err
	}

	return &Limiter{store: store}, nil
}

// limiterState is the immutable snapshot read by the audio thread.
type limiterState struct {
	thresholdLinear  float64
	attackCoeff      float64
	releaseCoeff     float64
	lookaheadSamples int
	stereoLink       float64

	ch       int
	rate     int
	bypassed bool
	gains    gainState
}

// Limiter is a lookahead brickwall dynamics processor.
type Limiter struct {
	store *paramStore

	state atomic.Pointer[limiterState]

	mu sync.Mutex

	rate  int
	ch    int
	isSet bool

	delay        *delayLine
	gainDelay    *delayLine
	envs         []*envState
	frameIn      []float64
	frameOut     []float64
	channelGains []float64
	delayedGains []float64

	in        Meter
	out       Meter
	reduction atomic.Uint32
}

var _ Effect = (*Limiter)(nil)
var _ Metered = (*Limiter)(nil)
var _ Bypassable = (*Limiter)(nil)
var _ Latent = (*Limiter)(nil)

func (l *Limiter) Name() string { return "limiter" }

func (l *Limiter) Schema() []Param { return l.store.Schema() }

func (l *Limiter) Get(key string) (any, error) { return l.store.Get(key) }

// Set validates and stores a parameter, then republishes the state snapshot.
func (l *Limiter) Set(key string, value any) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.store.Set(key, value); err != nil {
		return err
	}
	l.publishLocked()

	return nil
}

// Configure allocates scratch buffers and rebuilds filter state for the new format.
func (l *Limiter) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Rate <= 0 || in.Ch < 1 {
		return in, fmt.Errorf("dsp: limiter needs a positive rate and channel count, got %+v", in)
	}
	if in.Fmt != core.F32 {
		return in, fmt.Errorf("dsp: limiter needs float32 samples, got format %d", in.Fmt)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.rate = in.Rate
	l.ch = in.Ch
	l.isSet = true

	maxLookaheadFrames := int(math.Ceil(limiterMaxLookahead*float64(in.Rate)/1000.0)) + 1
	l.delay = newDelayLine(maxLookaheadFrames, in.Ch)
	l.gainDelay = newDelayLine(maxLookaheadFrames, in.Ch)
	l.envs = make([]*envState, in.Ch)
	attSec := l.store.Float(LimiterAttack) / 1000.0
	relSec := l.store.Float(LimiterRelease) / 1000.0
	for i := range l.envs {
		l.envs[i] = newEnvState(attSec, relSec, in.Rate, envPeak)
	}
	l.frameIn = make([]float64, in.Ch)
	l.frameOut = make([]float64, in.Ch)
	l.channelGains = make([]float64, in.Ch)
	l.delayedGains = make([]float64, in.Ch)

	l.publishLocked()
	l.clearStateLocked()

	return in, nil
}

// Process applies the lookahead limiter in place without allocations or locks.
func (l *Limiter) Process(buf []float32, frames int) error {
	st := l.state.Load()
	if st == nil {
		return nil
	}
	l.in.Push(buf, frames, l.meterCh(st))
	if st.bypassed {
		l.out.Push(buf, frames, l.meterCh(st))

		return nil
	}

	applyGains(buf, frames, st.ch, st.gains, func() {
		l.processAudio(buf, frames, st)
	})
	l.out.Push(buf, frames, l.meterCh(st))

	return nil
}

func (l *Limiter) processAudio(buf []float32, frames int, st *limiterState) {
	if l.delay == nil || l.gainDelay == nil || len(l.envs) < st.ch {
		return
	}
	for _, env := range l.envs {
		env.attack = st.attackCoeff
		env.release = st.releaseCoeff
	}

	n := frames * st.ch
	if n > len(buf) {
		n = len(buf)
	}
	n -= n % st.ch

	ch := st.ch
	delayFrames := st.lookaheadSamples
	if delayFrames >= l.delay.maxFrames {
		delayFrames = l.delay.maxFrames - 1
	}
	thresh := st.thresholdLinear
	threshF32 := float32(thresh)

	minAppliedGain := 1.0
	for i := 0; i < n; i += ch {
		for c := 0; c < ch; c++ {
			s := float64(buf[i+c])
			if math.IsNaN(s) || math.IsInf(s, 0) {
				s = 0
			}
			l.frameIn[c] = s
			peak := l.envs[c].step(s)
			if peak > thresh && peak > 0 {
				l.channelGains[c] = thresh / peak
			} else {
				l.channelGains[c] = 1.0
			}
		}

		if ch > 1 && st.stereoLink > 0 {
			minGain := l.channelGains[0]
			for c := 1; c < ch; c++ {
				if l.channelGains[c] < minGain {
					minGain = l.channelGains[c]
				}
			}
			for c := 0; c < ch; c++ {
				l.channelGains[c] += st.stereoLink * (minGain - l.channelGains[c])
			}
		}

		l.delay.process(l.frameIn, l.frameOut, delayFrames)
		l.gainDelay.process(l.channelGains, l.delayedGains, delayFrames)

		for c := 0; c < ch; c++ {
			g := l.delayedGains[c]
			if g < minAppliedGain {
				minAppliedGain = g
			}
			out := float32(l.frameOut[c] * g)
			if out > threshF32 {
				out = threshF32
			} else if out < -threshF32 {
				out = -threshF32
			}
			buf[i+c] = out
		}
	}

	var redDB float32
	if minAppliedGain < 1.0 && minAppliedGain > 0 {
		redDB = float32(20.0 * math.Log10(minAppliedGain))
	}
	l.reduction.Store(math.Float32bits(redDB))
}

// Reset clears internal filter state and zeroes meters.
func (l *Limiter) Reset() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.clearStateLocked()
	l.in.Reset()
	l.out.Reset()
	l.reduction.Store(0)

	return nil
}

// Meters reports input and output peak levels and gain reduction in dBFS.
func (l *Limiter) Meters() map[string]float32 {
	res := meterValues(l.in.Read(), l.out.Read())
	res[MeterReduction] = math.Float32frombits(l.reduction.Load())
	return res
}

// Bypassed reports whether the limiter is currently bypassed.
func (l *Limiter) Bypassed() bool {
	st := l.state.Load()

	return st != nil && st.bypassed
}

// Latency reports the lookahead delay as a duration.
func (l *Limiter) Latency() time.Duration {
	st := l.state.Load()
	if st == nil || st.rate <= 0 || st.lookaheadSamples <= 0 {
		return 0
	}

	return time.Duration(st.lookaheadSamples) * time.Second / time.Duration(st.rate)
}

func (l *Limiter) meterCh(st *limiterState) int {
	if st.ch > 0 {
		return st.ch
	}

	return defaultChannels
}

func (l *Limiter) clearStateLocked() {
	if l.delay != nil {
		l.delay.reset()
	}
	if l.gainDelay != nil {
		l.gainDelay.reset()
		for i := range l.gainDelay.buf {
			l.gainDelay.buf[i] = 1.0
		}
	}
	for _, env := range l.envs {
		if env != nil {
			env.reset()
		}
	}
	clear(l.frameIn)
	clear(l.frameOut)
	clear(l.channelGains)
	clear(l.delayedGains)
}

func (l *Limiter) publishLocked() {
	if !l.isSet {
		return
	}
	threshDB := l.store.Float(LimiterThreshold)
	attMs := l.store.Float(LimiterAttack)
	relMs := l.store.Float(LimiterRelease)
	lookMs := l.store.Float(LimiterLookahead)
	linkPct := l.store.Float(LimiterStereoLink)

	attSec := attMs / 1000.0
	relSec := relMs / 1000.0
	lookSec := lookMs / 1000.0
	lookSamples := int(math.Round(lookSec * float64(l.rate)))

	st := &limiterState{
		thresholdLinear:  dbToLinear(threshDB),
		attackCoeff:      envCoeff(attSec, l.rate),
		releaseCoeff:     envCoeff(relSec, l.rate),
		lookaheadSamples: lookSamples,
		stereoLink:       linkPct / 100.0,
		ch:               l.ch,
		rate:             l.rate,
		bypassed:         l.store.Bypassed(),
		gains:            compileGains(l.store),
	}
	l.state.Store(st)
}
