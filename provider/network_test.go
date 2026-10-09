package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// rangeBody is a deterministic byte slice so a read at an offset is checkable
// without a real media file.
func rangeBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 31)
	}
	return b
}

// rangeServer serves body with real Range support and counts the requests, so a
// test can prove reads are ranged and cached rather than whole-file.
func rangeServer(t *testing.T, body []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "track.opus", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)

	return srv, &requests
}

// readFixture reads a decode test fixture, so the provider tests exercise a real
// container rather than a synthetic byte pattern.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "decode", "testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return data
}

func TestHTTPRangeReaderReadsAcrossBlocks(t *testing.T) {
	body := rangeBody(3*httpBlockSize + 123)
	srv, requests := rangeServer(t, body)

	rr := newHTTPRangeReader(context.Background(), srv.Client(), srv.URL)
	rr.block = 4096 // shrink the block so the test does not fetch megabytes

	// A read that spans several blocks must reassemble the bytes in order.
	got := make([]byte, len(body))
	n, err := rr.ReadAt(got, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(body) {
		t.Fatalf("ReadAt read %d bytes, want %d", n, len(body))
	}
	if !bytes.Equal(got, body) {
		t.Fatal("ReadAt across blocks returned wrong bytes")
	}
	// A sequential read fetches each block exactly once: block 0 by the size
	// probe, the rest on demand. One request per block, never per read.
	blocks := (len(body) + int(rr.block) - 1) / int(rr.block)
	if requests.Load() != int64(blocks) {
		t.Fatalf("requests = %d, want %d (one per block, none repeated)", requests.Load(), blocks)
	}
}

func TestHTTPRangeReaderSeekAndRead(t *testing.T) {
	body := rangeBody(100000)
	srv, _ := rangeServer(t, body)

	rr := newHTTPRangeReader(context.Background(), srv.Client(), srv.URL)
	rr.block = 4096

	off, err := rr.Seek(-100, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if off != int64(len(body)-100) {
		t.Fatalf("Seek to end = %d, want %d", off, len(body)-100)
	}
	buf := make([]byte, 100)
	if _, err := io.ReadFull(rr, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(buf, body[len(body)-100:]) {
		t.Fatal("read after SeekEnd returned wrong bytes")
	}

	// Read at the very end is EOF, not data.
	if _, err := rr.ReadAt(make([]byte, 1), int64(len(body))); err != io.EOF {
		t.Fatalf("ReadAt past end = %v, want io.EOF", err)
	}
}

func TestHTTPRangeReaderCachesBlocks(t *testing.T) {
	body := rangeBody(50000)
	srv, requests := rangeServer(t, body)

	rr := newHTTPRangeReader(context.Background(), srv.Client(), srv.URL)
	rr.block = 4096

	// The container readers seek constantly. Two passes over the same window
	// must hit the cache the second time.
	if _, err := rr.ReadAt(make([]byte, 8192), 100); err != nil && err != io.EOF {
		t.Fatalf("first ReadAt: %v", err)
	}
	after := requests.Load()
	if _, err := rr.ReadAt(make([]byte, 8192), 100); err != nil && err != io.EOF {
		t.Fatalf("second ReadAt: %v", err)
	}
	if requests.Load() != after {
		t.Fatalf("a cached read made a request: %d -> %d", after, requests.Load())
	}
}

func TestHTTPRangeReaderEvictsOldBlocks(t *testing.T) {
	body := rangeBody((httpMaxBlocks + 4) * 4096)
	srv, requests := rangeServer(t, body)

	rr := newHTTPRangeReader(context.Background(), srv.Client(), srv.URL)
	rr.block = 4096

	// Touch more distinct blocks than the cache holds, so the oldest evict.
	for i := 0; i < httpMaxBlocks+4; i++ {
		if _, err := rr.ReadAt(make([]byte, 16), int64(i)*4096); err != nil {
			t.Fatalf("ReadAt block %d: %v", i, err)
		}
	}
	if len(rr.cache) > httpMaxBlocks {
		t.Fatalf("cache holds %d blocks, want at most %d", len(rr.cache), httpMaxBlocks)
	}
	// Block 0 was evicted, so re-reading it is one more request.
	before := requests.Load()
	if _, err := rr.ReadAt(make([]byte, 16), 0); err != nil {
		t.Fatalf("re-read block 0: %v", err)
	}
	if requests.Load() == before {
		t.Fatal("re-reading an evicted block did not re-request it")
	}
}

func TestHTTPRangeReaderFallsBackWhenRangeIgnored(t *testing.T) {
	body := rangeBody(20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Ignore Range entirely: a 200 with the whole body.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	rr := newHTTPRangeReader(context.Background(), srv.Client(), srv.URL)
	buf := make([]byte, 64)
	if _, err := rr.ReadAt(buf, 12345); err != nil {
		t.Fatalf("ReadAt on a non-ranged server: %v", err)
	}
	if !bytes.Equal(buf, body[12345:12345+64]) {
		t.Fatal("fallback read returned wrong bytes")
	}
	if rr.ranged {
		t.Fatal("reader marked a non-ranged server as ranged")
	}
}

func TestHTTPRangeReaderReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	rr := newHTTPRangeReader(context.Background(), srv.Client(), srv.URL)
	if _, err := rr.ReadAt(make([]byte, 4), 0); err == nil {
		t.Fatal("ReadAt on a 404 returned nil")
	}
}

func TestContentRangeTotal(t *testing.T) {
	cases := map[string]struct {
		want int64
		err  bool
	}{
		"bytes 0-1048575/42352": {42352, false},
		"bytes 0-1/*":           {0, true},
		"garbage":               {0, true},
	}
	for in, tc := range cases {
		got, err := contentRangeTotal(in)
		if tc.err {
			if err == nil {
				t.Fatalf("contentRangeTotal(%q) = %d, want error", in, got)
			}

			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("contentRangeTotal(%q) = %d, %v, want %d", in, got, err, tc.want)
		}
	}
}

func TestNetworkProviderMatch(t *testing.T) {
	p := NewNetworkProvider(nil)
	yes := []string{"http://a/b.opus", "https://a/b.opus", "https://example.com/x?y=1"}
	no := []string{"", "file.opus", "ftp://a/b.opus", "ytm:abc", "HTTP://A"}
	for _, ref := range yes {
		if !p.Match(ref) {
			t.Fatalf("Match(%q) = false, want true", ref)
		}
	}
	for _, ref := range no {
		if p.Match(ref) {
			t.Fatalf("Match(%q) = true, want false", ref)
		}
	}
	if p.Name() != "Network" {
		t.Fatalf("Name() = %q, want Network", p.Name())
	}
}

func TestNetworkProviderRejectsBadScheme(t *testing.T) {
	p := NewNetworkProvider(nil)
	if _, err := p.Open(context.Background(), "ftp://a/b.opus"); err == nil {
		t.Fatal("Open on a non-http scheme returned nil")
	}
}

func TestNetworkProviderOpenHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewNetworkProvider(nil).Open(ctx, "https://example.com/a.opus"); err == nil {
		t.Fatal("Open on a cancelled context returned nil")
	}
}

func TestNetworkLabels(t *testing.T) {
	cases := map[string][2]string{
		"/a/b.opus": {"ogg", "opus"},
		"/a/b.ogg":  {"ogg", "opus"},
		"/a/b.webm": {"webm", "opus"},
		"/a/b.weba": {"webm", "opus"},
		"/a/b.bin":  {"", ""},
	}
	for in, want := range cases {
		c, codec := networkLabels(in)
		if c != want[0] || codec != want[1] {
			t.Fatalf("networkLabels(%q) = %q/%q, want %q/%q", in, c, codec, want[0], want[1])
		}
	}
}

// TestNetworkProviderStreamsThroughRangeServer is the end-to-end proof: a real
// Ogg Opus fixture served over HTTP is opened through the provider's Opener and
// decoded. It exercises the whole seam: Match, Open, the ranged reader, the
// decode registry's reader dispatch and the Ogg Opus decoder.
func TestNetworkProviderStreamsThroughRangeServer(t *testing.T) {
	body := readFixture(t, "stereo_2s.opus")
	srv, requests := rangeServer(t, body)

	p := NewNetworkProvider(srv.Client())
	src, err := p.Open(context.Background(), srv.URL+"/stereo_2s.opus")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if src.Opener == nil {
		t.Fatal("Source has no Opener")
	}

	d, err := src.Opener(nil)
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	defer d.Close()

	// A ranged source must keep native seek: the decoder wrapper must not hide
	// the inner decoder's Seeker, or every seek would re-download the track.
	seeker, ok := d.(interface{ SeekFrame(int64) error })
	if !ok {
		t.Fatal("the provider's decoder lost its native seek")
	}
	if err := seeker.SeekFrame(48000); err != nil {
		t.Fatalf("SeekFrame: %v", err)
	}

	// The debug labels must survive the wrapper too.
	desc, ok := d.(interface {
		DecoderName() string
		ParserName() string
	})
	if !ok {
		t.Fatal("the provider's decoder lost its Descriptor")
	}
	if desc.DecoderName() == "" || desc.ParserName() == "" {
		t.Fatalf("Descriptor = %q/%q, want both labels", desc.DecoderName(), desc.ParserName())
	}

	// The decoder must have learned the real length from the container index
	// built over ranged reads, which is the whole point of the range reader.
	if got := d.Info().TotalFrames; got <= 0 {
		t.Fatalf("TotalFrames = %d, want a positive count from the index", got)
	}

	// Probe replays the shape the decoder reported.
	info, err := src.Probe(0)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != d.Info().TotalFrames {
		t.Fatalf("Probe TotalFrames = %d, want the decoder's %d", info.TotalFrames, d.Info().TotalFrames)
	}

	// The Ogg index walk must be bounded: a handful of requests for a 42 KiB
	// file, not one per page. Generous bound so the test is about "not per
	// page", not about an exact request count.
	if n := requests.Load(); n > 40 {
		t.Fatalf("served %d requests for a 42 KiB file; the index walk is not block-cached", n)
	}
}

func TestHTTPRangeReaderIntegrationReadsWholeBody(t *testing.T) {
	body := rangeBody(200000)
	srv, _ := rangeServer(t, body)
	rr := newHTTPRangeReader(context.Background(), srv.Client(), srv.URL)
	got, err := io.ReadAll(rr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("ReadAll through the range reader returned wrong bytes")
	}
}
