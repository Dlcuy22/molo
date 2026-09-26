package dsp

import (
	"math"
	"sync/atomic"
)

// Meter is a level accumulator for an effect's input or output. It is written
// by the audio thread in Push and read by a control goroutine in Read, so every
// field it shares is atomic and Push takes no lock and allocates nothing.
//
// Policy. A reading has two parts, peak and RMS, both linear amplitude where 1
// is full scale:
//
//   - Peak is a block peak-hold with an exponential release. Each Push raises
//     the held peak to that block's maximum if it is higher, otherwise decays
//     it by the per-push release factor. A hold alone would ratchet up and never
//     come down; an instant drop would flicker on every silent block. The
//     release factor is exp(-1/meterReleasePushes), so the peak falls by about
//     20 dB over meterReleasePushes pushes.
//   - RMS is a running mean of squared samples with the same one-pole shape,
//     so it settles instead of jumping between blocks. Read squares it back to
//     amplitude.
//
// Both time constants are in pushes rather than seconds because Push is called
// once per audio block, and the block size is the caller's to choose. The
// defaults suit the post-ring path, where a block is a few milliseconds.
//
// Stereo. For a block of ch channels, Peak is the largest absolute sample
// across all channels, so one loud channel is not hidden by a quiet one. RMS is
// the mean power across every sample in the block, so it is the same regardless
// of how the energy splits between channels.
type Meter struct {
	// The float fields are stored as math.Float32bits in an atomic so both the
	// audio thread and a control reader touch them without a lock. atomic.Float
	// would be clearer but needs Go 1.19; the rest of the package uses the same
	// uint trick for Gain.
	peak atomic.Uint32
	mean atomic.Uint32
}

// Default meter shaping. Release is roughly how many pushes the peak's decay
// spans before it falls about 20 dB. Window is how many pushes the RMS mean
// spans. Both are deliberately short: a UI at ~60 Hz wants a reading that
// responds within a few frames.
const (
	meterReleasePushes = 8.0
	meterWindowPushes  = 16.0
)

// dBFloor is the level a silent reading reports. A meter that returned -Inf
// would render as a broken control and poison any arithmetic a UI does on it,
// so silence clamps here instead.
const dBFloor = -120.0

// NewMeter returns a meter ready to push. It is a convenience only: the zero
// Meter works, since the shaping is constant and the atomics start at zero.
func NewMeter() *Meter { return &Meter{} }

// Push folds one block of frames interleaved samples into the accumulator. It
// only reads buf, takes no lock and allocates nothing, so it is legal on the
// real-time path and cannot alter the samples it measures.
//
// ch may be 1 or 2, matching the engine's frame widths. What a stereo block
// reports is settled in the type comment: Peak is the max across channels, RMS
// the mean power across all samples.
//
// A block with a non-finite sample is treated as silence rather than folded in.
// One NaN or Inf would otherwise poison the stored mean or peak for the rest of
// the session, because the update multiplies the previous value: NaN stays NaN
// and +Inf stays +Inf. Recovering to silence matches the engine's other guards
// and keeps MeterReading finite, which callers rely on.
//
// Push has a single writer: the goroutine that runs Process. The atomics make
// the individual load and store race-free so a control-side Read is safe, but
// they do not make two concurrent Pushers correct: the read-modify-write on
// peak would lose the higher value. Only one goroutine may call Push.
func (m *Meter) Push(buf []float32, frames, ch int) {
	if ch < 1 {
		return
	}
	n := frames * ch
	if n > len(buf) {
		n = len(buf)
	}
	// Drop any trailing partial frame, the same way the effects do, so the
	// meter measures exactly the samples the effect processed.
	n -= n % ch
	if n <= 0 {
		return
	}

	var blockPeak float32
	var blockPower float64
	for i := 0; i < n; i++ {
		s := buf[i]
		if !isFinite32(s) {
			// One bad sample invalidates the whole block's level; treat the
			// block as silent instead of poisoning the accumulator.
			return
		}
		if s < 0 {
			s = -s
		}
		if s > blockPeak {
			blockPeak = s
		}
		blockPower += float64(s) * float64(s)
	}
	blockMean := blockPower / float64(n)

	rel := float32(math.Exp(-1.0 / meterReleasePushes))
	win := float32(1.0 / meterWindowPushes)

	// Peak: hold on a rise, decay otherwise. The store is a plain store because
	// Push is the meter's only writer; a CAS loop would be needed only if a
	// second writer were ever introduced.
	peak := m.Peak()
	if blockPeak > peak {
		peak = blockPeak
	} else {
		peak *= rel
	}
	m.peak.Store(math.Float32bits(peak))

	// RMS: one-pole mean of power, stored as mean power and squared on Read.
	mean := m.meanPower()
	mean = float32(win)*float32(blockMean) + (1-float32(win))*mean
	m.mean.Store(math.Float32bits(mean))
}

// isFinite32 reports whether f is a real number, not NaN or an infinity.
func isFinite32(f float32) bool {
	return !math.IsNaN(float64(f)) && !math.IsInf(float64(f), 0)
}

// Read snapshots the current reading. It is safe from any goroutine and pairs
// with Push's atomics, so a control reader never blocks the audio thread.
func (m *Meter) Read() MeterReading {
	mean := m.meanPower()
	if mean < 0 {
		mean = 0
	}

	return MeterReading{
		Peak: m.Peak(),
		RMS:  float32(math.Sqrt(float64(mean))),
	}
}

// Reset clears the accumulator so a seek or a format change starts from
// silence. It is safe to call while Process is running: a reader may see one
// more stale block, which is harmless for a display.
func (m *Meter) Reset() {
	m.peak.Store(0)
	m.mean.Store(0)
}

// Peak returns the held peak without taking a lock.
func (m *Meter) Peak() float32 {
	return math.Float32frombits(m.peak.Load())
}

// meanPower returns the stored mean power, which is what the atomic holds; the
// reader squares it back to amplitude.
func (m *Meter) meanPower() float32 {
	return math.Float32frombits(m.mean.Load())
}

// MeterReading is one snapshot of a meter, both parts linear amplitude where 1
// is full scale and anything above 1 is clipping.
type MeterReading struct {
	Peak float32
	RMS  float32
}

// DB converts a reading to dBFS. Silence reads the floor rather than -Inf, and
// a non-finite or negative amplitude is treated as silence, so a UI can do
// arithmetic on the result without a special case.
func (r MeterReading) DB() (peakDB, rmsDB float32) {
	return amplitudeDB(r.Peak), amplitudeDB(r.RMS)
}

func amplitudeDB(a float32) float32 {
	if !(a > 0) {
		return dBFloor
	}
	db := float32(20 * math.Log10(float64(a)))
	if db < dBFloor {
		return dBFloor
	}

	return db
}

// meterValues packs the two readings under the standard keys. It allocates a
// map, which is fine because Meters is called from the control side; the audio
// thread never goes through here. The reported value is peak, which is what a
// bar meter shows; RMS is available on the reading itself for a UI that wants
// it. Silence reports the dB floor, so every key is always present and finite.
func meterValues(in, out MeterReading) map[string]float32 {
	inPeak, _ := in.DB()
	outPeak, _ := out.DB()

	return map[string]float32{
		MeterIn:  inPeak,
		MeterOut: outPeak,
	}
}
