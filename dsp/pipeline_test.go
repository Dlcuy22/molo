// Tests that drive the effect framework with a real decoded audio file and
// report what each stage did to the signal. Nothing here opens a device: the
// chain runs over a buffer that was decoded from disk, so the test is
// deterministic and silent.
//
// This is an external test package on purpose. The engine's layering rule is
// that dsp depends only on core, the same way decode does; putting the file
// import here keeps decode out of the dsp library's dependency set.

package dsp_test

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/dsp"
)

// fixturePath resolves a WAV fixture shared with the decode tests. WAV is
// lossless PCM, so the samples the chain sees are the samples the file holds.
func fixturePath(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("..", "decode", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s is missing: %v", name, err)
	}

	return path
}

// readFixture decodes a file to the canonical format the post-ring chain runs
// at. It reads the whole file, which is fine for a quarter-second fixture.
func readFixture(t *testing.T, path string) ([]float32, core.FrameFormat) {
	t.Helper()

	dec, err := decode.Default.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = dec.Close() }()

	info := dec.Info()
	if !info.Format.Equal(core.CanonicalFormat) {
		t.Fatalf("fixture decoded to %+v, want %+v", info.Format, core.CanonicalFormat)
	}

	var out []float32
	buf := make([]float32, 4800*info.Format.Ch)
	for {
		n, err := dec.ReadFrames(buf)
		if n > 0 {
			out = append(out, buf[:n*info.Format.Ch]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("fixture %s decoded to no samples", path)
	}

	return out, info.Format
}

// measure is the report a test logs and asserts on: per-channel level plus the
// magnitude of the two tones the fixture uses.
type measure struct {
	rms       [2]float64
	peak      [2]float64
	leftTone  [2]float64 // 440 Hz in each channel
	rightTone [2]float64 // 660 Hz in each channel
}

func analyse(samples []float32, rate int) measure {
	var m measure
	for c := 0; c < 2; c++ {
		m.rms[c] = rms(samples, 2, c)
		m.peak[c] = peak(samples, 2, c)
		m.leftTone[c] = tone(samples, 2, c, rate, 440)
		m.rightTone[c] = tone(samples, 2, c, rate, 660)
	}

	return m
}

func (m measure) log(t *testing.T, label string) {
	t.Helper()

	for c := 0; c < 2; c++ {
		t.Logf("%-8s ch%d rms=%.6f peak=%.6f | 440 Hz=%.6f 660 Hz=%.6f",
			label, c, m.rms[c], m.peak[c], m.leftTone[c], m.rightTone[c])
	}
}

// rms is the root-mean-square of one interleaved channel.
func rms(samples []float32, ch, channel int) float64 {
	n := len(samples) / ch
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(samples[i*ch+channel])
		sum += v * v
	}

	return math.Sqrt(sum / float64(n))
}

// peak is the largest absolute sample in one interleaved channel.
func peak(samples []float32, ch, channel int) float64 {
	var p float64
	for i := channel; i < len(samples); i += ch {
		if v := math.Abs(float64(samples[i])); v > p {
			p = v
		}
	}

	return p
}

// tone measures one frequency in one channel with a single-bin DFT.
func tone(samples []float32, ch, channel int, rate int, freq float64) float64 {
	n := len(samples) / ch
	if n == 0 {
		return 0
	}
	w := 2 * math.Pi * freq / float64(rate)
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for i := 0; i < n; i++ {
		s0 := float64(samples[i*ch+channel]) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	if power < 0 {
		power = 0
	}

	return 2 * math.Sqrt(power) / float64(n)
}

// processInChunks runs the chain the way the device would: fixed-size buffers,
// within one sample rate, so no stage sees a buffer larger than it would in
// production.
func processInChunks(t *testing.T, chain *dsp.Chain, samples []float32, ch, chunkFrames int) {
	t.Helper()

	for start := 0; start < len(samples); start += chunkFrames * ch {
		end := min(start+chunkFrames*ch, len(samples))
		frames := (end - start) / ch
		if err := chain.Process(samples[start:end], frames); err != nil {
			t.Fatalf("Process at sample %d: %v", start, err)
		}
	}
}

func TestPipelineCrossfeedOnRealAudio(t *testing.T) {
	path := fixturePath(t, "sine_stereo_48k.wav")
	original, format := readFixture(t, path)

	before := analyse(original, format.Rate)
	before.log(t, "before")

	// The Jpop preset's crossfeed block: cutoff 700 Hz, feed 4.5 dB, active.
	pipeline := dsp.Pipeline{
		Post: []dsp.Spec{{
			ID:   "crossfeed-0",
			Kind: "crossfeed",
			Params: dsp.Values{
				dsp.CrossfeedCutoff: 700.0,
				dsp.CrossfeedFeed:   4.5,
			},
		}},
	}

	effects, err := dsp.BuildPost(pipeline)
	if err != nil {
		t.Fatalf("BuildPost: %v", err)
	}
	chain := dsp.NewChain()
	chain.Set(effects)
	if _, err := chain.Configure(format); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	processed := append([]float32(nil), original...)
	processInChunks(t, chain, processed, format.Ch, 480)

	after := analyse(processed, format.Rate)
	after.log(t, "after")

	// The fixture puts 440 Hz on the left and 660 Hz on the right. Crossfeed
	// must move a little of each tone into the other channel, which is the one
	// audible change the stage makes.
	leftLeakIntoRight := after.leftTone[1]
	rightLeakIntoLeft := after.rightTone[0]
	t.Logf("cross-leak: 440 Hz into right = %.6f (was %.6f), 660 Hz into left = %.6f (was %.6f)",
		leftLeakIntoRight, before.leftTone[1], rightLeakIntoLeft, before.rightTone[0])

	if leftLeakIntoRight <= before.leftTone[1] {
		t.Fatalf("440 Hz did not leak into the right channel: before %.6f, after %.6f",
			before.leftTone[1], leftLeakIntoRight)
	}
	if rightLeakIntoLeft <= before.rightTone[0] {
		t.Fatalf("660 Hz did not leak into the left channel: before %.6f, after %.6f",
			before.rightTone[0], rightLeakIntoLeft)
	}

	// Crossfeed is a low-pass mix, so it must not act like a make-up gain
	// stage: the level moves a little and stays bounded.
	for c := 0; c < 2; c++ {
		if after.peak[c] > before.peak[c]*1.5 {
			t.Fatalf("ch%d peak grew from %.6f to %.6f, more than crossfeed should", c, before.peak[c], after.peak[c])
		}
	}

	dumpIfRequested(t, "crossfeed_before.wav", original, format)
	dumpIfRequested(t, "crossfeed_after.wav", processed, format)
}

func TestPipelineBypassedStageIsBitIdentical(t *testing.T) {
	path := fixturePath(t, "sine_stereo_48k.wav")
	original, format := readFixture(t, path)

	values := dsp.Values{
		dsp.CrossfeedCutoff: 700.0,
		dsp.CrossfeedFeed:   4.5,
		dsp.ParamBypass:     true,
	}
	effects, err := dsp.BuildPost(dsp.Pipeline{Post: []dsp.Spec{{Kind: "crossfeed", Params: values}}})
	if err != nil {
		t.Fatalf("BuildPost: %v", err)
	}
	chain := dsp.NewChain()
	chain.Set(effects)
	if _, err := chain.Configure(format); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	processed := append([]float32(nil), original...)
	processInChunks(t, chain, processed, format.Ch, 480)

	for i := range processed {
		if processed[i] != original[i] {
			t.Fatalf("bypassed stage changed sample %d: %v vs %v", i, processed[i], original[i])
		}
	}
}

func TestPipelineSwapsImplementationByWeight(t *testing.T) {
	// One kind, asked for by name and automatically, must resolve to the same
	// registered implementation. This is the decode registry behaviour applied
	// to effects: a preset names the kind, the weight picks the build.
	byKind, err := dsp.Default.New("crossfeed", "", nil)
	if err != nil {
		t.Fatalf("New(auto): %v", err)
	}
	byName, err := dsp.Default.New("crossfeed", "crossfeed-bs2b", nil)
	if err != nil {
		t.Fatalf("New(named): %v", err)
	}
	if byKind.Name() != byName.Name() {
		t.Fatalf("auto chose %q, named %q", byKind.Name(), byName.Name())
	}

	if _, err := dsp.Default.New("crossfeed", "no-such-impl", nil); !errors.Is(err, dsp.ErrUnknownImpl) {
		t.Fatalf("unknown impl = %v, want ErrUnknownImpl", err)
	}
}

// dumpIfRequested writes a WAV next to the test when DSP_DUMP_DIR is set, so a
// run that wants to listen can keep the before and after audio.
func dumpIfRequested(t *testing.T, name string, samples []float32, format core.FrameFormat) {
	t.Helper()

	dir := os.Getenv("DSP_DUMP_DIR")
	if dir == "" {
		return
	}
	writeWAV(t, filepath.Join(dir, name), samples, format)
	t.Logf("wrote %s", filepath.Join(dir, name))
}

// writeWAV writes interleaved float32 samples as a 32-bit float WAV. The
// device would never be handed this; it exists so a run can keep the processed
// audio and listen to what the report describes.
func writeWAV(t *testing.T, path string, samples []float32, format core.FrameFormat) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create dump dir: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer file.Close()

	dataBytes := uint32(len(samples) * 4)
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	putU32(header[4:], uint32(36)+dataBytes)
	copy(header[8:], "WAVE")
	copy(header[12:], "fmt ")
	putU32(header[16:], 16)
	putU16(header[20:], 3) // IEEE float
	putU16(header[22:], uint16(format.Ch))
	putU32(header[24:], uint32(format.Rate))
	putU32(header[28:], uint32(format.Rate*format.Ch*4))
	putU16(header[32:], uint16(format.Ch*4))
	putU16(header[34:], 32)
	copy(header[36:], "data")
	putU32(header[40:], dataBytes)

	if _, err := file.Write(header); err != nil {
		t.Fatalf("write %s header: %v", path, err)
	}
	raw := make([]byte, len(samples)*4)
	for i, s := range samples {
		putU32(raw[i*4:], math.Float32bits(s))
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatalf("write %s body: %v", path, err)
	}
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func putU16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}
