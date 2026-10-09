// HTTP range-backed reader for the Network provider.
//
// A remote audio file is read through ranged GETs rather than downloaded whole.
// The container readers in the decode package seek across the source to build
// their index and to seek, so a network source has to answer ReadAt and Seek.
// Ranged reads are the honest way to serve that without holding a whole track in
// memory.
//
// Reads are served from a bounded cache of fixed-size blocks. The container
// readers seek constantly while indexing (an Ogg page index walks every page
// header; a WebM index walks the clusters), so without a cache each page header
// would be its own request. With it, an index walk costs one request per block
// the file spans, not one per page. The cache is bounded so a long file's index
// walk does not quietly accumulate into a whole-file buffer.
//
// A server that ignores Range and replies 200 is handled by buffering the whole
// body, bounded by maxFallbackBody, so the source stays seekable even though the
// server does not cooperate.

package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	// httpBlockSize is the read granularity of a ranged source: one Range
	// request and one cached block.
	httpBlockSize = 1 << 20 // 1 MiB

	// httpMaxBlocks bounds the block cache. It caps the reader's memory at
	// roughly this many megabytes regardless of file size; an index walk over a
	// longer file re-fetches evicted blocks rather than growing without bound.
	httpMaxBlocks = 16

	// maxFallbackBody bounds the whole-file fallback used when a server ignores
	// Range. Such a source cannot be ranged, so it is buffered once; the bound
	// stops a lying or endless body from exhausting memory.
	maxFallbackBody = 512 << 20
)

var (
	errNegativeOffset = errors.New("provider: negative offset")
	errSeekOutOfRange = errors.New("provider: seek out of range")
)

// httpRangeReader is an io.ReaderAt and io.ReadSeeker over an HTTP resource. It
// is safe for the sequential use a single decoder makes of it; the mutex guards
// the block cache and the read cursor against the seek-then-read pattern the
// container readers use, not concurrent readers. Each decoder gets its own
// instance.
type httpRangeReader struct {
	ctx    context.Context
	client *http.Client
	url    string
	block  int64

	mu       sync.Mutex
	size     int64
	ranged   bool
	buffered []byte
	cache    map[int64][]byte
	order    []int64
	off      int64
}

func newHTTPRangeReader(ctx context.Context, client *http.Client, url string) *httpRangeReader {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		client = http.DefaultClient
	}

	return &httpRangeReader{
		ctx:    ctx,
		client: client,
		url:    url,
		block:  httpBlockSize,
		size:   -1,
		cache:  make(map[int64][]byte),
	}
}

func (r *httpRangeReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureSizeLocked(); err != nil {
		return 0, err
	}
	if r.off >= r.size {
		return 0, io.EOF
	}
	n, err := r.readAtLocked(p, r.off)
	r.off += int64(n)

	return n, err
}

func (r *httpRangeReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errNegativeOffset
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureSizeLocked(); err != nil {
		return 0, err
	}
	if off >= r.size {
		return 0, io.EOF
	}

	return r.readAtLocked(p, off)
}

func (r *httpRangeReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureSizeLocked(); err != nil {
		return 0, err
	}

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("provider: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errSeekOutOfRange
	}
	r.off = abs

	return abs, nil
}

// readAtLocked copies from off, fetching blocks as needed. It returns io.EOF
// only when the request runs past the end of the resource.
func (r *httpRangeReader) readAtLocked(p []byte, off int64) (int, error) {
	if r.buffered != nil {
		n := copy(p, r.buffered[off:])
		if n < len(p) {
			return n, io.EOF
		}

		return n, nil
	}

	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			break
		}
		idx := pos / r.block
		data, err := r.blockLocked(idx)
		if err != nil {
			return n, err
		}
		start := pos - idx*r.block
		if start >= int64(len(data)) {
			// The final, short block. Nothing more to read.
			break
		}
		n += copy(p[n:], data[start:])
	}
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// ensureSizeLocked performs the first request and resolves the resource size
// and the ranged capability. It is a no-op after the first call.
func (r *httpRangeReader) ensureSizeLocked() error {
	if r.size >= 0 {
		return nil
	}
	resp, err := r.doRangeLocked(0, r.block-1)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		total, err := contentRangeTotal(resp.Header.Get("Content-Range"))
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, r.block))
		if err != nil {
			return err
		}
		r.ranged = true
		r.size = total
		r.storeLocked(0, data)
	case http.StatusOK:
		// The server ignored Range and is sending the whole body. Buffer it so
		// the decoder still gets a seekable source.
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxFallbackBody+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > maxFallbackBody {
			return fmt.Errorf("provider: %s exceeds %d bytes and the server does not support Range", r.url, maxFallbackBody)
		}
		r.buffered = data
		r.size = int64(len(data))
	default:
		return fmt.Errorf("provider: %s returned %s", r.url, resp.Status)
	}

	return nil
}

// blockLocked returns the cached block at idx, fetching it when absent. The
// first block is filled by ensureSizeLocked, so a ranged source never re-fetches
// it.
func (r *httpRangeReader) blockLocked(idx int64) ([]byte, error) {
	if data, ok := r.cache[idx]; ok {
		return data, nil
	}
	if !r.ranged {
		// A non-ranged source was buffered whole, so reaching here is a logic
		// error, not a network path.
		return nil, errors.New("provider: block read on a non-ranged source")
	}

	start := idx * r.block
	end := start + r.block - 1
	if end >= r.size {
		end = r.size - 1
	}
	resp, err := r.doRangeLocked(start, end)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("provider: %s range %d-%d returned %s", r.url, start, end, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, r.block))
	if err != nil {
		return nil, err
	}
	r.storeLocked(idx, data)

	return data, nil
}

// storeLocked records a block and evicts the oldest once the cache is full.
// Eviction is FIFO, which is enough for the near-sequential access an index walk
// makes; a re-fetched block is a missed cache, never a wrong result.
func (r *httpRangeReader) storeLocked(idx int64, data []byte) {
	if _, ok := r.cache[idx]; !ok {
		r.order = append(r.order, idx)
	}
	r.cache[idx] = data
	for len(r.order) > httpMaxBlocks {
		oldest := r.order[0]
		r.order = r.order[1:]
		if oldest != idx {
			delete(r.cache, oldest)
		}
	}
}

func (r *httpRangeReader) doRangeLocked(start, end int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	return r.client.Do(req)
}

// contentRangeTotal parses the total from a Content-Range header such as
// "bytes 0-1048575/42352".
func contentRangeTotal(v string) (int64, error) {
	slash := strings.LastIndexByte(v, '/')
	if slash < 0 {
		return 0, fmt.Errorf("provider: malformed Content-Range %q", v)
	}
	total := strings.TrimSpace(v[slash+1:])
	if total == "*" {
		return 0, fmt.Errorf("provider: Content-Range %q has no total", v)
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("provider: malformed Content-Range %q: %w", v, err)
	}

	return n, nil
}
