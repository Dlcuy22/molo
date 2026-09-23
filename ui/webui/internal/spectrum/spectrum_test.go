package spectrum

import (
	"math"
	"testing"
	"time"
)

// testClock is a fake wall clock. The tap advances it by the duration of every
// publish, so publishing audio also moves time forward at the sample rate and
// the jitter buffer's wall-clock pacing is deterministic under test.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func (c *testClock) advance(n int) {
	c.t = c.t.Add(time.Duration(n) * time.Second / 48000)
}

// sliceTap models the engine's tap: a producer cursor the test advances, and a
// reader cursor that drains what has been published. Read returns zero once it
// has caught up, which is exactly what stops the runner's drain loop.
type sliceTap struct {
	samples []float32
	read    int
	write   int
	// chunk bounds one Read, so the multi-read drain path is exercised the way
	// the real tap's bounded reads are.
	chunk int
	// clock, when set, advances with each publish so the runner paces off the
	// same timeline the audio arrives on.
	clock *testClock
}

// publish makes count more samples available, wrapping through the sample
// slice so a tone can keep playing for any number of frames.
func (t *sliceTap) publish(count int) {
	t.write += count
	if t.clock != nil {
		t.clock.advance(count)
	}
}

// arrive makes count more samples available without moving the clock, so a test
// can model a coarse device pull landing while the display pump keeps its own
// cadence.
func (t *sliceTap) arrive(count int) { t.write += count }

// tick moves the clock by samples worth of time without publishing, modelling
// one display frame elapsing between the tap's bursts.
func (t *sliceTap) tick(samples int) {
	if t.clock != nil {
		t.clock.advance(samples)
	}
}

// runner builds a Runner paced by this tap's clock, so a test that publishes
// audio and calls Frame sees the wall clock advance with the audio.
func (t *sliceTap) runner(tb testing.TB, cfg Config) *Runner {
	tb.Helper()
	t.clock = &testClock{t: time.Unix(1, 0)}
	r, err := New(t, cfg)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	r.now = t.clock.now

	return r
}

func (t *sliceTap) Read(dst []float32) int {
	if t.read >= t.write || len(t.samples) == 0 {
		return 0
	}
	limit := min(len(dst), t.write-t.read)
	if t.chunk > 0 {
		limit = min(limit, t.chunk)
	}
	for i := range limit {
		dst[i] = t.samples[(t.read+i)%len(t.samples)]
	}
	t.read += limit

	return limit
}

// silence returns a tap that never publishes, which is the stopped state.
type silence struct{}

func (silence) Read([]float32) int { return 0 }

// feed publishes n more frames and runs one frame per publish, so a test drives
// continuous playback rather than draining a finite file and measuring the
// decay.
func (t *sliceTap) feed(r *Runner, n, perFrame int) {
	for done := 0; done < n; done += perFrame {
		t.publish(perFrame)
		r.Frame()
	}
}

// tone builds a sine at freq over n frames at the default rate. Frequencies
// with an integer cycle count in n (e.g. k*48000/n) wrap cleanly, which a
// stationary-signal test wants; others gain a phase jump at the wrap that a
// rolling window turns into broadband flicker.
func tone(freq float64, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/48000))
	}

	return out
}

// TestNewRejectsBadConfig pins the one invalid input: a low cut above the high
// cut has no sensible interpretation.
func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(silence{}, Config{MinHz: 5000, MaxHz: 100}); err != nil {
		t.Errorf("New accepted an inverted range: %v", err)
	}
	// New itself does not range-check; ConfigureSpectrum does. New only needs a
	// valid FFT, and the default config supplies it.
	if _, err := New(silence{}, Config{Bars: 40, FFT: 2048}); err != nil {
		t.Errorf("New with a partial config: %v", err)
	}
	// A non-power-of-two FFT has no transform.
	if _, err := New(silence{}, Config{FFT: 1000}); err == nil {
		t.Error("New accepted a non-power-of-two FFT")
	}
}

// TestDefaultsFillIn checks a partial config gets the shipped shape rather than
// zeroes that would divide by zero.
func TestDefaultsFillIn(t *testing.T) {
	r, err := New(silence{}, Config{Bars: 40, FFT: 1024, SampleRate: 48000, MinHz: 20, MaxHz: 20000})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := r.Config()
	if cfg.RangeDB <= 0 || cfg.Release <= 0 || cfg.Release >= 1 {
		t.Fatalf("defaults not filled: %+v", cfg)
	}
	if len(r.Bands()) != 40 {
		t.Fatalf("bands = %d, want 40", len(r.Bands()))
	}
}

// TestSilenceStaysFlat is the stopped-state contract: with no audio the display
// must read empty, not hold the last frame or drift.
func TestSilenceStaysFlat(t *testing.T) {
	r, err := New(silence{}, Config{Bars: 64, FFT: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range 30 {
		for b, v := range r.Frame() {
			if v != 0 {
				t.Fatalf("frame %d band %d = %v, want 0 with no audio", i, b, v)
			}
		}
	}
}

// TestToneRaisesSomeBarsAndLeavesOthersEmpty is the load-bearing behaviour: a
// mid-band tone must light the bars around its frequency and leave distant bars
// at zero. A broken bin mapping lights the wrong end of the display.
func TestToneRaisesSomeBarsAndLeavesOthersEmpty(t *testing.T) {
	const freq = 3000
	tap := &sliceTap{samples: tone(freq, 2048), chunk: 1024}
	r := tap.runner(t, Config{Bars: 100, FFT: 2048})

	// Publish enough frames for the rolling window and the peak follower to
	// settle on the tone.
	tap.feed(r, 2048*12, 1024)

	bands := r.Bands()

	// The bars covering 3 kHz are the loud ones; bars far below (bass) must be
	// near zero.
	loud := 0
	for _, v := range bands {
		if v > 0.5 {
			loud++
		}
	}
	if loud == 0 {
		t.Fatalf("no bar rose above 0.5 for a %d Hz tone", freq)
	}
	// The first fifth of a 20 Hz..20 kHz log display is all below ~160 Hz, well
	// under a 3 kHz tone.
	for i, v := range bands[:20] {
		if v > 0.2 {
			t.Fatalf("bass bar %d = %.3f rose for a %d Hz tone", i, v, freq)
		}
	}
}

// noise builds deterministic white noise: an xorshift32 stream scaled to
// amplitude, so smoothness tests are stable run to run.
func noise(amp float64, n int) []float32 {
	out := make([]float32, n)
	x := uint32(0x243F6A88)
	for i := range out {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		out[i] = float32(amp * (float64(x)/float64(math.MaxUint32)*2 - 1))
	}

	return out
}

// TestFramesAreTemporallySmooth pins the EasyEffects-like motion: the display
// shows each fresh analysis directly, with no follower lagging it, so on
// stationary noise the frame-to-frame movement is only the periodogram's own
// flicker between heavily overlapping windows. It must stay small (correlated,
// not jumpy) without being ironed flat: over-smoothing here is what read as a
// slow display trailing the beat.
func TestFramesAreTemporallySmooth(t *testing.T) {
	tap := &sliceTap{samples: noise(0.3, 8192), chunk: 1440}
	r := tap.runner(t, Config{Bars: 150, FFT: 8192})
	tap.feed(r, 8192*4, 1440)

	var total, steps float64
	prev := append([]float64(nil), r.Bands()...)
	for range 120 {
		tap.publish(1440)
		cur := r.Frame()
		for b, v := range cur {
			total += math.Abs(v - prev[b])
		}
		copy(prev, cur)
		steps++
	}
	if mean := total / steps / float64(len(prev)); mean > 0.06 {
		t.Fatalf("mean frame-to-frame bar movement = %.4f, want <= 0.06", mean)
	}
}

// TestBandsAreSmoothAcrossFrequency pins the un-ragged display: on noise the
// neighbour-to-neighbour step must stay small once settled.
func TestBandsAreSmoothAcrossFrequency(t *testing.T) {
	tap := &sliceTap{samples: noise(0.3, 8192), chunk: 1440}
	r := tap.runner(t, Config{Bars: 150, FFT: 8192})
	tap.feed(r, 8192*8, 1440)

	bands := r.Bands()
	var total float64
	for i := 1; i < len(bands); i++ {
		total += math.Abs(bands[i] - bands[i-1])
	}
	if mean := total / float64(len(bands)-1); mean > 0.03 {
		t.Fatalf("mean neighbour bar step = %.4f, want <= 0.03", mean)
	}
}

// TestPeakHoldsStillOnStationaryInput pins the EasyEffects-style axis
// hysteresis: once settled on unchanging audio, the scale must not wander.
//
// The measure is the mean absolute move from the settled peak, not the worst
// single frame. The periodogram itself fluctuates frame to frame on noise (the
// same flicker TestFramesAreTemporallySmooth allows), so one excursion says
// nothing about the follower: it just says which window the run happened to
// land on. A release fast enough to track that flicker shows up as a large mean
// move, which is the drift this pins out.
func TestPeakHoldsStillOnStationaryInput(t *testing.T) {
	tap := &sliceTap{samples: noise(0.3, 8192), chunk: 1440}
	r := tap.runner(t, Config{Bars: 150, FFT: 8192})
	tap.feed(r, 8192*8, 1440)
	settled := r.peak

	const frames = 500
	var move float64
	for range frames {
		tap.publish(1440)
		r.Frame()
		move += math.Abs(r.peak - settled)
	}
	if mean := move / frames; mean > 0.3 {
		t.Fatalf("peak moved %.3f dB/frame on stationary input, want <= 0.3", mean)
	}
}

// TestPeakFollowsPromptlyWithoutYanking pins the scale dynamics both ways: a
// loud onset must move the scale most of the gap within one frame (otherwise
// bars pin at the ceiling and the display reads sluggish), but never the whole
// gap at once (otherwise one drum hit rescales everything by itself and the
// display breathes). The same band holds the downward release: prompt enough
// to re-amplify a quiet passage, slow enough not to pump on brief gaps.
func TestPeakFollowsPromptlyWithoutYanking(t *testing.T) {
	newRunner := func() *Runner {
		r, err := New(silence{}, Config{Bars: 64, FFT: 2048})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		return r
	}
	setBands := func(r *Runner, db float64) {
		for i := range r.bands {
			r.bands[i] = db
		}
	}

	// Upward: one loud frame from a settled quiet scale.
	r := newRunner()
	setBands(r, -20)
	r.peak = -60
	r.trackPeak()
	if frac := (r.peak + 60) / 40; frac < 0.4 || frac > 0.85 {
		t.Fatalf("rise moved %.2f of the gap, want within [0.40, 0.85]", frac)
	}

	// Downward: one quiet frame from a settled loud scale.
	r = newRunner()
	setBands(r, -50)
	r.peak = -30
	r.trackPeak()
	if frac := (r.peak + 30) / -20; frac < 0.05 || frac > 0.3 {
		t.Fatalf("release moved %.2f of the gap, want within [0.05, 0.30]", frac)
	}
}

// TestTonePeaksAtItsFrequency pins the resampling accuracy: the loudest bar
// of a pure tone must sit at the tone's frequency, not pulled toward a
// neighbouring bin the way loudest-bin picking would.
func TestTonePeaksAtItsFrequency(t *testing.T) {
	const freq = 1000
	tap := &sliceTap{samples: tone(freq, 8192), chunk: 4096}
	r := tap.runner(t, Config{Bars: 100, FFT: 8192})
	tap.feed(r, 8192*12, 4096)

	bands := r.Bands()
	best := 0
	for i, v := range bands {
		if v > bands[best] {
			best = i
		}
	}
	if center := r.centers[best]; math.Abs(center-freq)/freq > 0.1 {
		t.Fatalf("loudest bar centers at %.1f Hz for a %d Hz tone", center, freq)
	}
}

// TestBandValueRisesWithLevel checks the dynamic range actually scales. The
// tone is wrap-clean (an integer 43 cycles in the slice) so the rolling
// window sees a stationary signal; otherwise the wrap's phase jump becomes
// broadband flicker that a smoothed follower fairly reads as instability.
func TestBandValueRisesWithLevel(t *testing.T) {
	measure := func(amp float64) float64 {
		samples := tone(1007.8125, 2048)
		for i := range samples {
			samples[i] *= float32(amp)
		}
		tap := &sliceTap{samples: samples, chunk: 1024}
		r := tap.runner(t, Config{Bars: 64, FFT: 2048})
		tap.feed(r, 2048*12, 1024)

		peak := 0.0
		for _, v := range r.Bands() {
			if v > peak {
				peak = v
			}
		}

		return peak
	}

	quiet := measure(0.02)
	loud := measure(0.5)
	// The peak follower normalizes both to the top by design (a quiet passage
	// is amplified), so this only guards against a gross inversion plus float
	// noise, not an exact ordering.
	if quiet > loud+1e-6 {
		t.Fatalf("quiet peak %.3f exceeds loud peak %.3f", quiet, loud)
	}
	if loud < 0.9 {
		t.Fatalf("loud peak = %.3f, want near the top of the display", loud)
	}
}

// TestFrameDecaysAfterAudioStops is the difference between "paused" and "the
// last frame is stuck": once the tap goes quiet the bars must fall, not freeze.
func TestFrameDecaysAfterAudioStops(t *testing.T) {
	tap := &sliceTap{samples: tone(1000, 2048), chunk: 1024}
	r := tap.runner(t, Config{Bars: 64, FFT: 2048})
	tap.feed(r, 2048*12, 1024)

	before := 0.0
	for _, v := range r.Bands() {
		if v > before {
			before = v
		}
	}
	for range 120 {
		r.Frame()
	}
	after := 0.0
	for _, v := range r.Bands() {
		if v > after {
			after = v
		}
	}

	if after >= before {
		t.Fatalf("bars did not decay after audio stopped: %.3f -> %.3f", before, after)
	}
	if after > 0.05 {
		t.Fatalf("bars still at %.3f after 120 silent frames", after)
	}
}

// TestEmptyFramesHoldBeforeDrain pins the jitter guard: a tick or two with no
// new samples is the pump landing between device pulls, not silence, so the
// display must hold bit for bit through the hold window and only drain on a
// sustained drought. Without the hold, every such tick dips the bars 10% and
// the next full tick jumps them back, which reads as tremor.
func TestEmptyFramesHoldBeforeDrain(t *testing.T) {
	tap := &sliceTap{samples: tone(1000, 2048), chunk: 1024}
	r := tap.runner(t, Config{Bars: 64, FFT: 2048})
	tap.feed(r, 2048*12, 1024)

	settled := append([]float64(nil), r.Bands()...)
	peak := 0.0
	for _, v := range settled {
		if v > peak {
			peak = v
		}
	}
	if peak == 0 {
		t.Fatal("settled display reads empty, the hold has nothing to hold")
	}

	for i := range holdEmptyTicks {
		for b, v := range r.Frame() {
			if v != settled[b] {
				t.Fatalf("empty frame %d moved bar %d: %v != %v", i, b, v, settled[b])
			}
		}
	}

	// A sustained drought still drains, and new audio resumes from the hold.
	for range 120 {
		r.Frame()
	}
	after := 0.0
	for _, v := range r.Bands() {
		if v > after {
			after = v
		}
	}
	if after > 0.05 {
		t.Fatalf("bars still at %.3f after a sustained drought", after)
	}
	tap.feed(r, 2048*4, 1024)
	resumed := 0.0
	for _, v := range r.Bands() {
		if v > resumed {
			resumed = v
		}
	}
	if resumed < 0.2 {
		t.Fatalf("bars did not resume after audio returned: %.3f", resumed)
	}
}

// TestPartialFrameIsSilent guards the first moments of a track: before a whole
// FFT frame is buffered the display must read empty, not a scaled-up fragment.
func TestPartialFrameIsSilent(t *testing.T) {
	tap := &sliceTap{samples: tone(1000, 2048), chunk: 64}
	r, err := New(tap, Config{Bars: 32, FFT: 2048})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, v := range r.Frame() {
		if v != 0 {
			t.Fatalf("bar rose to %v on a partial frame", v)
		}
	}
}

// TestJitterBufferSlidesThroughBursts pins the load-bearing jitter-buffer
// behaviour: when the tap delivers audio in coarse bursts (the device pull)
// while the display pump keeps ticking, the window must still advance on every
// pump frame. Before the buffer, each burst jumped the window to the newest
// sample and the pump frames in between found nothing new, so the display
// stepped at the buffer's rate instead of sliding at the pump's.
func TestJitterBufferSlidesThroughBursts(t *testing.T) {
	// A burst every 4800 samples (100 ms) against a 60 Hz pump (800 samples a
	// tick): exactly the measured device cadence.
	const burst, tick = 4800, 800

	tap := &sliceTap{samples: tone(1000, 8192), chunk: 4096}
	r := tap.runner(t, Config{Bars: 64, FFT: 2048})

	// Prime with a couple of bursts, letting the pump run between them.
	for range 2 {
		tap.arrive(burst)
		for range burst / tick {
			tap.tick(tick)
			r.Frame()
		}
	}

	// Count how many pump frames actually move the display.
	moved := 0
	for range 6 {
		tap.arrive(burst)
		for range burst / tick {
			tap.tick(tick)
			before := append([]float64(nil), r.Bands()...)
			r.Frame()
			for b, v := range r.Bands() {
				if v != before[b] {
					moved++
					break
				}
			}
		}
	}
	if want := 6 * burst / tick; moved < want {
		t.Fatalf("display advanced on %d of %d pump frames across bursts, want all", moved, want)
	}
}

// TestJitterBufferHoldsCushion pins that the window never races to the newest
// sample: it keeps a reserve so a late burst cannot starve the next frames. The
// reserve is what turns a stalling catch-up into a smooth hold.
func TestJitterBufferHoldsCushion(t *testing.T) {
	const burst, tick = 4800, 800

	tap := &sliceTap{samples: tone(1000, 8192), chunk: 4096}
	r := tap.runner(t, Config{Bars: 64, FFT: 2048})

	// Feed a long run so the backlog settles, then one burst and let the pump
	// drain it dry. The cushion must survive: the backlog never reaches zero.
	for range 4 {
		tap.arrive(burst)
		for range burst / tick {
			tap.tick(tick)
			r.Frame()
		}
	}
	tap.arrive(burst)
	for range 2 * burst / tick {
		tap.tick(tick)
		r.Frame()
	}
	if got := len(r.backlog); got < r.targetLag() {
		t.Fatalf("backlog fell to %d, below the %d cushion", got, r.targetLag())
	}
}

// TestJitterBufferCapsLag pins the other bound: a consumer that stalls must not
// let the backlog (and with it the display lag) grow without bound. Beyond the
// cap the oldest samples are dropped, the same lose-history trade the tap
// makes.
func TestJitterBufferCapsLag(t *testing.T) {
	tap := &sliceTap{samples: tone(1000, 8192), chunk: 4096}
	r := tap.runner(t, Config{Bars: 64, FFT: 2048})

	// Dump far more audio than the cap allows without advancing the clock, so
	// the backlog would grow past its bound if nothing trimmed it.
	for range 20 {
		tap.arrive(4800)
		r.Frame()
	}
	if maxLen := len(r.window) + r.maxBacklog(); len(r.backlog) > maxLen {
		t.Fatalf("backlog = %d, above cap %d", len(r.backlog), maxLen)
	}
}

// one center per bar, strictly rising, inside [MinHz, MaxHz], and evenly
// spaced in log frequency so equal pitch intervals get equal width.
func TestBarCentersSpanTheRange(t *testing.T) {
	for _, cfg := range []Config{
		{Bars: 150, FFT: 8192, SampleRate: 48000, MinHz: 20, MaxHz: 20000},
		{Bars: 200, FFT: 512, SampleRate: 48000, MinHz: 10, MaxHz: 20000},
		{Bars: 16, FFT: 4096, SampleRate: 44100, MinHz: 30, MaxHz: 16000},
	} {
		centers := barCenters(cfg)
		if len(centers) != cfg.Bars {
			t.Fatalf("centers = %d, want %d", len(centers), cfg.Bars)
		}
		for i, f := range centers {
			if f < cfg.MinHz || f > cfg.MaxHz {
				t.Fatalf("bar %d center = %v, outside [%v, %v]", i, f, cfg.MinHz, cfg.MaxHz)
			}
			if i > 0 && f <= centers[i-1] {
				t.Fatalf("bar %d center %v does not rise above %v", i, f, centers[i-1])
			}
		}
		// Log-even spacing: consecutive ratios agree to a percent.
		ratio := centers[1] / centers[0]
		for i := 2; i < len(centers); i++ {
			if got := centers[i] / centers[i-1]; math.Abs(got-ratio)/ratio > 0.01 {
				t.Fatalf("bar %d spacing ratio = %v, want %v", i, got, ratio)
			}
		}
	}
}

// TestNewCapsBarsToBins checks the guard that keeps a request for more bars
// than the transform can resolve from producing dead columns at the top.
func TestNewCapsBarsToBins(t *testing.T) {
	r, err := New(silence{}, Config{Bars: 4096, FFT: 512})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := len(r.Bands()); got != 256 {
		t.Fatalf("bands = %d, want 256 (512/2 bins)", got)
	}
}

// TestSchemaStaysConsistent pins the numbers the UI shows: the shipped points
// and range, and a low cut that defaults below the high cut.
func TestSchemaStaysConsistent(t *testing.T) {
	params := Schema()
	if len(params) != 3 {
		t.Fatalf("schema has %d params, want 3", len(params))
	}
	byKey := map[string]Param{}
	for _, p := range params {
		byKey[p.Key] = p
		if p.Min > p.Max {
			t.Errorf("%s: min %v above max %v", p.Key, p.Min, p.Max)
		}
		if p.Default < p.Min || p.Default > p.Max {
			t.Errorf("%s: default %v outside [%v, %v]", p.Key, p.Default, p.Min, p.Max)
		}
	}
	if byKey["bars"].Default != 150 {
		t.Errorf("default bars = %v, want 150", byKey["bars"].Default)
	}
	if byKey["minHz"].Default != 20 || byKey["maxHz"].Default != 20000 {
		t.Errorf("default range = %v..%v, want 20..20000", byKey["minHz"].Default, byKey["maxHz"].Default)
	}
}
