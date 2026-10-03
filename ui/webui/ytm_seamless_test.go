package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/provider"
	ytm "github.com/dlcuy22/ytm-go"
)

// countingMediaServer serves the fixture and records how many media requests
// were made and whether they carried the Range header, so a test can tell a
// lazy background download from an eager one.
func countingMediaServer(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	data := fixtureBytes(t)
	var hits atomic.Int64
	var ranged atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Range") != "" {
			ranged.Store(true)
		}
		w.Header().Set("Content-Type", "audio/webm")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	return srv, &hits, &ranged
}

// openSeamlessSource resolves a seamless source whose media URL is the test
// server's root, so each test can shape the body the server returns.
func openSeamlessSource(t *testing.T, srv *httptest.Server) provider.Source {
	t.Helper()
	p := &ytmProvider{
		index:    newYTMIndex(),
		http:     srv.Client(),
		seamless: true,
		resolve: func(context.Context, string) (*ytm.Streams, error) {
			return ytmStreams(srv.URL+"/media", time.Now().Add(time.Hour)), nil
		},
	}
	src, err := p.Open(context.Background(), "ytm:abc123")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return src
}

// runWithin fails the test if fn does not return before the deadline. It is the
// guard that a blocked call is woken rather than parking the suite.
func runWithin(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out waiting for a blocked call to return")
	}
}

// sameFrames reports whether two interleaved PCM runs are identical.
func sameFrames(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// TestYTMSeamlessStartsLazilyAndUpgrades proves the fast-start contract: Open
// resolves a source that offers Upgrade without fetching anything, the opener
// starts one background download and decodes forward, and Upgrade reuses the
// buffered body to build a seekable decoder that reads real audio.
func TestYTMSeamlessStartsLazilyAndUpgrades(t *testing.T) {
	srv, hits, ranged := countingMediaServer(t)

	index := newYTMIndex()
	index.put(ytmTrack{ID: "abc123", Title: "Catalogue Title", Artist: "Catalogue Artist", Album: "Catalogue Album"})
	p := &ytmProvider{
		index:    index,
		http:     srv.Client(),
		seamless: true,
		resolve: func(context.Context, string) (*ytm.Streams, error) {
			return ytmStreams(srv.URL+"/media", time.Now().Add(time.Hour)), nil
		},
	}

	src, err := p.Open(context.Background(), "ytm:abc123")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if src.Upgrade == nil {
		t.Fatal("a seamless source must offer Upgrade")
	}
	// The fast path changes how the bytes arrive, not what the track is.
	if src.Meta.Tags.Title != "Catalogue Title" || src.Meta.Tags.Album != "Catalogue Album" {
		t.Fatalf("meta = %+v, want the catalogue tags", src.Meta.Tags)
	}
	if info, err := src.Probe(core.DurationProbe); err != nil || info.TotalFrames != 2*ytmSampleRate {
		t.Fatalf("Probe = (%+v, %v), want the catalogue duration", info, err)
	}
	// Nothing is fetched until the opener runs.
	if got := hits.Load(); got != 0 {
		t.Fatalf("media requests after Open = %d, want 0 (lazy download)", got)
	}

	dec, err := src.Opener(make(chan struct{}))
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	defer dec.Close()
	if got := dec.Info().Format; got != core.CanonicalFormat {
		t.Fatalf("forward decoder format = %+v, want canonical", got)
	}
	if seeker, ok := dec.(decode.Seeker); ok && seeker.SeekFrame(ytmSampleRate) == nil {
		t.Fatal("the streaming decoder must not seek")
	}
	buf := make([]float32, 4800)
	if n, err := dec.ReadFrames(buf); err != nil || n == 0 {
		t.Fatalf("ReadFrames = (%d, %v), want audio", n, err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("media requests after Opener = %d, want 1", got)
	}
	if !ranged.Load() {
		t.Fatal("the streaming request did not carry a Range header")
	}

	up, err := src.Upgrade(context.Background())
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if up == nil {
		t.Fatal("Upgrade returned no opener")
	}
	// The upgrade reuses the one buffered body rather than fetching again.
	if got := hits.Load(); got != 1 {
		t.Fatalf("media requests after Upgrade = %d, want 1", got)
	}

	seekable, err := up(make(chan struct{}))
	if err != nil {
		t.Fatalf("upgraded opener: %v", err)
	}
	defer seekable.Close()
	if _, ok := seekable.(decode.Seeker); !ok {
		t.Fatal("the upgraded decoder must be seekable")
	}
	buf = make([]float32, 4800)
	if n, err := seekable.ReadFrames(buf); err != nil || n == 0 {
		t.Fatalf("upgraded ReadFrames = (%d, %v), want audio", n, err)
	}
	if err := seekable.(decode.Seeker).SeekFrame(ytmSampleRate); err != nil {
		t.Fatalf("upgraded SeekFrame: %v", err)
	}
}

// TestYTMSeamlessPlaysBeforeDownloadFinishes is the headline proof: the opener
// returns and yields audio while the server still holds the tail of the body,
// and exactly one GET was made. The server writes the header and the opening
// clusters, flushes them, then waits for the test to release the rest.
func TestYTMSeamlessPlaysBeforeDownloadFinishes(t *testing.T) {
	data := fixtureBytes(t)
	split := len(data) / 2

	var gets atomic.Int64
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTail := func() { releaseOnce.Do(func() { close(release) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Type", "audio/webm")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:split])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(data[split:])
	}))
	t.Cleanup(func() {
		releaseTail()
		srv.Close()
	})

	src := openSeamlessSource(t, srv)
	dec, err := src.Opener(make(chan struct{}))
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	defer dec.Close()

	// The server is blocked on release, so any audio read here came from the
	// prefix alone: playback started before the download finished.
	buf := make([]float32, 4800)
	runWithin(t, 5*time.Second, func() {
		n, err := dec.ReadFrames(buf)
		if err != nil || n == 0 {
			t.Errorf("ReadFrames = (%d, %v) before the download finished, want audio", n, err)
		}
	})
	if got := gets.Load(); got != 1 {
		t.Fatalf("media GETs = %d, want 1", got)
	}

	// Releasing the tail completes the body, which Upgrade then wraps.
	releaseTail()
	runWithin(t, 5*time.Second, func() {
		up, err := src.Upgrade(context.Background())
		if err != nil {
			t.Errorf("Upgrade after the body completed: %v", err)

			return
		}
		seekable, err := up(make(chan struct{}))
		if err != nil {
			t.Errorf("upgraded opener: %v", err)

			return
		}
		defer seekable.Close()
		if err := seekable.(decode.Seeker).SeekFrame(ytmSampleRate); err != nil {
			t.Errorf("upgraded SeekFrame: %v", err)
		}
	})
}

// TestYTMSeamlessReopenStartsAtFrameZero proves the Opener is a factory: the
// streamer's reopen-and-discard fallback calls it again, and that second
// decoder must read from the beginning while the source still performs exactly
// one download.
func TestYTMSeamlessReopenStartsAtFrameZero(t *testing.T) {
	srv, hits, _ := countingMediaServer(t)
	src := openSeamlessSource(t, srv)

	first, err := src.Opener(make(chan struct{}))
	if err != nil {
		t.Fatalf("first Opener: %v", err)
	}
	ch := first.Info().Format.Ch
	fromStart := make([]float32, 4800)
	n, err := first.ReadFrames(fromStart)
	if err != nil || n == 0 {
		t.Fatalf("first ReadFrames = (%d, %v), want audio", n, err)
	}
	first.Close()

	second, err := src.Opener(make(chan struct{}))
	if err != nil {
		t.Fatalf("second Opener: %v", err)
	}
	defer second.Close()
	if got := hits.Load(); got != 1 {
		t.Fatalf("media GETs after two openers = %d, want 1 (the download runs once)", got)
	}

	again := make([]float32, 4800)
	m, err := second.ReadFrames(again)
	if err != nil || m == 0 {
		t.Fatalf("second ReadFrames = (%d, %v), want audio", m, err)
	}
	if m != n || !sameFrames(fromStart[:n*ch], again[:m*ch]) {
		t.Fatal("the second opener did not start at frame zero")
	}
}

// TestYTMSeamlessDisabledKeepsFullFetch pins the default: with the switch off
// there is no Upgrade and the opener fetches the whole track, exactly as the
// provider behaved before the fast path existed.
func TestYTMSeamlessDisabledKeepsFullFetch(t *testing.T) {
	srv, ranged := mediaServer(t)
	p := &ytmProvider{
		index: newYTMIndex(),
		http:  srv.Client(),
		resolve: func(context.Context, string) (*ytm.Streams, error) {
			return ytmStreams(srv.URL+"/media", time.Now().Add(time.Hour)), nil
		},
	}

	src, err := p.Open(context.Background(), "ytm:abc123")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if src.Upgrade != nil {
		t.Fatal("the default source must not offer Upgrade")
	}

	dec, err := src.Opener(make(chan struct{}))
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	defer dec.Close()
	buf := make([]float32, 4800)
	if n, err := dec.ReadFrames(buf); err != nil || n == 0 {
		t.Fatalf("ReadFrames = (%d, %v), want audio", n, err)
	}
	if !ranged.Load() {
		t.Fatal("the full fetch did not carry a Range header")
	}
}

// TestYTMSeamlessSurfacesDownloadFailure proves a failed background download
// reaches the caller instead of leaving a reader parked. The rejected-request
// case fails before a decoder exists; the cut-mid-body case opens a decoder
// from the whole track and then surfaces the download's own error through
// Upgrade, so a header-parse failure cannot masquerade as the tested path.
func TestYTMSeamlessSurfacesDownloadFailure(t *testing.T) {
	t.Run("rejectedRequest", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		src := openSeamlessSource(t, srv)
		runWithin(t, 5*time.Second, func() {
			if _, err := src.Opener(make(chan struct{})); err == nil {
				t.Error("Opener succeeded on a 500 media response")
			}
		})
		runWithin(t, 5*time.Second, func() {
			if _, err := src.Upgrade(context.Background()); err == nil {
				t.Error("Upgrade succeeded on a 500 media response")
			}
		})
	})

	t.Run("cutMidBody", func(t *testing.T) {
		data := fixtureBytes(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Serve the whole track but promise more, then return: the
			// connection closes mid-body, so the client sees ErrUnexpectedEOF
			// after the bytes it did receive. Serving the full track keeps this
			// independent of where any container element happens to end.
			w.Header().Set("Content-Length", strconv.Itoa(len(data)+4096))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		}))
		t.Cleanup(srv.Close)

		src := openSeamlessSource(t, srv)

		// The header parses from the bytes served, so a decoder really opens
		// and reads audio; the failure must be the cut body, not the header.
		dec, err := src.Opener(make(chan struct{}))
		if err != nil {
			t.Fatalf("Opener on a full-but-truncated body: %v", err)
		}
		defer dec.Close()
		buf := make([]float32, 4800)
		if n, err := dec.ReadFrames(buf); err != nil || n == 0 {
			t.Fatalf("ReadFrames = (%d, %v), want audio before the tail error", n, err)
		}

		// Upgrade waits for the download to end and must report its terminal
		// error: the body was cut, so that error is ErrUnexpectedEOF.
		_, err = src.Upgrade(context.Background())
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("Upgrade error = %v, want io.ErrUnexpectedEOF", err)
		}
	})
}

// TestYTMStreamBufferWakesBlockedReader proves a reader parked on an empty
// buffer is woken by the next append and returns exactly what arrived.
func TestYTMStreamBufferWakesBlockedReader(t *testing.T) {
	b := newYTMStreamBuffer()
	r := b.reader()

	started := make(chan struct{})
	got := make(chan string, 1)
	go func() {
		close(started)
		p := make([]byte, 4)
		n, err := r.Read(p)
		if err != nil {
			got <- "error: " + err.Error()

			return
		}
		got <- string(p[:n])
	}()

	<-started
	// Let the reader park on the empty buffer; the point is that the wake
	// follows the append rather than a poll.
	time.Sleep(10 * time.Millisecond)
	b.append([]byte("abcd"))

	select {
	case s := <-got:
		if s != "abcd" {
			t.Fatalf("Read got %q, want abcd", s)
		}
	case <-time.After(time.Second):
		t.Fatal("append did not wake a blocked Read")
	}
}

// TestYTMStreamBufferFinishesWithEOFOrError proves the drain rule: buffered
// bytes are served first, then the terminal state, and Read never returns
// (0, nil) for a non-empty destination.
func TestYTMStreamBufferFinishesWithEOFOrError(t *testing.T) {
	p := make([]byte, 8)

	eof := newYTMStreamBuffer()
	r := eof.reader()
	eof.append([]byte("abc"))
	eof.finish(nil)
	if n, err := r.Read(p); n != 3 || err != nil {
		t.Fatalf("Read = (%d, %v), want (3, nil)", n, err)
	}
	if n, err := r.Read(p); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read after drain = (%d, %v), want (0, io.EOF)", n, err)
	}

	cause := errors.New("download failed")
	failed := newYTMStreamBuffer()
	rf := failed.reader()
	failed.append([]byte("xy"))
	failed.finish(cause)
	if n, err := rf.Read(p); n != 2 || err != nil {
		t.Fatalf("Read = (%d, %v), want (2, nil)", n, err)
	}
	if n, err := rf.Read(p); n != 0 || !errors.Is(err, cause) {
		t.Fatalf("Read after drain = (%d, %v), want (0, %v)", n, err, cause)
	}
}

// TestYTMStreamBufferWaitReturnsBodyAndHonoursContext proves wait hands back
// the complete body once the download ends and gives up promptly when its
// context is cancelled before that.
func TestYTMStreamBufferWaitReturnsBodyAndHonoursContext(t *testing.T) {
	b := newYTMStreamBuffer()
	b.append([]byte("hello"))
	b.finish(nil)
	data, err := b.wait(context.Background())
	if err != nil || string(data) != "hello" {
		t.Fatalf("wait = (%q, %v), want hello", data, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := newYTMStreamBuffer().wait(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not return on context cancellation")
	}
}
