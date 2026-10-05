package dsp

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func convTestWriteWAV(t *testing.T, path string, samples []float32, rate, ch, formatCode, bits int) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create test dir: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create test wav: %v", err)
	}
	defer file.Close()

	bytesPerSample := bits / 8
	dataBytes := uint32(len(samples) * bytesPerSample)
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], 36+dataBytes)
	copy(header[8:], "WAVE")
	copy(header[12:], "fmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], uint16(formatCode))
	binary.LittleEndian.PutUint16(header[22:], uint16(ch))
	binary.LittleEndian.PutUint32(header[24:], uint32(rate))
	binary.LittleEndian.PutUint32(header[28:], uint32(rate*ch*bytesPerSample))
	binary.LittleEndian.PutUint16(header[32:], uint16(ch*bytesPerSample))
	binary.LittleEndian.PutUint16(header[34:], uint16(bits))
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], dataBytes)

	if _, err := file.Write(header); err != nil {
		t.Fatalf("write wav header: %v", err)
	}

	body := make([]byte, dataBytes)
	switch {
	case formatCode == 1 && bits == 16:
		for i, s := range samples {
			clamped := math.Max(-1.0, math.Min(1.0, float64(s)))
			val := int16(math.Round(clamped * 32767.0))
			binary.LittleEndian.PutUint16(body[i*2:], uint16(val))
		}
	case formatCode == 3 && bits == 32:
		for i, s := range samples {
			binary.LittleEndian.PutUint32(body[i*4:], math.Float32bits(s))
		}
	case formatCode == 3 && bits == 64:
		for i, s := range samples {
			binary.LittleEndian.PutUint64(body[i*8:], math.Float64bits(float64(s)))
		}
	default:
		t.Fatalf("unsupported test wav format: code %d, bits %d", formatCode, bits)
	}

	if _, err := file.Write(body); err != nil {
		t.Fatalf("write wav body: %v", err)
	}
}

func TestIRLoaderFloat32Stereo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stereo32.wav")
	src := []float32{0.25, -0.5, 0.75, -0.125}
	convTestWriteWAV(t, path, src, 48000, 2, 3, 32)

	if err := RegisterKernel("test-f32-stereo", path); err != nil {
		t.Fatalf("RegisterKernel: %v", err)
	}

	ir, err := getKernel("test-f32-stereo")
	if err != nil {
		t.Fatalf("getKernel: %v", err)
	}
	if ir.sampleRate != 48000 || ir.frames != 2 {
		t.Fatalf("unexpected ir metadata: rate=%d frames=%d", ir.sampleRate, ir.frames)
	}
	for i, want := range src {
		if math.Abs(float64(ir.samples[i]-want)) > 1e-6 {
			t.Fatalf("sample %d: got %v, want %v", i, ir.samples[i], want)
		}
	}
}

func TestIRLoaderPCM16MonoNormalizesToStereo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mono16.wav")
	src := []float32{0.5, -0.25, 0.75}
	convTestWriteWAV(t, path, src, 44100, 1, 1, 16)

	if err := RegisterKernel("test-pcm16-mono", path); err != nil {
		t.Fatalf("RegisterKernel: %v", err)
	}

	ir, err := getKernel("test-pcm16-mono")
	if err != nil {
		t.Fatalf("getKernel: %v", err)
	}
	if ir.sampleRate != 44100 || ir.frames != 3 {
		t.Fatalf("unexpected metadata: rate=%d frames=%d", ir.sampleRate, ir.frames)
	}
	if len(ir.samples) != 6 {
		t.Fatalf("expected 6 interleaved samples, got %d", len(ir.samples))
	}

	for i, want := range src {
		gotL := ir.samples[i*2]
		gotR := ir.samples[i*2+1]
		if math.Abs(float64(gotL-want)) > 1.0/32768.0 || math.Abs(float64(gotR-want)) > 1.0/32768.0 {
			t.Fatalf("frame %d: got (%v, %v), want %v", i, gotL, gotR, want)
		}
	}
}

func TestIRLoaderFloat64Stereo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stereo64.wav")
	src := []float32{0.1, 0.2, 0.3, 0.4}
	convTestWriteWAV(t, path, src, 96000, 2, 3, 64)

	if err := RegisterKernel("test-f64-stereo", path); err != nil {
		t.Fatalf("RegisterKernel: %v", err)
	}

	ir, err := getKernel("test-f64-stereo")
	if err != nil {
		t.Fatalf("getKernel: %v", err)
	}
	if ir.sampleRate != 96000 || ir.frames != 2 {
		t.Fatalf("unexpected metadata: rate=%d frames=%d", ir.sampleRate, ir.frames)
	}
	for i, want := range src {
		if math.Abs(float64(ir.samples[i]-want)) > 1e-6 {
			t.Fatalf("sample %d: got %v, want %v", i, ir.samples[i], want)
		}
	}
}

func TestIRLoaderErrors(t *testing.T) {
	if err := RegisterKernel("", "any.wav"); err == nil {
		t.Fatal("expected error for empty kernel name")
	}
	if err := RegisterKernel("missing", "non-existent-path.wav"); err == nil {
		t.Fatal("expected error for missing file")
	}

	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.wav")
	if err := os.WriteFile(badPath, []byte("NOT A RIFF WAV"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKernel("bad", badPath); err == nil {
		t.Fatal("expected error for corrupt wav")
	}
}

func TestKernelsListSorted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.wav")
	convTestWriteWAV(t, path, []float32{1.0, 1.0}, 48000, 2, 3, 32)

	_ = RegisterKernel("z-kernel", path)
	_ = RegisterKernel("a-kernel", path)
	_ = RegisterKernel("m-kernel", path)

	names := Kernels()
	if !slices.Contains(names, "a-kernel") || !slices.Contains(names, "z-kernel") {
		t.Fatalf("expected registered kernels in list: %v", names)
	}
	if !slices.IsSorted(names) {
		t.Fatalf("kernels list is not sorted: %v", names)
	}
}

func TestResampleLinear(t *testing.T) {
	dc := []float32{1.0, 1.0, 1.0, 1.0, 1.0, 1.0}
	out, dstFrames := resampleLinear(dc, 3, 44100, 48000)
	if dstFrames < 3 {
		t.Fatalf("expected >= 3 frames, got %d", dstFrames)
	}
	for i, v := range out {
		if math.Abs(float64(v-1.0)) > 1e-6 {
			t.Fatalf("dc resample index %d: got %v, want 1.0", i, v)
		}
	}

	same, frames := resampleLinear(dc, 3, 48000, 48000)
	if frames != 3 || len(same) != len(dc) {
		t.Fatalf("same rate changed frame count")
	}
}

func TestApplyIRWidth(t *testing.T) {
	samples := []float32{1.0, 0.0}
	applyIRWidth(samples, 100.0)
	if samples[0] != 1.0 || samples[1] != 0.0 {
		t.Fatalf("width 100 altered stereo signal: %v", samples)
	}

	samples = []float32{1.0, 0.0}
	applyIRWidth(samples, 0.0)
	if math.Abs(float64(samples[0]-0.5)) > 1e-6 || math.Abs(float64(samples[1]-0.5)) > 1e-6 {
		t.Fatalf("width 0 failed mono collapse: %v", samples)
	}

	samples = []float32{1.0, 0.0}
	applyIRWidth(samples, 200.0)
	if math.Abs(float64(samples[0]-1.5)) > 1e-6 || math.Abs(float64(samples[1]-(-0.5))) > 1e-6 {
		t.Fatalf("width 200 failed widening: %v", samples)
	}
}

func TestApplyIRAutogain(t *testing.T) {
	loud := []float32{2.0, 2.0}
	applyIRAutogain(loud)
	want := float32(2.0 / math.Sqrt(4.0))
	if math.Abs(float64(loud[0]-want)) > 1e-5 {
		t.Fatalf("autogain loud: got %v, want %v", loud[0], want)
	}

	quiet := []float32{0.1, 0.1}
	applyIRAutogain(quiet)
	if quiet[0] != 0.1 || quiet[1] != 0.1 {
		t.Fatalf("autogain quiet should be capped at 1.0, got %v", quiet)
	}
}
