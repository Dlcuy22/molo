// Package spectrum turns the player's live audio tap into the band magnitudes a
// bar visualizer draws.
//
// It is display-only: it reads a copy of the post-gain samples the engine
// already publishes and never touches playback. The work runs on a caller-owned
// goroutine at the visualizer's own rate, so a slow frame costs the display a
// frame, not the audio path anything.
package spectrum

import (
	"math"
)

// Tap is the read side of the engine's visualizer feed. It is declared here as
// the narrowest useful interface so this package does not depend on the player
// facade and a test can drive it with a slice.
type Tap interface {
	// Read copies up to len(dst) mono frames and reports how many. It never
	// blocks and returns zero when nothing is buffered.
	Read(dst []float32) int
}

// Config is the visualizer's transform shape. Every field has a working default
// in DefaultConfig; the UI exposes the bar count and the frequency range.
type Config struct {
	// Bars is how many bands the spectrum is reduced to. It is the number of
	// bars the display draws, so it is independent of the FFT size.
	Bars int

	// FFT is the transform length. It must be a power of two. 8192 gives a
	// 5.9 Hz bin at 48 kHz, which resolves bass notes the way EasyEffects'
	// 8192-point spectrum does; a shorter frame smears the low end across
	// neighbouring bars.
	FFT int

	// SampleRate is the rate of the tapped audio. The engine's canonical format
	// is 48 kHz.
	SampleRate int

	// MinHz and MaxHz bound the analysed range. Below 20 Hz is inaudible rumble
	// and above 20 kHz is above human hearing, so the default range maps the
	// audible band across the full display instead of wasting bars on silence.
	MinHz float64
	MaxHz float64

	// RangeDB is how far below the running peak a band falls to zero. It sets
	// the contrast: a small range makes quiet bands visible, a large one makes
	// the display peakier.
	RangeDB float64

	// Release is the per-frame decay of the peak follower in [0, 1). It is what
	// makes the scale dynamic: the display re-ranges to the loudest recent
	// content instead of sitting at a fixed, mostly-empty level. It decays
	// slowly so a transient does not pump the whole display's scale, which is
	// also what keeps the peak from wandering on stationary input (pinned by
	// TestPeakHoldsStillOnStationaryInput): a faster release re-amplifies
	// quiet passages sooner but lets noise walk the scale.
	Release float64
}

// DefaultConfig is the visualization the product ships with: 150 bars over the
// full audible range at the engine's canonical rate.
func DefaultConfig() Config {
	return Config{
		Bars:       150,
		FFT:        8192,
		SampleRate: 48000,
		MinHz:      20,
		MaxHz:      20000,
		RangeDB:    60,
		Release:    0.92,
	}
}

// Param is one visualizer control, described for a generic UI renderer. The set
// is small and fixed, so the frontend can build the panel without a schema
// library.
type Param struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Kind    string  `json:"kind"` // "int" | "float"
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Step    float64 `json:"step"`
	Default float64 `json:"default"`
}

// Schema describes the visualizer controls. The frequency range is expressed in
// whole Hz, so the UI shows a plain number rather than a fraction.
func Schema() []Param {
	return []Param{
		{Key: "bars", Label: "Points", Kind: "int", Min: 16, Max: 256, Step: 1, Default: 150},
		{Key: "minHz", Label: "Low cut", Kind: "int", Min: 10, Max: 500, Step: 1, Default: 20},
		{Key: "maxHz", Label: "High cut", Kind: "int", Min: 2000, Max: 20000, Step: 100, Default: 20000},
	}
}

// Runner computes one spectrum frame per call. It owns its FFT scratch and its
// rolling sample window, so a steady display allocates nothing after New.
type Runner struct {
	cfg Config
	tap Tap
	an  *Analyzer

	// window is the last FFT samples in time order, filled once the tap has
	// published a whole frame's worth.
	window []float32
	filled int

	// tmp is the drain buffer. The tap overwrites oldest when a consumer falls
	// behind, so a frame drains everything buffered and keeps the newest FFT
	// samples rather than reading one stale block.
	tmp []float32

	// mags is the raw per-bin spectrum; bands is the per-bar magnitude
	// resampled from it, converted to dB by toDB; out is the smoothed display
	// value. Keeping them separate is what lets the display decay when audio
	// stops instead of freezing on the last frame.
	mags  []float64
	bands []float64
	out   []float64
	// smooth is scratch for the neighbour-averaging pass, kept so a steady
	// display allocates nothing after New.
	smooth []float64
	// centers holds each bar's center frequency, log-spaced across the
	// configured range the way EasyEffects resamples its FFT onto log-spaced
	// display points.
	centers []float64

	// peak is the smoothed recent maximum in dB, which the dynamic scale is
	// built on.
	peak float64

	// emptyTicks counts consecutive Frame calls that found no new samples on
	// the tap. A few empty frames are normal scheduling jitter between the
	// device pulls and this ticker, so the display holds through them; only a
	// sustained drought means playback really stopped and the bars drain.
	emptyTicks int
}

// New validates cfg, fills any unset field from DefaultConfig, and returns a
// Runner over tap.
func New(tap Tap, cfg Config) (*Runner, error) {
	cfg = withDefaults(cfg)

	an, err := NewAnalyzer(cfg.FFT)
	if err != nil {
		return nil, err
	}
	// More bars than bins oversamples the curve without adding detail, so cap
	// the display at what the transform resolves.
	if bins := an.Bins(); cfg.Bars > bins {
		cfg.Bars = bins
	}

	r := &Runner{
		cfg:     cfg,
		tap:     tap,
		an:      an,
		window:  make([]float32, cfg.FFT),
		tmp:     make([]float32, cfg.FFT*2),
		mags:    make([]float64, an.Bins()),
		bands:   make([]float64, cfg.Bars),
		out:     make([]float64, cfg.Bars),
		smooth:  make([]float64, cfg.Bars),
		centers: barCenters(cfg),
		peak:    math.Inf(-1),
	}

	return r, nil
}

// withDefaults fills a zero Config field from the shipped default, so a caller
// that only cares about one knob does not have to spell out the rest.
func withDefaults(cfg Config) Config {
	def := DefaultConfig()
	if cfg.Bars <= 0 {
		cfg.Bars = def.Bars
	}
	if cfg.FFT < 2 {
		cfg.FFT = def.FFT
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.MinHz <= 0 {
		cfg.MinHz = def.MinHz
	}
	if cfg.MaxHz <= 0 {
		cfg.MaxHz = def.MaxHz
	}
	if cfg.RangeDB <= 0 {
		cfg.RangeDB = def.RangeDB
	}
	if cfg.Release <= 0 || cfg.Release >= 1 {
		cfg.Release = def.Release
	}

	return cfg
}

// holdEmptyTicks is how many consecutive empty frames the display holds
// before it starts draining. At the ~60 Hz pump this is about half a second:
// far above any scheduling gap between the device pulls and the ticker, so
// live playback never trips it, but short enough that a real stop still reads
// as immediate.
const holdEmptyTicks = 30

// drainFactor is the per-tick multiplier toward zero once a sustained drought
// has proven playback really stopped. It clears the display in a handful of
// frames instead of snapping it off.
const drainFactor = 0.5

// peakRise is how far the scale follower moves toward a new maximum in one
// frame. It answers the two failure modes at once: near 1.0 a single transient
// rescales the whole display by itself (breathing), near 0.0 the scale trails
// loud onsets and bars pin at the ceiling (sluggish). Two thirds lands a loud
// onset within a couple of frames at the 60 Hz pump.
const peakRise = 0.65

// Bands returns the current smoothed frame, one value per bar in [0, 1]. It is
// the same slice Frame writes, so a caller that keeps it must copy.
func (r *Runner) Bands() []float64 { return r.out }

// Config returns the effective config, defaults resolved.
func (r *Runner) Config() Config { return r.cfg }

// Frame drains the tap, transforms the newest window, and updates the bands. It
// returns the same slice as Bands. It is safe to call from one goroutine; the
// runner is not internally synchronised because its owner is the ticker.
func (r *Runner) Frame() []float64 {
	got := r.drain()
	if got > 0 {
		r.emptyTicks = 0
		r.an.Magnitudes(r.window[:r.filled], r.mags)
		r.sampleBands()
		r.toDB()
		r.trackPeak()

		// The display shows the fresh analysis directly, the way EasyEffects
		// replaces its series every frame with no follower in between:
		// consecutive windows overlap ~90% at the 60 Hz pump, so the motion
		// is already continuous. A per-bar attack/release here only adds
		// lag, which reads as a slow display trailing the beat.
		for b := range r.out {
			r.out[b] = r.bandValue(b)
		}

		return r.out
	}

	// No new samples is the ticker landing between device pulls, not
	// silence: hold the display bit for bit so the bars never dip on
	// scheduling jitter. Only a sustained drought drains toward zero, which
	// is what clears the bars when playback stops rather than leaving the
	// last frame frozen on screen.
	r.emptyTicks++
	if r.emptyTicks <= holdEmptyTicks {
		return r.out
	}
	for b := range r.out {
		r.out[b] += (0 - r.out[b]) * drainFactor
	}

	return r.out
}

// bandValue maps one band's dB to a display level in [0, 1] against the dynamic
// range. A silent band sits at zero, and a partial first frame sits low rather
// than reading as a level the audio never had.
func (r *Runner) bandValue(b int) float64 {
	if r.filled < r.cfg.FFT {
		return 0
	}
	top := r.peak
	if math.IsInf(top, -1) {
		return 0
	}
	v := (r.bands[b] - (top - r.cfg.RangeDB)) / r.cfg.RangeDB
	if v < 0 || math.IsNaN(v) {
		return 0
	}
	if v > 1 {
		return 1
	}

	return v
}

// drain reads everything buffered and keeps the newest FFT samples. The tap
// overwrites the oldest frames when the consumer is slow, so reading one block
// per frame would skip audio and make the display jitter; draining makes a slow
// frame lose history instead. It returns how many frames this call read, so a
// caller can tell silence from a frame that simply had no new audio.
func (r *Runner) drain() int {
	got := 0
	for {
		n := r.tap.Read(r.tmp)
		if n <= 0 {
			break
		}
		r.advance(r.tmp[:n])
		got += n
	}

	return got
}

// advance folds n new samples into the rolling window, discarding the oldest.
func (r *Runner) advance(p []float32) {
	if len(p) >= len(r.window) {
		copy(r.window, p[len(p)-len(r.window):])
		r.filled = len(r.window)

		return
	}

	keep := r.filled + len(p)
	if keep > len(r.window) {
		drop := keep - len(r.window)
		copy(r.window, r.window[drop:r.filled])
		r.filled -= drop
	}
	copy(r.window[r.filled:], p)
	r.filled += len(p)
}

// toDB converts band magnitudes to dB relative to full scale. A silent band is
// pinned to a floor far below the dynamic range so it reads as empty rather
// than as a number with a sign.
func (r *Runner) toDB() {
	const floorDB = -160

	for b := range r.bands {
		if r.bands[b] <= 1e-9 {
			r.bands[b] = floorDB

			continue
		}
		r.bands[b] = 20 * math.Log10(r.bands[b])
	}
}

// sampleBands resamples the bin magnitude curve at each bar's center
// frequency with linear interpolation, the way EasyEffects resamples its FFT
// onto log-spaced display points (it uses a Steffen spline; on 4097 bins a
// line between neighbours is within a fraction of a dB and, like the spline,
// never overshoots). Taking the loudest bin instead would pull every wide bar
// toward its noisiest bin and read hot; averaging would bury a tonal peak
// among quiet neighbours. Interpolation reads the curve where the bar
// actually is.
func (r *Runner) sampleBands() {
	hzPerBin := float64(r.cfg.SampleRate) / float64(r.cfg.FFT)
	top := len(r.mags) - 1

	for b, f := range r.centers {
		pos := f / hzPerBin
		if pos < 0 {
			pos = 0
		}
		if pos > float64(top) {
			pos = float64(top)
		}
		lo := int(pos)
		hi := min(lo+1, top)
		frac := pos - float64(lo)
		r.bands[b] = r.mags[lo] + (r.mags[hi]-r.mags[lo])*frac
	}

	// One pass of neighbour averaging calms the bin-to-bin periodogram
	// variance that reads as ragged spikes between adjacent bars. It runs on
	// linear magnitudes, before the dB conversion: averaging decibels lets a
	// floored silent neighbour drag a real peak down tens of dB, while
	// averaging magnitudes only softens it by a few.
	copy(r.smooth, r.bands)
	last := len(r.bands) - 1
	for b := range r.bands {
		lo := r.smooth[max(b-1, 0)]
		hi := r.smooth[min(b+1, last)]
		r.bands[b] = 0.25*lo + 0.5*r.smooth[b] + 0.25*hi
	}
}

// trackPeak follows the recent loudest band with a deadband: inside ~1% of
// the display range the scale holds still, so periodogram noise never
// shimmers it the way EasyEffects' axis hysteresis avoids relayouts. Outside
// the band it eases up toward a new maximum and decays slowly, so a quiet
// passage is amplified and a loud one is not pinned at the ceiling for long.
// The floor it decays toward keeps the next quiet track from being scaled
// against a peak from the previous, louder one.
func (r *Runner) trackPeak() {
	peak := math.Inf(-1)
	for _, db := range r.bands {
		if db > peak {
			peak = db
		}
	}
	if peak < -60 {
		peak = r.floor()
	}
	if math.IsInf(r.peak, -1) {
		r.peak = peak

		return
	}
	// EasyEffects only moves its dynamic axis when the data moves more than
	// ~1%, so the scale never shimmers with periodogram noise. Same deadband
	// here, measured against the display range: inside it the scale holds
	// perfectly still.
	if math.Abs(peak-r.peak)/r.cfg.RangeDB <= 0.01 {
		return
	}
	if peak > r.peak {
		// A transient would yank the whole display's scale if the peak
		// jumped to it at once; easing up most of the way in a couple of
		// frames keeps the response immediate without letting one drum hit
		// rescale everything by itself.
		r.peak += (peak - r.peak) * peakRise
	} else {
		r.peak = r.peak*r.cfg.Release + peak*(1-r.cfg.Release)
	}
}

// floor is the dB level the follower decays toward when nothing is playing.
func (r *Runner) floor() float64 { return -90 }

// barCenters returns each bar's center frequency, log-spaced across the
// configured range so equal pitch intervals get equal width, the way the ear
// divides sound. Sampling the curve at centers rather than assigning bin
// ranges keeps every bar on the curve even where the log scale outruns the
// bin resolution, so there are no dead columns at the top and no leaked
// energy past the high cut.
func barCenters(cfg Config) []float64 {
	centers := make([]float64, cfg.Bars)
	ratio := cfg.MaxHz / cfg.MinHz

	for i := range centers {
		centers[i] = cfg.MinHz * math.Pow(ratio, (float64(i)+0.5)/float64(cfg.Bars))
	}

	return centers
}
