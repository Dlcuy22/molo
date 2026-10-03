package analysis

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
)

// streamDecoder is a synthetic decode.Decoder over a fixed sample slice. Tests
// pin the waveform math to values computed by hand rather than to whatever the
// Opus fixtures happen to decode to.
type streamDecoder struct {
	info    core.StreamInfo
	data    []float32
	pos     int
	onRead  func(frames int)
	readErr error
	closed  bool
}

func (d *streamDecoder) Info() core.StreamInfo { return d.info }

func (d *streamDecoder) ReadFrames(dst []float32) (int, error) {
	if d.closed {
		return 0, decode.ErrClosed
	}
	if d.pos >= len(d.data) {
		if d.readErr != nil {
			return 0, d.readErr
		}

		return 0, io.EOF
	}

	ch := d.info.Format.Ch
	if ch < 1 {
		ch = 1
	}
	n := copy(dst, d.data[d.pos:])
	n -= n % ch
	d.pos += n
	frames := n / ch
	if d.onRead != nil {
		d.onRead(frames)
	}

	return frames, nil
}

func (d *streamDecoder) Close() error {
	d.closed = true

	return nil
}

// stubOpen replaces the decoder entry point for one test and returns the open
// count, so a cache test can prove a hit skipped the decode entirely.
func stubOpen(t *testing.T, make func() decode.Decoder) *int {
	t.Helper()

	old := openDecoder
	opens := 0
	openDecoder = func(string) (decode.Decoder, error) {
		opens++

		return make(), nil
	}
	t.Cleanup(func() { openDecoder = old })

	return &opens
}

// tempPath creates a real file so the stat-based cache key is well defined; the
// bytes are never decoded because openDecoder is stubbed.
func tempPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "synthetic.opus")
	if err := os.WriteFile(path, []byte("not real audio"), 0o600); err != nil {
		t.Fatalf("write synthetic file: %v", err)
	}

	return path
}

func approx(t *testing.T, got, want float32, what string) {
	t.Helper()

	if math.Abs(float64(got-want)) > 1e-5 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// monoInfo is a one-channel format so the hand-computed buckets are the raw
// samples, with no downmix in the way.
func monoInfo(frames int64) core.StreamInfo {
	return core.StreamInfo{
		Format:      core.FrameFormat{Rate: 48000, Ch: 1, Fmt: core.F32},
		TotalFrames: frames,
	}
}

func stereoInfo(frames int64) core.StreamInfo {
	return core.StreamInfo{
		Format:      core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		TotalFrames: frames,
	}
}

func TestWaveformMonoMath(t *testing.T) {
	data := []float32{0, 1, 2, 3, 4, 5, 6, 7}
	stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(8), data: data}
	})

	w, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 2})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}
	if w.Frames != 8 {
		t.Fatalf("Frames = %d, want 8", w.Frames)
	}
	if len(w.Buckets) != 2 {
		t.Fatalf("len(Buckets) = %d, want 2", len(w.Buckets))
	}

	// Bucket 0 covers frames 0..3, bucket 1 covers 4..7.
	approx(t, w.Buckets[0].Min, 0, "bucket 0 Min")
	approx(t, w.Buckets[0].Max, 3, "bucket 0 Max")
	approx(t, w.Buckets[0].RMS, float32(math.Sqrt(3.5)), "bucket 0 RMS")

	approx(t, w.Buckets[1].Min, 4, "bucket 1 Min")
	approx(t, w.Buckets[1].Max, 7, "bucket 1 Max")
	approx(t, w.Buckets[1].RMS, float32(math.Sqrt(31.5)), "bucket 1 RMS")
}

func TestWaveformStereoDownmix(t *testing.T) {
	// Frames are (1,-1), (0.5,0.5), (0,0), (2,2), so the mono downmix is
	// 0, 0.5, 0, 2.
	data := []float32{1, -1, 0.5, 0.5, 0, 0, 2, 2}
	stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: stereoInfo(4), data: data}
	})

	w, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 2})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}

	approx(t, w.Buckets[0].Min, 0, "bucket 0 Min")
	approx(t, w.Buckets[0].Max, 0.5, "bucket 0 Max")
	approx(t, w.Buckets[0].RMS, float32(math.Sqrt(0.125)), "bucket 0 RMS")

	approx(t, w.Buckets[1].Min, 0, "bucket 1 Min")
	approx(t, w.Buckets[1].Max, 2, "bucket 1 Max")
	approx(t, w.Buckets[1].RMS, float32(math.Sqrt(2)), "bucket 1 RMS")
}

func TestWaveformUnknownTotalStreamsAndCounts(t *testing.T) {
	data := []float32{0, 1, 2, 3, 4, 5, 6, 7}
	info := monoInfo(-1)
	opens := stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: info, data: data}
	})

	w, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 2})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}
	if w.Frames != 8 {
		t.Fatalf("Frames = %d, want 8", w.Frames)
	}
	// One open to count, one to reduce.
	if *opens != 2 {
		t.Fatalf("opens = %d, want 2 (count then reduce)", *opens)
	}

	approx(t, w.Buckets[0].Min, 0, "bucket 0 Min")
	approx(t, w.Buckets[0].Max, 3, "bucket 0 Max")
	approx(t, w.Buckets[1].Min, 4, "bucket 1 Min")
	approx(t, w.Buckets[1].Max, 7, "bucket 1 Max")
}

func TestWaveformZeroFramesKnownTotal(t *testing.T) {
	stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(0)}
	})

	w, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 3})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}
	if w.Frames != 0 {
		t.Fatalf("Frames = %d, want 0", w.Frames)
	}
	for i, b := range w.Buckets {
		if b != (Bucket{}) {
			t.Fatalf("bucket %d = %+v, want zero", i, b)
		}
	}
}

func TestWaveformZeroFramesUnknownTotal(t *testing.T) {
	opens := stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(-1)}
	})

	w, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 3})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}
	if w.Frames != 0 {
		t.Fatalf("Frames = %d, want 0", w.Frames)
	}
	if *opens != 2 {
		t.Fatalf("opens = %d, want 2", *opens)
	}
}

func TestWaveformFewerFramesThanBuckets(t *testing.T) {
	stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(2), data: []float32{10, 20}}
	})

	w, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 4})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}

	// frame i maps to floor(i*4/2): frame 0 -> bucket 0, frame 1 -> bucket 2.
	approx(t, w.Buckets[0].Min, 10, "bucket 0 Min")
	approx(t, w.Buckets[0].Max, 10, "bucket 0 Max")
	if w.Buckets[1] != (Bucket{}) {
		t.Fatalf("bucket 1 = %+v, want zero", w.Buckets[1])
	}
	approx(t, w.Buckets[2].Min, 20, "bucket 2 Min")
	approx(t, w.Buckets[2].Max, 20, "bucket 2 Max")
	if w.Buckets[3] != (Bucket{}) {
		t.Fatalf("bucket 3 = %+v, want zero", w.Buckets[3])
	}
}

func TestWaveformSingleBucket(t *testing.T) {
	stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(3), data: []float32{1, 2, 3}}
	})

	w, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 1})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}
	if len(w.Buckets) != 1 {
		t.Fatalf("len(Buckets) = %d, want 1", len(w.Buckets))
	}
	approx(t, w.Buckets[0].Min, 1, "Min")
	approx(t, w.Buckets[0].Max, 3, "Max")
	approx(t, w.Buckets[0].RMS, float32(math.Sqrt(14.0/3.0)), "RMS")
}

func TestWaveformRequiresPositiveBuckets(t *testing.T) {
	opens := stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(1), data: []float32{1}}
	})

	if _, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 0}); !errors.Is(err, ErrBuckets) {
		t.Fatalf("Buckets=0 error = %v, want ErrBuckets", err)
	}
	if *opens != 0 {
		t.Fatalf("opens = %d, want 0 (validation happens before decode)", *opens)
	}
}

func TestWaveformCancelledBeforeStart(t *testing.T) {
	opens := stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: monoInfo(1), data: []float32{1}}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Waveform(ctx, tempPath(t), Options{Buckets: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if *opens != 0 {
		t.Fatalf("opens = %d, want 0", *opens)
	}
}

func TestWaveformCancelledMidStreamReleasesDecoder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Two chunks worth of mono frames, so the cancel lands after the first read.
	data := make([]float32, 2*chunkFrames)
	for i := range data {
		data[i] = float32(i%7) - 3
	}
	dec := &streamDecoder{
		info:   monoInfo(int64(len(data))),
		data:   data,
		onRead: func(int) { cancel() },
	}
	old := openDecoder
	openDecoder = func(string) (decode.Decoder, error) { return dec, nil }
	t.Cleanup(func() { openDecoder = old })

	dir := t.TempDir()
	_, err := Waveform(ctx, tempPathIn(t, dir), Options{Buckets: 8, CacheDir: dir})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !dec.closed {
		t.Fatal("decoder was not closed on the cancel path")
	}
	if files := cacheFiles(t, dir); len(files) != 0 {
		t.Fatalf("cancelled run wrote cache files: %v", files)
	}
}

func TestWaveformFailureReleasesDecoder(t *testing.T) {
	boom := errors.New("decode exploded")
	dec := &streamDecoder{info: monoInfo(4), data: []float32{1, 2, 3, 4}, readErr: boom}
	old := openDecoder
	openDecoder = func(string) (decode.Decoder, error) { return dec, nil }
	t.Cleanup(func() { openDecoder = old })

	if _, err := Waveform(context.Background(), tempPath(t), Options{Buckets: 1}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	if !dec.closed {
		t.Fatal("decoder was not closed on the failure path")
	}
}

func TestWaveformProgressIsMonotonic(t *testing.T) {
	data := make([]float32, 3*chunkFrames)
	info := monoInfo(int64(len(data)))
	stubOpen(t, func() decode.Decoder {
		return &streamDecoder{info: info, data: data}
	})

	var calls [][2]int64
	_, err := Waveform(context.Background(), tempPath(t), Options{
		Buckets: 16,
		OnProgress: func(done, total int64) {
			calls = append(calls, [2]int64{done, total})
		},
	})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("OnProgress was never called")
	}
	prev := int64(-1)
	for _, c := range calls {
		if c[0] <= prev {
			t.Fatalf("done went backwards: %v after %d", calls, prev)
		}
		if c[1] != int64(len(data)) {
			t.Fatalf("total = %d, want %d", c[1], len(data))
		}
		prev = c[0]
	}
	if prev != int64(len(data)) {
		t.Fatalf("last done = %d, want %d", prev, len(data))
	}
}

func TestWaveformFixtureReport(t *testing.T) {
	path := filepath.Join("..", "decode", "testdata", "stereo_2s.opus")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	start := time.Now()
	w, err := Waveform(context.Background(), path, Options{Buckets: 480, NoCache: true})
	if err != nil {
		t.Fatalf("Waveform: %v", err)
	}
	elapsed := time.Since(start)

	if !w.Format.Equal(core.CanonicalFormat) {
		t.Fatalf("Format = %+v, want %+v", w.Format, core.CanonicalFormat)
	}
	if w.Frames < 96000 || w.Frames > 96000+5760 {
		t.Fatalf("Frames = %d, want within [96000, %d]", w.Frames, 96000+5760)
	}

	var lo, hi float32 = math.MaxFloat32, -math.MaxFloat32
	var rmsSum float32
	for _, b := range w.Buckets {
		lo = min(lo, b.Min)
		hi = max(hi, b.Max)
		rmsSum += b.RMS
	}
	meanRMS := rmsSum / float32(len(w.Buckets))

	// The fixture is a 440 Hz + 660 Hz sine pair, one tone per channel, so the
	// mono downmix is a bounded tone pair with a healthy, non-clipped peak and
	// a mean RMS that is a substantial fraction of it.
	if lo < -0.9 || lo > -0.3 {
		t.Fatalf("global Min = %v, want in [-0.9,-0.3]", lo)
	}
	if hi < 0.3 || hi > 0.9 {
		t.Fatalf("global Max = %v, want in [0.3,0.9]", hi)
	}
	if meanRMS < 0.1 || meanRMS > 0.6 {
		t.Fatalf("mean bucket RMS = %v, want in [0.1,0.6]", meanRMS)
	}
	// A sine's RMS is peak/sqrt(2). Two independent tones downmixed land lower
	// still, but anything near silence or noise would fall outside this band.
	if ratio := meanRMS / hi; ratio < 0.4 || ratio > 0.85 {
		t.Fatalf("meanRMS/peak = %v, want tone-like [0.4,0.85]", ratio)
	}

	t.Logf("fixture waveform: buckets=%d frames=%d min=%v max=%v meanRMS=%v elapsed=%v",
		len(w.Buckets), w.Frames, lo, hi, meanRMS, elapsed)
}

func TestWaveformFixtureNoGoroutineLeak(t *testing.T) {
	path := filepath.Join("..", "decode", "testdata", "stereo_2s.opus")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	// A completed run must leave no goroutine behind; the reduction is
	// synchronous, so anything extra would be a regression.
	before := runtimeGoroutines()
	for i := 0; i < 3; i++ {
		if _, err := Waveform(context.Background(), path, Options{Buckets: 64, NoCache: true}); err != nil {
			t.Fatalf("Waveform: %v", err)
		}
	}
	after := runtimeGoroutines()
	if after > before+2 {
		t.Fatalf("goroutines grew from %d to %d", before, after)
	}
}
