// Clean-room note: this file implements uniform-partitioned overlap-add
// convolution from published signal processing literature. It is an
// independent clean-room implementation and is not a translation of
// zita-convolver, EasyEffects, or any GPL/LGPL codebase.

package dsp

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlcuy22/molo/core"
)

func init() {
	Register(NewConvolverFactory())
}

// Convolver parameter keys.
const (
	ConvolverKernel   = "kernel-name"
	ConvolverIRWidth  = "ir-width"
	ConvolverAutogain = "autogain"
)

const (
	convolverBlockSize = 512
	convolverFFTSize   = 1024
	convolverBins      = 513
	convolverRingCap   = 256
	convolverMinWidth  = 0.0
	convolverMaxWidth  = 200.0
)

// ConvolverFactory registers the partitioned convolver.
type ConvolverFactory struct{}

// NewConvolverFactory returns a factory for the convolver effect.
func NewConvolverFactory() *ConvolverFactory { return &ConvolverFactory{} }

func (f *ConvolverFactory) Kind() string { return "convolver" }

func (f *ConvolverFactory) Impl() string { return "convolver-partitioned" }

func (f *ConvolverFactory) FriendlyName() string { return "Convolver" }

func (f *ConvolverFactory) Weight() int { return 50 }

func (f *ConvolverFactory) Placement() Placement { return Post }

func (f *ConvolverFactory) Schema() []Param {
	names := kernels()
	defKernel := "identity"
	if len(names) > 0 && !slices.Contains(names, defKernel) {
		defKernel = names[0]
	}

	return withCommon([]Param{
		{
			Key:     ConvolverKernel,
			Kind:    Enum,
			Options: names,
			Default: defKernel,
			Label:   "Kernel",
			Widget:  WidgetSelect,
		},
		{
			Key:     ConvolverIRWidth,
			Kind:    Float,
			Min:     convolverMinWidth,
			Max:     convolverMaxWidth,
			Step:    1,
			Unit:    "%",
			Default: 100.0,
			Label:   "IR Width",
			Widget:  WidgetSlider,
		},
		{
			Key:     ConvolverAutogain,
			Kind:    Bool,
			Default: false,
			Label:   "Auto Gain",
			Widget:  WidgetSwitch,
		},
	})
}

func (f *ConvolverFactory) New(values Values) (Effect, error) {
	clonedValues := make(Values, len(values)+1)
	for k, v := range values {
		clonedValues[k] = v
	}
	if v, ok := clonedValues["kernel"]; ok && clonedValues[ConvolverKernel] == nil {
		clonedValues[ConvolverKernel] = v
		delete(clonedValues, "kernel")
	}

	kVal := clonedValues[ConvolverKernel]
	var kernelName string
	if kVal != nil {
		s, ok := kVal.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %q wants a string, got %T", ErrUnknownParam, ConvolverKernel, kVal)
		}
		kernelName = s
	} else {
		kernelName = "identity"
		clonedValues[ConvolverKernel] = kernelName
	}

	if _, err := getKernel(kernelName); err != nil {
		return nil, fmt.Errorf("%w: unknown kernel %q", ErrUnknownParam, kernelName)
	}

	store, err := newParamStore(f.Schema(), clonedValues)
	if err != nil {
		return nil, err
	}

	return &Convolver{
		store: store,
	}, nil
}

// convolverState is the immutable snapshot read by the audio thread.
type convolverState struct {
	H_L [][]complex64
	H_R [][]complex64

	numPartitions int
	rate          int
	ch            int
	bypassed      bool
	gains         gainState

	kernel   string
	width    float64
	autogain bool
}

// Convolver implements uniform-partitioned overlap-add convolution.
type Convolver struct {
	store *paramStore
	state atomic.Pointer[convolverState]
	mu    sync.Mutex

	rate  int
	ch    int
	isSet bool

	in  Meter
	out Meter

	fftL *fftPlan
	fftR *fftPlan

	ringL    [][]complex64
	ringR    [][]complex64
	ringHead int

	tailL []float32
	tailR []float32

	inBuf    []float32
	inFrames int

	outBuf  []float32
	outHead int

	timeIn      []float32
	timeOut     []float32
	specX       []complex64
	specAccum   []complex64
	specProd    []complex64
	scratchOutL []float32
	scratchOutR []float32
}

var _ Effect = (*Convolver)(nil)
var _ Latent = (*Convolver)(nil)
var _ Bypassable = (*Convolver)(nil)
var _ Metered = (*Convolver)(nil)

func (c *Convolver) Name() string { return "convolver" }

func (c *Convolver) Schema() []Param { return c.store.Schema() }

func (c *Convolver) Get(key string) (any, error) {
	if key == "kernel" {
		key = ConvolverKernel
	}

	return c.store.Get(key)
}

func (c *Convolver) Set(key string, value any) error {
	if key == "kernel" {
		key = ConvolverKernel
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if key == ConvolverKernel {
		name, ok := value.(string)
		if !ok {
			return fmt.Errorf("%w: %q wants a string, got %T", ErrUnknownParam, key, value)
		}
		if _, err := getKernel(name); err != nil {
			return err
		}
	}

	if err := c.store.Set(key, value); err != nil {
		if errors.Is(err, ErrUnknownParam) && key == ConvolverKernel {
			vals := c.store.Values()
			vals[key] = value
			newStore, storeErr := newParamStore(NewConvolverFactory().Schema(), vals)
			if storeErr != nil {
				return storeErr
			}
			c.store = newStore
		} else {
			return err
		}
	}

	if c.isSet {
		if err := c.rebuildLocked(); err != nil {
			return err
		}
	}

	return nil
}

func (c *Convolver) Configure(in core.FrameFormat) (core.FrameFormat, error) {
	if in.Rate <= 0 || in.Ch < 1 {
		return in, fmt.Errorf("dsp: convolver needs a positive rate and channel count, got %+v", in)
	}
	if in.Fmt != core.F32 {
		return in, fmt.Errorf("dsp: convolver needs float32 samples, got format %d", in.Fmt)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.rate = in.Rate
	c.ch = in.Ch
	c.isSet = true

	c.allocateBuffersLocked()
	if err := c.rebuildLocked(); err != nil {
		return in, err
	}
	c.clearBuffersLocked()

	return in, nil
}

func (c *Convolver) allocateBuffersLocked() {
	if c.fftL == nil {
		c.fftL, _ = newFFT(convolverFFTSize)
		c.fftR, _ = newFFT(convolverFFTSize)
	}
	if len(c.ringL) < convolverRingCap {
		c.ringL = make([][]complex64, convolverRingCap)
		c.ringR = make([][]complex64, convolverRingCap)
		for i := range c.ringL {
			c.ringL[i] = make([]complex64, convolverBins)
			c.ringR[i] = make([]complex64, convolverBins)
		}
	}
	if len(c.tailL) != convolverBlockSize {
		c.tailL = make([]float32, convolverBlockSize)
		c.tailR = make([]float32, convolverBlockSize)
	}
	bufFrames := convolverBlockSize * c.ch
	if len(c.inBuf) != bufFrames {
		c.inBuf = make([]float32, bufFrames)
		c.outBuf = make([]float32, bufFrames)
	}
	if len(c.timeIn) != convolverFFTSize {
		c.timeIn = make([]float32, convolverFFTSize)
		c.timeOut = make([]float32, convolverFFTSize)
		c.specX = make([]complex64, convolverBins)
		c.specAccum = make([]complex64, convolverBins)
		c.specProd = make([]complex64, convolverBins)
		c.scratchOutL = make([]float32, convolverBlockSize)
		c.scratchOutR = make([]float32, convolverBlockSize)
	}
}

func (c *Convolver) Process(buf []float32, frames int) error {
	st := c.state.Load()
	if st == nil {
		return nil
	}

	c.in.Push(buf, frames, c.meterCh(st))
	if st.bypassed {
		c.out.Push(buf, frames, c.meterCh(st))

		return nil
	}

	applyGains(buf, frames, st.ch, st.gains, func() {
		c.processBuffer(buf, frames, st)
	})

	c.out.Push(buf, frames, c.meterCh(st))

	return nil
}

func (c *Convolver) processBuffer(buf []float32, frames int, st *convolverState) {
	ch := st.ch
	if ch < 1 {
		return
	}
	totalSamples := frames * ch
	if totalSamples > len(buf) {
		totalSamples = len(buf)
	}
	totalSamples -= totalSamples % ch
	validFrames := totalSamples / ch

	framesRemaining := validFrames
	offset := 0

	for framesRemaining > 0 {
		chunk := min(framesRemaining, convolverBlockSize-c.inFrames)

		inDst := c.inBuf[c.inFrames*ch : (c.inFrames+chunk)*ch]
		copy(inDst, buf[offset*ch:(offset+chunk)*ch])

		outSrc := c.outBuf[c.outHead*ch : (c.outHead+chunk)*ch]
		copy(buf[offset*ch:(offset+chunk)*ch], outSrc)

		c.outHead += chunk
		c.inFrames += chunk
		offset += chunk
		framesRemaining -= chunk

		if c.inFrames == convolverBlockSize {
			c.convolveBlock(st)
			c.outHead = 0
			c.inFrames = 0
		}
	}
}

func (c *Convolver) convolveBlock(st *convolverState) {
	ch := st.ch
	if ch == 1 {
		copy(c.timeIn[:convolverBlockSize], c.inBuf[:convolverBlockSize])
		clear(c.timeIn[convolverBlockSize:])

		c.fftL.forward(c.specX, c.timeIn)
		copy(c.ringL[c.ringHead], c.specX)
		clear(c.specAccum)

		numP := min(st.numPartitions, len(c.ringL))
		for p := 0; p < numP; p++ {
			idx := (c.ringHead - p) % len(c.ringL)
			if idx < 0 {
				idx += len(c.ringL)
			}
			mulComplex(c.specProd, c.ringL[idx], st.H_L[p])
			addComplex(c.specAccum, c.specAccum, c.specProd)
		}

		c.fftL.inverse(c.timeOut, c.specAccum)
		for n := 0; n < convolverBlockSize; n++ {
			c.outBuf[n] = c.timeOut[n] + c.tailL[n]
			c.tailL[n] = c.timeOut[convolverBlockSize+n]
		}
		c.ringHead = (c.ringHead + 1) % len(c.ringL)

		return
	}

	for n := 0; n < convolverBlockSize; n++ {
		c.timeIn[n] = c.inBuf[n*ch]
	}
	clear(c.timeIn[convolverBlockSize:])

	c.fftL.forward(c.specX, c.timeIn)
	copy(c.ringL[c.ringHead], c.specX)
	clear(c.specAccum)

	numP := min(st.numPartitions, len(c.ringL))
	for p := 0; p < numP; p++ {
		idx := (c.ringHead - p) % len(c.ringL)
		if idx < 0 {
			idx += len(c.ringL)
		}
		mulComplex(c.specProd, c.ringL[idx], st.H_L[p])
		addComplex(c.specAccum, c.specAccum, c.specProd)
	}

	c.fftL.inverse(c.timeOut, c.specAccum)
	for n := 0; n < convolverBlockSize; n++ {
		c.scratchOutL[n] = c.timeOut[n] + c.tailL[n]
		c.tailL[n] = c.timeOut[convolverBlockSize+n]
	}

	for n := 0; n < convolverBlockSize; n++ {
		c.timeIn[n] = c.inBuf[n*ch+1]
	}
	clear(c.timeIn[convolverBlockSize:])

	c.fftR.forward(c.specX, c.timeIn)
	copy(c.ringR[c.ringHead], c.specX)
	clear(c.specAccum)

	for p := 0; p < numP; p++ {
		idx := (c.ringHead - p) % len(c.ringR)
		if idx < 0 {
			idx += len(c.ringR)
		}
		mulComplex(c.specProd, c.ringR[idx], st.H_R[p])
		addComplex(c.specAccum, c.specAccum, c.specProd)
	}

	c.fftR.inverse(c.timeOut, c.specAccum)
	for n := 0; n < convolverBlockSize; n++ {
		c.scratchOutR[n] = c.timeOut[n] + c.tailR[n]
		c.tailR[n] = c.timeOut[convolverBlockSize+n]
	}

	for n := 0; n < convolverBlockSize; n++ {
		c.outBuf[n*ch] = c.scratchOutL[n]
		c.outBuf[n*ch+1] = c.scratchOutR[n]
	}
	if ch > 2 {
		for n := 0; n < convolverBlockSize; n++ {
			for extra := 2; extra < ch; extra++ {
				c.outBuf[n*ch+extra] = 0
			}
		}
	}

	c.ringHead = (c.ringHead + 1) % len(c.ringL)
}

func (c *Convolver) Reset() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.clearBuffersLocked()
	c.in.Reset()
	c.out.Reset()

	return nil
}

func (c *Convolver) Meters() map[string]float32 {
	return meterValues(c.in.Read(), c.out.Read())
}

func (c *Convolver) Bypassed() bool {
	st := c.state.Load()

	return st != nil && st.bypassed
}

func (c *Convolver) Latency() time.Duration {
	rate := c.rate
	st := c.state.Load()
	if st != nil && st.rate > 0 {
		rate = st.rate
	}
	if rate <= 0 {
		return 0
	}

	return time.Duration(convolverBlockSize) * time.Second / time.Duration(rate)
}

func (c *Convolver) meterCh(st *convolverState) int {
	if st.ch > 0 {
		return st.ch
	}

	return defaultChannels
}

func (c *Convolver) clearBuffersLocked() {
	c.ringHead = 0
	for i := range c.ringL {
		clear(c.ringL[i])
	}
	for i := range c.ringR {
		clear(c.ringR[i])
	}
	clear(c.tailL)
	clear(c.tailR)
	clear(c.inBuf)
	clear(c.outBuf)
	c.inFrames = 0
	c.outHead = 0
}

func (c *Convolver) rebuildLocked() error {
	kernelVal, err := c.store.Get(ConvolverKernel)
	if err != nil {
		return err
	}
	kernelName, ok := kernelVal.(string)
	if !ok {
		return fmt.Errorf("%w: %q wants string", ErrUnknownParam, ConvolverKernel)
	}

	ir, err := getKernel(kernelName)
	if err != nil {
		return err
	}

	samples, frames := resampleLinear(ir.samples, ir.frames, ir.sampleRate, c.rate)

	width := c.store.Float(ConvolverIRWidth)
	applyIRWidth(samples, width)

	autogain := c.store.Bool(ConvolverAutogain)
	if autogain {
		applyIRAutogain(samples)
	}

	numPartitions := (frames + convolverBlockSize - 1) / convolverBlockSize
	if numPartitions < 1 {
		numPartitions = 1
	}

	plan, err := newFFT(convolverFFTSize)
	if err != nil {
		return err
	}

	H_L := make([][]complex64, numPartitions)
	H_R := make([][]complex64, numPartitions)
	timeBlock := make([]float32, convolverFFTSize)

	for p := 0; p < numPartitions; p++ {
		H_L[p] = make([]complex64, convolverBins)
		H_R[p] = make([]complex64, convolverBins)

		clear(timeBlock)
		start := p * convolverBlockSize
		n := min(convolverBlockSize, frames-start)
		for i := 0; i < n; i++ {
			timeBlock[i] = samples[(start+i)*2]
		}
		plan.forward(H_L[p], timeBlock)

		clear(timeBlock)
		for i := 0; i < n; i++ {
			timeBlock[i] = samples[(start+i)*2+1]
		}
		plan.forward(H_R[p], timeBlock)
	}

	st := &convolverState{
		H_L:           H_L,
		H_R:           H_R,
		numPartitions: numPartitions,
		rate:          c.rate,
		ch:            c.ch,
		bypassed:      c.store.Bypassed(),
		gains:         compileGains(c.store),
		kernel:        kernelName,
		width:         width,
		autogain:      autogain,
	}
	c.state.Store(st)

	return nil
}
