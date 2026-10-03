package analysis

import (
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"

	"github.com/dlcuy22/molo/core"
)

// cacheVersion is part of the cache key. Bump it whenever the reduction or the
// entry layout changes, so an old entry is ignored instead of read as garbage.
const cacheVersion = 1

// cacheKey identifies a source revision: the absolute path plus the size and
// mtime that a decode result is a function of.
type cacheKey struct {
	Path    string
	Size    int64
	ModTime int64
}

// statKey builds the key for one file. It stats rather than opens, so the key
// is cheap even when the entry is about to hit.
func statKey(path string) (cacheKey, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return cacheKey{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return cacheKey{}, err
	}

	return cacheKey{Path: abs, Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}

// cache is the on-disk waveform store. The zero cache is inert, which is what
// NoCache and an unavailable user cache directory both resolve to.
type cache struct {
	key     cacheKey
	dir     string
	file    string
	enabled bool
}

// newCache resolves the entry path for key. An empty dir selects the per-user
// cache; NoCache disables the cache entirely.
func newCache(dir string, noCache bool, key cacheKey) *cache {
	if noCache {
		return &cache{}
	}
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return &cache{}
		}
		dir = filepath.Join(base, "molo", "waveform")
	}

	return &cache{
		key:     key,
		dir:     dir,
		file:    filepath.Join(dir, entryName(key)),
		enabled: true,
	}
}

// entryName is a stable filename for a key. The raw path is hashed so an entry
// is one flat file rather than a mirror of the user's directory tree, and the
// size and mtime are folded in so a changed file maps to a different name.
func entryName(key cacheKey) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key.Path))

	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(key.Size))
	_, _ = h.Write(size[:])

	var mtime [8]byte
	binary.LittleEndian.PutUint64(mtime[:], uint64(key.ModTime))
	_, _ = h.Write(mtime[:])

	return hexName(h.Sum64())
}

func hexName(v uint64) string {
	const digits = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}

	return string(b[:])
}

// entry is the serialized form. It stores the key so a hash collision re-decodes
// instead of returning another file's waveform.
type entry struct {
	Version int        `json:"version"`
	Key     cacheKey   `json:"key"`
	Buckets int        `json:"buckets"`
	Format  frameJSON  `json:"format"`
	Frames  int64      `json:"frames"`
	Data    []bucketDB `json:"data"`
}

type frameJSON struct {
	Rate int `json:"rate"`
	Ch   int `json:"ch"`
	Fmt  int `json:"fmt"`
}

type bucketDB struct {
	Min float32 `json:"min"`
	Max float32 `json:"max"`
	RMS float32 `json:"rms"`
}

// load returns a cached result for the requested bucket count. A missing,
// unreadable, corrupt or mismatched entry is a miss, never an error: the disk
// cache is an optimisation and must not be able to break a waveform.
func (c *cache) load(buckets int) (*Result, bool) {
	if !c.enabled {
		return nil, false
	}
	raw, err := os.ReadFile(c.file)
	if err != nil {
		return nil, false
	}

	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false
	}
	if e.Version != cacheVersion || e.Key != c.key || e.Buckets != buckets || len(e.Data) != buckets {
		return nil, false
	}
	// NaN would survive a round trip as null and silently become zero, so a
	// non-finite value marks the entry untrustworthy rather than wrong.
	for _, b := range e.Data {
		if !finite(b.Min) || !finite(b.Max) || !finite(b.RMS) {
			return nil, false
		}
	}

	res := &Result{
		Buckets: make([]Bucket, buckets),
		Format:  core.FrameFormat{Rate: e.Format.Rate, Ch: e.Format.Ch, Fmt: core.SampleFormat(e.Format.Fmt)},
		Frames:  e.Frames,
	}
	for i, b := range e.Data {
		res.Buckets[i] = Bucket{Min: b.Min, Max: b.Max, RMS: b.RMS}
	}

	return res, true
}

func finite(f float32) bool {
	return !math.IsNaN(float64(f)) && !math.IsInf(float64(f), 0)
}

// store writes the entry. Write failures are ignored: a full or read-only cache
// directory must never fail a waveform the caller already has.
func (c *cache) store(res *Result) {
	if !c.enabled {
		return
	}

	e := entry{
		Version: cacheVersion,
		Key:     c.key,
		Buckets: len(res.Buckets),
		Format: frameJSON{
			Rate: res.Format.Rate,
			Ch:   res.Format.Ch,
			Fmt:  int(res.Format.Fmt),
		},
		Frames: res.Frames,
		Data:   make([]bucketDB, len(res.Buckets)),
	}
	for i, b := range res.Buckets {
		e.Data[i] = bucketDB{Min: b.Min, Max: b.Max, RMS: b.RMS}
	}

	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return
	}

	// A temp file plus rename keeps a reader from seeing a half-written entry,
	// which is the only way a concurrent reader could load a torn waveform.
	tmp, err := os.CreateTemp(c.dir, ".waveform-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)

		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)

		return
	}
	if err := os.Rename(name, c.file); err != nil {
		os.Remove(name)
	}
}
