// Package analysis computes information about a file that playback does not
// need, starting with the waveform a seek bar draws.
//
// It never touches the playback ring: the ring is forward-only and feeding the
// device, so reading it would steal audio. Waveform opens its own decoder and
// runs synchronously on the caller's goroutine, which is why every entry point
// takes a context a scrubbing UI can cancel.
package analysis

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
)

// ErrBuckets means Options.Buckets was not positive. It is a caller bug rather
// than a decode failure, so it is returned before any file is touched.
var ErrBuckets = errors.New("analysis: Buckets must be positive")

// chunkFrames bounds one decoder read, about 100 ms at the canonical rate. It
// matches the streamer's own chunk so a waveform pass reads the same units the
// audio path does.
const chunkFrames = 48000 * 100 / 1000

// openDecoder is the decoder entry point. It is a variable so tests can inject
// a synthetic stream and hand-compute the expected buckets.
var openDecoder = decode.Default.Open

// Bucket is one column of a waveform: the extremes and the RMS of the mono
// downmix over a slice of frames.
type Bucket struct {
	Min, Max, RMS float32
}

// Result is a decoded waveform.
//
// It is named Result rather than Waveform because Go gives a package one
// namespace per identifier, so a type and the function that builds it cannot
// share the same name.
type Result struct {
	Buckets []Bucket
	Format  core.FrameFormat
	Frames  int64
}

// Options controls one waveform pass. Buckets is required; the rest have
// sensible zero values.
type Options struct {
	// Buckets is the requested column count. It must be positive.
	Buckets int

	// OnProgress reports how far the decode has come, in frames. It is called
	// once with zero before the first read and after every chunk. It runs on
	// the calling goroutine, so a UI must keep it cheap.
	OnProgress func(done, total int64)

	// CacheDir overrides the on-disk cache directory. Empty selects
	// <user cache dir>/player/waveform. Tests set it so they never touch the
	// real cache.
	CacheDir string

	// NoCache skips both reading and writing the cache.
	NoCache bool
}

// Waveform decodes path and reduces it to a fixed number of buckets.
//
// The total frame count is read from the decoder. A decoder that does not know
// its length (-1) is streamed once to count and once to reduce, so memory stays
// bounded instead of trusting a total that is not there.
func Waveform(ctx context.Context, path string, opts Options) (*Result, error) {
	if opts.Buckets < 1 {
		return nil, ErrBuckets
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key, err := statKey(path)
	if err != nil {
		return nil, err
	}

	c := newCache(opts.CacheDir, opts.NoCache, key)
	if res, ok := c.load(opts.Buckets); ok {
		return res, nil
	}

	res, err := reduce(ctx, path, opts)
	if err != nil {
		// A cancelled or failed run is never cached: the next attempt must do
		// the real work rather than replay a half-finished reduction.
		return nil, err
	}
	c.store(res)

	return res, nil
}

// reduce decodes path and accumulates the buckets. The decoder is released on
// every exit path, including cancellation and decoder failure.
func reduce(ctx context.Context, path string, opts Options) (*Result, error) {
	d, err := openDecoder(path)
	if err != nil {
		return nil, err
	}
	// The close is registered only after a successful open: a failed Open can
	// return a typed-nil decoder, and calling Close on it panics.
	defer func() { _ = d.Close() }()

	info := d.Info()
	ch := info.Format.Ch
	if ch < 1 {
		return nil, errors.New("analysis: decoder reports a non-positive channel count")
	}

	total := info.TotalFrames
	if total < 0 {
		counted, err := countFrames(ctx, d, ch)
		if err != nil {
			return nil, err
		}
		total = counted

		// A forward-only decoder cannot be rewound, so the reduction needs a
		// fresh one. Release the first before replacing it. On a failed reopen
		// d still points at the first decoder, which the deferred close then
		// closes again harmlessly, so it never closes a typed-nil.
		_ = d.Close()
		reopened, err := openDecoder(path)
		if err != nil {
			return nil, err
		}
		d = reopened
	}

	acc := newAccumulator(opts.Buckets)
	buf := make([]float32, chunkFrames*ch)
	var pos int64
	zeroReads := 0

	if opts.OnProgress != nil {
		opts.OnProgress(0, total)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		n, err := d.ReadFrames(buf)
		if n > 0 {
			acc.add(buf[:n*ch], ch, pos, total)
			pos += int64(n)
			zeroReads = 0
		} else if err == nil {
			// A decoder is allowed to return (0, nil) but not forever; a stuck
			// one must fail the pass rather than spin.
			zeroReads++
			if zeroReads > 1000 {
				return nil, errors.New("analysis: decoder made no progress")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if opts.OnProgress != nil {
			opts.OnProgress(pos, total)
		}
	}

	return &Result{Buckets: acc.finish(), Format: info.Format, Frames: pos}, nil
}

// countFrames consumes a forward-only decoder just to learn its length. It
// allocates one bounded buffer and keeps no samples.
func countFrames(ctx context.Context, d decode.Decoder, ch int) (int64, error) {
	buf := make([]float32, chunkFrames*ch)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, err := d.ReadFrames(buf)
		count += int64(n)
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// accumulator folds frames into buckets as they stream past, so a whole file
// never has to be resident.
type accumulator struct {
	buckets []Bucket
	counts  []int64
	sumsq   []float64
}

func newAccumulator(n int) *accumulator {
	return &accumulator{
		buckets: make([]Bucket, n),
		counts:  make([]int64, n),
		sumsq:   make([]float64, n),
	}
}

// add folds dst's frames, in order, starting at frame index pos of total. The
// min and max of an empty bucket stay zero instead of an infinity, so a file
// with more buckets than frames draws flat rather than nonsense.
func (a *accumulator) add(dst []float32, ch int, pos, total int64) {
	frames := len(dst) / ch
	for f := 0; f < frames; f++ {
		var mono float32
		base := f * ch
		for c := 0; c < ch; c++ {
			mono += dst[base+c]
		}
		mono /= float32(ch)

		b := bucketFor(pos+int64(f), total, len(a.buckets))
		if a.counts[b] == 0 {
			a.buckets[b].Min = mono
			a.buckets[b].Max = mono
		} else {
			a.buckets[b].Min = min(a.buckets[b].Min, mono)
			a.buckets[b].Max = max(a.buckets[b].Max, mono)
		}
		a.counts[b]++
		a.sumsq[b] += float64(mono) * float64(mono)
	}
}

func (a *accumulator) finish() []Bucket {
	for i := range a.buckets {
		if a.counts[i] > 0 {
			a.buckets[i].RMS = float32(math.Sqrt(a.sumsq[i] / float64(a.counts[i])))
		}
	}

	return a.buckets
}

// bucketFor maps a frame index to a bucket. A frame at or past a total that
// undercounted the stream lands in the last bucket rather than out of range.
func bucketFor(pos, total int64, n int) int {
	if total <= 0 {
		return 0
	}
	if pos >= total {
		return n - 1
	}

	b := int(pos * int64(n) / total)
	if b >= n {
		b = n - 1
	}

	return b
}
