package dsp

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
)

func convTestDirectConvolve(input, ir []float32, ch int) []float32 {
	framesIn := len(input) / ch
	framesIR := len(ir) / ch
	framesOut := framesIn + framesIR - 1
	out := make([]float32, framesOut*ch)

	for c := 0; c < ch; c++ {
		for n := 0; n < framesIn; n++ {
			x := input[n*ch+c]
			if x == 0 {
				continue
			}
			for m := 0; m < framesIR; m++ {
				out[(n+m)*ch+c] += x * ir[m*ch+c]
			}
		}
	}

	return out
}

func convTestNewConvolver(t *testing.T, kernelName string, values Values) *Convolver {
	t.Helper()

	v := Values{ConvolverKernel: kernelName}
	for k, val := range values {
		v[k] = val
	}
	effect, err := NewConvolverFactory().New(v)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := effect.(*Convolver)
	if _, err := c.Configure(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	return c
}

func TestConvolverSelfParityImpulse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "impulse.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0}, 48000, 2, 3, 32)
	if err := RegisterKernel("conv-parity-impulse", path); err != nil {
		t.Fatal(err)
	}

	cf := convTestNewConvolver(t, "conv-parity-impulse", nil)
	const frames = 4800
	input := sineStereo(frames, 48000, 1000)
	got := make([]float32, len(input))
	copy(got, input)

	renderEffectChunks(t, cf, got, 2, 480)

	const b = convolverBlockSize
	gotActive := got[b*2:]
	wantActive := input[:(frames-b)*2]

	nullDB := parityNullDB(gotActive, wantActive)
	t.Logf("impulse parity null: %.2f dB", nullDB)
	if nullDB > -120.0 {
		t.Fatalf("impulse parity null %.2f dB exceeds tolerance -120 dB", nullDB)
	}
}

func TestConvolverSelfParityLongIR(t *testing.T) {
	const irFrames = 2000
	ir := make([]float32, irFrames*2)
	for i := 0; i < irFrames; i++ {
		decay := float32(math.Exp(-float64(i) / 400.0))
		ir[2*i] = float32(0.5*math.Sin(2*math.Pi*200.0*float64(i)/48000.0)) * decay
		ir[2*i+1] = float32(0.5*math.Cos(2*math.Pi*300.0*float64(i)/48000.0)) * decay
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "long_ir.wav")
	convTestWriteWAV(t, path, ir, 48000, 2, 3, 32)
	if err := RegisterKernel("conv-parity-long", path); err != nil {
		t.Fatal(err)
	}

	cf := convTestNewConvolver(t, "conv-parity-long", nil)
	const inFrames = 4096
	input := sineStereo(inFrames, 48000, 440)
	got := make([]float32, len(input))
	copy(got, input)

	renderEffectChunks(t, cf, got, 2, 512)

	direct := convTestDirectConvolve(input, ir, 2)

	const b = convolverBlockSize
	gotSlice := got[b*2:]
	wantSlice := direct[:(inFrames-b)*2]

	nullDB := parityNullDB(gotSlice, wantSlice)
	t.Logf("long IR (2000 frames) parity null: %.2f dB", nullDB)
	if nullDB > -100.0 {
		t.Fatalf("long IR parity null %.2f dB exceeds tolerance -100 dB", nullDB)
	}
}

func TestConvolverLatency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0}, 48000, 2, 3, 32)
	_ = RegisterKernel("conv-latency-k", path)

	cf := convTestNewConvolver(t, "conv-latency-k", nil)
	want := time.Duration(convolverBlockSize) * time.Second / 48000
	if got := cf.Latency(); got != want {
		t.Fatalf("Latency() = %v, want %v", got, want)
	}
}

func TestConvolverProcessZeroAllocations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0}, 48000, 2, 3, 32)
	_ = RegisterKernel("conv-alloc-k", path)

	cf := convTestNewConvolver(t, "conv-alloc-k", nil)
	buf := sineStereo(960, 48000, 1000)

	allocs := testing.AllocsPerRun(100, func() {
		if err := cf.Process(buf, 480); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("Process allocated %f times per run, want 0", allocs)
	}
}

func TestConvolverBypassIsBitIdentical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.wav")
	convTestWriteWAV(t, path, []float32{0.5, 0.5}, 48000, 2, 3, 32)
	_ = RegisterKernel("conv-bypass-k", path)

	cf := convTestNewConvolver(t, "conv-bypass-k", Values{ParamBypass: true})
	buf := sineStereo(1024, 48000, 1000)
	orig := make([]float32, len(buf))
	copy(orig, buf)

	if err := cf.Process(buf, 512); err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		if math.Float32bits(buf[i]) != math.Float32bits(orig[i]) {
			t.Fatalf("sample %d mutated during bypass: got %v, want %v", i, buf[i], orig[i])
		}
	}
}

func TestConvolverMonoProcessing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0}, 48000, 2, 3, 32)
	_ = RegisterKernel("conv-mono-k", path)

	effect, err := NewConvolverFactory().New(Values{ConvolverKernel: "conv-mono-k"})
	if err != nil {
		t.Fatal(err)
	}
	cf := effect.(*Convolver)
	if _, err := cf.Configure(core.FrameFormat{Rate: 48000, Ch: 1, Fmt: core.F32}); err != nil {
		t.Fatal(err)
	}

	buf := []float32{1.0, 0.5, -0.5, 0.25}
	padded := make([]float32, convolverBlockSize*2)
	copy(padded, buf)

	if err := cf.Process(padded, len(padded)); err != nil {
		t.Fatal(err)
	}
	for i, v := range buf {
		outSample := padded[convolverBlockSize+i]
		if math.Abs(float64(outSample-v)) > 1e-6 {
			t.Fatalf("mono sample %d: got %v, want %v", i, outSample, v)
		}
	}
}

func TestConvolverResetClearsTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0, 0.5, 0.5}, 48000, 2, 3, 32)
	_ = RegisterKernel("conv-reset-k", path)

	cf := convTestNewConvolver(t, "conv-reset-k", nil)
	buf := sineStereo(1024, 48000, 1000)
	if err := cf.Process(buf, 512); err != nil {
		t.Fatal(err)
	}

	if err := cf.Reset(); err != nil {
		t.Fatal(err)
	}
	if cf.inFrames != 0 || cf.outHead != 0 {
		t.Fatalf("Reset left buffer pointers dirty: inFrames=%d, outHead=%d",
			cf.inFrames, cf.outHead)
	}
}

func TestConvolverMeters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0}, 48000, 2, 3, 32)
	_ = RegisterKernel("conv-meters-k", path)

	cf := convTestNewConvolver(t, "conv-meters-k", nil)
	buf := sineStereo(1024, 48000, 1000)
	if err := cf.Process(buf, 512); err != nil {
		t.Fatal(err)
	}

	m := cf.Meters()
	if _, ok := m[MeterIn]; !ok {
		t.Fatalf("missing in meter: %v", m)
	}
	if _, ok := m[MeterOut]; !ok {
		t.Fatalf("missing out meter: %v", m)
	}
}

func TestConvolverParamValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0}, 48000, 2, 3, 32)
	_ = RegisterKernel("conv-valid-k", path)

	cf := convTestNewConvolver(t, "conv-valid-k", nil)

	if err := cf.Set(ConvolverKernel, "conv-valid-k"); err != nil {
		t.Fatalf("Set(kernel-name): %v", err)
	}
	if got, _ := cf.Get(ConvolverKernel); got != "conv-valid-k" {
		t.Fatalf("Get(kernel-name) = %v, want conv-valid-k", got)
	}
	if err := cf.Set("kernel", "conv-valid-k"); err != nil {
		t.Fatalf("Set(kernel alias): %v", err)
	}
	if got, _ := cf.Get("kernel"); got != "conv-valid-k" {
		t.Fatalf("Get(kernel alias) = %v, want conv-valid-k", got)
	}

	if err := cf.Set(ConvolverIRWidth, 150.0); err != nil {
		t.Fatalf("Set(ir-width): %v", err)
	}
	if got, _ := cf.Get(ConvolverIRWidth); got != 150.0 {
		t.Fatalf("Get(ir-width) = %v, want 150.0", got)
	}
	if err := cf.Set(ConvolverAutogain, true); err != nil {
		t.Fatalf("Set(autogain): %v", err)
	}
	if got, _ := cf.Get(ConvolverAutogain); got != true {
		t.Fatalf("Get(autogain) = %v, want true", got)
	}

	if err := cf.Set("unknown-param", 123); err == nil {
		t.Fatal("expected error for unknown parameter")
	}
}

func TestConvolverDefaultIdentityKernel(t *testing.T) {
	f := NewConvolverFactory()
	schema := f.Schema()
	var found bool
	for _, p := range schema {
		if p.Key == ConvolverKernel {
			found = true
			if p.Default != "identity" {
				t.Fatalf("kernel-name default = %v, want identity", p.Default)
			}
		}
	}
	if !found {
		t.Fatalf("schema missing canonical key %s", ConvolverKernel)
	}

	eff, err := f.New(nil)
	if err != nil {
		t.Fatalf("New(nil) failed: %v", err)
	}
	if _, err := eff.Configure(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}
	buf := sineStereo(1024, 48000, 1000)
	if err := eff.Process(buf, 512); err != nil {
		t.Fatalf("Process failed: %v", err)
	}
}

func TestConvolverKernelLongerThanRingDoesNotPanic(t *testing.T) {
	const longFrames = 300 * convolverBlockSize
	ir := make([]float32, longFrames*2)
	ir[0] = 1.0
	ir[1] = 1.0
	ir[2*(longFrames-1)] = 0.5
	ir[2*(longFrames-1)+1] = 0.5

	dir := t.TempDir()
	path := filepath.Join(dir, "giant_ir.wav")
	convTestWriteWAV(t, path, ir, 48000, 2, 3, 32)
	if err := RegisterKernel("conv-giant-ir", path); err != nil {
		t.Fatal(err)
	}

	cf := convTestNewConvolver(t, "conv-giant-ir", nil)
	buf := sineStereo(1024, 48000, 1000)
	if err := cf.Process(buf, 512); err != nil {
		t.Fatalf("Process on oversized kernel failed: %v", err)
	}
}
