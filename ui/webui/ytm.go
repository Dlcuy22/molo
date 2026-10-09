// YouTube Music as an audio source for the engine.
//
// This file is the consumer half of the provider seam: the engine knows nothing
// about YouTube, and everything network-facing lives here, in the UI module.
// The provider resolves a "ytm:<videoId>" reference into a Source whose Opener
// hands the engine a decoder over the track bytes.
//
// The track is fetched whole, into memory, with one ranged GET. That is a
// deliberate choice over true streaming: the WebM reader indexes every block at
// open to place seeks, so a streaming reader would walk the file anyway and
// then fetch it a second time to play it. A music track is a few megabytes, so
// one fetch is both simpler and faster, and it keeps native seek exact.
//
// The web UI opts into the experimental seamless path (ytmSeamlessSwap): a
// forward-only decoder plays the download as it arrives, and Upgrade swaps in
// the whole-track seekable decoder once the body is complete, so the first
// sound no longer waits for the last byte. Flipping that one value off disables
// the path everywhere in the UI and restores the whole-track fetch.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/meta"
	"github.com/dlcuy22/molo/provider"
	"github.com/dlcuy22/molo/stream"
	"github.com/dlcuy22/molo/ui/webui/internal/cover"
	ytm "github.com/dlcuy22/ytm-go"
)

const (
	// ytmRefPrefix is the scheme that marks a reference as a YouTube Music
	// track. It is the only thing the engine sees, so it must not look like a
	// filesystem path.
	ytmRefPrefix = "ytm:"

	// ytmMaxTrackBytes bounds one track held in memory. A two hour set at the
	// top music bitrate is well under this; the bound exists so a wrong
	// Content-Length or an endless body cannot exhaust memory.
	ytmMaxTrackBytes = 256 << 20

	// ytmMaxThumbBytes bounds one thumbnail download. Art is decorative, so a
	// body past this is dropped rather than kept.
	ytmMaxThumbBytes = 4 << 20

	// ytmHTTPTimeout bounds one media or thumbnail request.
	ytmHTTPTimeout = 60 * time.Second

	// ytmSampleRate is the rate the WebM/Opus reader always outputs. It is what
	// converts a duration in milliseconds to the engine's frame domain.
	ytmSampleRate = 48000

	// ytmIndexMax bounds the catalogue metadata cache. Search results fill it,
	// so without a bound a long session would grow it with every query.
	ytmIndexMax = 512

	// ytmPlaylistMaxTracks and ytmPlaylistMaxPages bound one playlist add. A
	// long playlist is still bounded work: the walk stops at the track cap, or
	// at the page cap if the catalogue keeps handing back continuations. The
	// bounds are stated rather than silent so a truncated add is a known limit.
	ytmPlaylistMaxTracks = 1000
	ytmPlaylistMaxPages  = 40

	// ytmSeamlessSwap is the one switch for the experimental fast-start path.
	// The web UI is the consumer and opts in here, at initialization: this value
	// is read by both the provider, which then offers the Upgrade, and the
	// engine, which then consumes it, so the two cannot drift. Flipping this one
	// value off disables the path everywhere in the UI and restores the
	// whole-track fetch.
	ytmSeamlessSwap = true
)

// ytmRef builds the queue reference for a video id. CleanSongID strips the
// "MPED" prefix some catalogue responses carry, so the same track has one ref
// however it was reached.
func ytmRef(id string) string { return ytmRefPrefix + ytm.CleanSongID(id) }

// ytmID reads the video id back out of a reference.
func ytmID(ref string) string { return ytm.CleanSongID(strings.TrimPrefix(ref, ytmRefPrefix)) }

// isYTMRef reports whether a queue entry belongs to this provider. It is the
// cheap check the session calls for every queued ref.
func isYTMRef(ref string) bool { return strings.HasPrefix(ref, ytmRefPrefix) }

// ytmTrack is what the UI knows about one video id: the catalogue metadata and,
// once it has been fetched, the artwork bytes. It is the row's identity, so it
// carries no URL the UI would have to fetch itself.
type ytmTrack struct {
	ID         string
	Title      string
	Artist     string
	Album      string
	DurationMs int64

	// ArtistID is the browse id of the first credited artist. It is the key the
	// avatar lookup uses, and it is kept even when no avatar has been resolved
	// yet, so the lookup can happen later.
	ArtistID string

	// ArtistThumbURL is the resolved avatar URL, or "" when the artist has none
	// or has not been looked up. It is a URL rather than bytes because only the
	// Discord presence consumes it, and Discord fetches an https URL itself.
	ArtistThumbURL string

	// ThumbnailURL is the high quality art URL from the catalogue. It is kept
	// so the art can be fetched lazily, when a row is drawn or the track is
	// played, rather than for every search result at once.
	ThumbnailURL string

	// Thumb is the fetched art, filled on the first fetch and reused after.
	Thumb []byte
}

// coverID identifies the artwork for the UI. It prefers the content hash once
// the bytes are here, so a track played and then listed shares one cache entry
// with its now-playing art; before that it falls back to a hash of the URL,
// which is stable across the session.
func (t ytmTrack) coverID() string {
	if len(t.Thumb) > 0 {
		return cover.ID(t.Thumb)
	}
	if t.ThumbnailURL != "" {
		return ytmURLID(t.ThumbnailURL)
	}

	return ""
}

// ytmURLID is a short, stable id for a thumbnail URL. It keeps the long signed
// URL out of the 4 Hz snapshot while still changing when the art changes.
func ytmURLID(url string) string {
	h := fnv.New64a()
	_, _ = io.WriteString(h, url)

	return "ytm-" + strconv.FormatUint(h.Sum64(), 16)
}

// ytmIndex caches the metadata of every video id the session has seen, so a
// queue row for a remote ref shows the real title, artist and art instead of
// the reference string. It is filled from search results and from the provider
// when a track is opened.
type ytmIndex struct {
	mu      sync.Mutex
	entries map[string]ytmTrack
}

func newYTMIndex() *ytmIndex {
	return &ytmIndex{entries: make(map[string]ytmTrack)}
}

// put records a track under its reference. An entry with no thumbnail URL keeps
// any art already fetched, so a later search hit cannot erase the bytes.
func (x *ytmIndex) put(t ytmTrack) {
	ref := ytmRef(t.ID)
	if ref == ytmRefPrefix {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	prev := x.entries[ref]
	if t.ThumbnailURL == "" {
		t.ThumbnailURL = prev.ThumbnailURL
	}
	if len(t.Thumb) == 0 {
		t.Thumb = prev.Thumb
	}
	// The artist id and its resolved avatar are filled in by a later, separate
	// lookup, so a subsequent search hit that carries neither must not erase
	// them.
	if t.ArtistID == "" {
		t.ArtistID = prev.ArtistID
	}
	if t.ArtistThumbURL == "" {
		t.ArtistThumbURL = prev.ArtistThumbURL
	}
	x.entries[ref] = t
}

// setArtist records the resolved avatar (and the artist id it belongs to) on an
// existing entry, leaving the rest alone.
func (x *ytmIndex) setArtist(ref, artistID, avatarURL string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	t, ok := x.entries[ref]
	if !ok {
		return
	}
	if artistID != "" {
		t.ArtistID = artistID
	}
	t.ArtistThumbURL = avatarURL
	x.entries[ref] = t
}

// lookup returns the cached track for a reference.
func (x *ytmIndex) lookup(ref string) (ytmTrack, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	t, ok := x.entries[ref]

	return t, ok
}

// setThumb records fetched artwork for a reference, leaving the rest of the
// entry alone.
func (x *ytmIndex) setThumb(ref string, thumb []byte) {
	x.mu.Lock()
	defer x.mu.Unlock()
	t, ok := x.entries[ref]
	if !ok {
		return
	}
	t.Thumb = thumb
	x.entries[ref] = t
}

// prune bounds the index. It drops entries that are not in the protected set,
// and only once the cap is exceeded, so a reference cached for a search and
// not yet in the queue survives the gap between the play command and the
// control loop installing the queue.
func (x *ytmIndex) prune(protected map[string]struct{}) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.entries) <= ytmIndexMax {
		return
	}
	for ref := range x.entries {
		if len(x.entries) <= ytmIndexMax {
			return
		}
		if _, ok := protected[ref]; ok {
			continue
		}
		delete(x.entries, ref)
	}
}

// ytmProvider resolves YouTube Music references. It implements
// provider.AudioProvider.
type ytmProvider struct {
	index *ytmIndex
	http  *http.Client

	// artists caches resolved artist avatars for the presence's small image. It
	// is separate from the track index because one artist can front many tracks.
	artists *ytmArtistIndex

	// avatarMu guards the decoded-avatar cache, which serves the settings
	// preview. It is keyed by artist id and holds a data URL, so the preview
	// never reaches the image host itself.
	avatarMu   sync.Mutex
	avatarData map[string]string

	log *slog.Logger

	// seamless selects the experimental fast-start path: Open returns a
	// forward-only Opener over a background download plus an Upgrade that wraps
	// the completed body in a seekable decoder. The UI sets it from
	// ytmSeamlessSwap, the single switch shared with the engine; with that off,
	// Open keeps the whole-track fetch exactly as before.
	seamless bool

	// resolve, search, loadArtist and loadPlaylist are the network calls,
	// indirected so a test can drive the provider and the service without
	// touching YouTube. In production they are the client's own methods; the
	// client itself is not kept, because nothing else on it is used.
	resolve      func(context.Context, string) (*ytm.Streams, error)
	search       func(context.Context, string, string, bool) (*ytm.SearchResults, error)
	loadArtist   func(context.Context, string) (*ytm.Artist, error)
	loadPlaylist func(context.Context, string, *ytm.BuiltInContinuation) (*ytm.Playlist, error)
}

func newYTMProvider(index *ytmIndex, seamless bool, log *slog.Logger) *ytmProvider {
	if log == nil {
		log = slog.Default()
	}
	client := ytm.NewClient()

	return &ytmProvider{
		index:      index,
		artists:    newYTMArtistIndex(),
		avatarData: make(map[string]string),
		log:        log,
		http:       &http.Client{Timeout: ytmHTTPTimeout},
		seamless:   seamless,
		resolve:    client.GetStream,
		search:     client.Search,
		loadArtist: client.LoadArtist,
		loadPlaylist: func(ctx context.Context, id string, cont *ytm.BuiltInContinuation) (*ytm.Playlist, error) {
			return client.LoadPlaylist(ctx, id, cont, nil, nil, false)
		},
	}
}

func (p *ytmProvider) Name() string { return "ytm" }

func (p *ytmProvider) Match(ref string) bool { return isYTMRef(ref) }

// Open resolves a reference into a playable Source. The stream lookup and the
// artwork fetch happen here, on the session's build worker; the audio fetch is
// deferred to the Opener, which the streamer also calls again on a seek
// fallback, so the fetch always starts from the beginning.
func (p *ytmProvider) Open(ctx context.Context, ref string) (provider.Source, error) {
	if err := ctx.Err(); err != nil {
		return provider.Source{}, err
	}
	id := ytmID(ref)
	if id == "" {
		return provider.Source{}, fmt.Errorf("ytm: %q carries no track id", ref)
	}

	streams, err := p.resolve(ctx, id)
	if err != nil {
		return provider.Source{}, err
	}
	best, ok := streams.Best()
	if !ok {
		return provider.Source{}, fmt.Errorf("ytm: no audio format for %s", id)
	}

	track := p.trackFor(ref, streams)
	if thumb := p.thumbFor(ctx, track.ThumbnailURL); len(thumb) > 0 {
		track.Thumb = thumb
		p.index.setThumb(ref, thumb)
	}
	p.index.put(track)

	src := provider.Source{
		Meta: meta.Meta{
			Container: "webm",
			Codec:     "opus",
			Tags: meta.Tags{
				Title:  track.Title,
				Artist: track.Artist,
				Album:  track.Album,
			},
		},
	}
	// The now-playing artwork rides in the meta the way a tagged file's does,
	// so the existing cover path needs no special case for a remote track.
	if len(track.Thumb) > 0 {
		src.Meta.Tags.Cover = track.Thumb
		src.Meta.Tags.CoverMIME = http.DetectContentType(track.Thumb)
	}
	if frames := ytmFrames(track.DurationMs); frames >= 0 {
		src.Meta.Stream = core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: frames}
		src.Probe = func(core.DurationMode) (core.StreamInfo, error) {
			return core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: frames}, nil
		}
	}

	url := best.URL
	expires := best.ExpiresAt

	// refresh re-resolves a signed URL that has expired. It is shared by the
	// full-fetch opener and the streaming start: a signed URL is bound to this
	// host and dies after a few hours, and the streamer calls the opener again
	// on the reopen-and-discard seek fallback and on a decoder swap, so
	// re-resolving on expiry is what keeps a long session seeking correctly.
	refresh := func() error {
		if expires.IsZero() || !time.Now().After(expires) {
			return nil
		}
		fresh, err := p.resolve(ctx, id)
		if err != nil {
			return err
		}
		f, ok := fresh.Best()
		if !ok {
			return fmt.Errorf("ytm: no audio format for %s", id)
		}
		url = f.URL
		expires = f.ExpiresAt

		return nil
	}

	if p.seamless {
		// Fast start: the body downloads once, in the background, while the
		// forward-only decoder plays from the front, so audio begins before the
		// whole track is here. Upgrade waits for the last byte and swaps in a
		// seekable decoder over the same buffer, so a seek after the switch is
		// exact and needs no second fetch.
		buf := newYTMStreamBuffer()
		var once sync.Once
		start := func(stop <-chan struct{}) {
			once.Do(func() {
				if err := refresh(); err != nil {
					buf.finish(err)

					return
				}
				go p.download(ctx, stop, url, ytmMaxTrackBytes, buf)
			})
		}
		src.Opener = func(stop <-chan struct{}) (decode.Decoder, error) {
			start(stop)

			return decode.NewWebMOpusFactory().OpenReader(buf.reader())
		}
		src.Upgrade = func(uctx context.Context) (stream.Opener, error) {
			// The opener normally starts the download already; starting here
			// too keeps a caller that upgrades first from waiting on a body
			// nobody fetched.
			start(nil)
			data, err := buf.wait(uctx)
			if err != nil {
				return nil, err
			}

			return func(<-chan struct{}) (decode.Decoder, error) {
				return decode.NewWebMOpusFactory().OpenReader(bytes.NewReader(data))
			}, nil
		}

		return src, nil
	}

	src.Opener = func(stop <-chan struct{}) (decode.Decoder, error) {
		if err := refresh(); err != nil {
			return nil, err
		}

		data, err := p.fetch(ctx, stop, url, ytmMaxTrackBytes)
		if err != nil {
			return nil, err
		}

		return decode.NewWebMOpusFactory().OpenReader(bytes.NewReader(data))
	}

	return src, nil
}

// playlist walks a playlist's pages, flattening each into the rows the service
// caches and queues. The walk is bounded so a runaway continuation cannot spin:
// it stops at the track cap, or at the page cap if the catalogue keeps handing
// back tokens. A nil continuation ends it, so a single-page playlist costs one
// request.
func (p *ytmProvider) playlist(ctx context.Context, id string) ([]YTMResult, error) {
	var out []YTMResult
	seen := make(map[string]struct{})
	var cont *ytm.BuiltInContinuation

	for page := 0; page < ytmPlaylistMaxPages; page++ {
		pl, err := p.loadPlaylist(ctx, id, cont)
		if err != nil {
			return nil, err
		}
		if pl == nil {
			break
		}
		for _, r := range ytmPlaylistResults(pl) {
			if _, ok := seen[r.VideoID]; ok {
				continue
			}
			seen[r.VideoID] = struct{}{}
			out = append(out, r)
			if len(out) >= ytmPlaylistMaxTracks {
				return out, nil
			}
		}
		if pl.Continuation == nil {
			break
		}
		cont = pl.Continuation
	}

	return out, nil
}

// trackFor merges the catalogue metadata already cached for a reference with
// what the stream lookup reported, preferring the catalogue copy: it is the one
// that carries the album and the artwork.
func (p *ytmProvider) trackFor(ref string, streams *ytm.Streams) ytmTrack {
	t, _ := p.index.lookup(ref)
	t.ID = firstNonEmpty(t.ID, streams.VideoID, ytmID(ref))
	t.Title = firstNonEmpty(t.Title, streams.Title)
	t.Artist = firstNonEmpty(t.Artist, streams.Author)
	if t.DurationMs <= 0 {
		t.DurationMs = streams.DurationMs
	}

	return t
}

// artistArtFor resolves the avatar URL for a cached track, looking it up and
// caching it on the first ask. It is called from the presence update, off the
// playback path, so the browse round trip never delays a track's start.
func (p *ytmProvider) artistArtFor(ctx context.Context, ref string) string {
	t, ok := p.index.lookup(ref)
	if !ok {
		return ""
	}
	if t.ArtistThumbURL != "" {
		return t.ArtistThumbURL
	}
	url := p.artistArt(ctx, t.ArtistID)
	if url != "" {
		p.index.setArtist(ref, t.ArtistID, url)
	}

	return url
}

// thumbFor fetches one thumbnail, best effort. Art is decorative, so a failure
// returns nothing rather than failing the track. ctx is honoured so a shutdown
// does not wait out the HTTP timeout on a decorative fetch.
func (p *ytmProvider) thumbFor(ctx context.Context, url string) []byte {
	if url == "" {
		return nil
	}
	data, err := p.fetch(ctx, nil, url, ytmMaxThumbBytes)
	if err != nil {
		return nil
	}

	return data
}

// fetch reads one URL whole into memory through the ranged GET that withBody
// issues, bounded by limit.
func (p *ytmProvider) fetch(parent context.Context, stop <-chan struct{}, url string, limit int64) ([]byte, error) {
	var data []byte
	err := p.withBody(parent, stop, url, func(body io.Reader) error {
		var rerr error
		data, rerr = io.ReadAll(io.LimitReader(body, limit+1))

		return rerr
	})
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("ytm: media body exceeds %d bytes", limit)
	}

	return data, nil
}

// download streams one URL into buf with the ranged GET, stopping at limit
// bytes so an endless or lying body cannot exhaust memory. The terminal error
// is published through buf, so a forward reader blocked mid-track sees the
// failure instead of parking forever.
func (p *ytmProvider) download(parent context.Context, stop <-chan struct{}, url string, limit int64, buf *ytmStreamBuffer) {
	err := p.withBody(parent, stop, url, func(body io.Reader) error {
		chunk := make([]byte, 64<<10)
		var total int64
		for {
			n, rerr := body.Read(chunk)
			if n > 0 {
				total += int64(n)
				if total > limit {
					return fmt.Errorf("ytm: media body exceeds %d bytes", limit)
				}
				buf.append(chunk[:n])
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					return nil
				}

				return rerr
			}
		}
	})
	buf.finish(err)
}

// withBody issues the ranged GET the full fetch and the streaming download
// share, and hands the live body to consume. The Range header is not an
// optimisation: googlevideo throttles a GET without one to roughly 32 KiB/s,
// measured at about eighty times slower on the same track.
//
// stop, when non-nil, cancels the request; the streamer closes it on shutdown
// so an opener cannot strand a producer goroutine in a blocked read. parent, if
// non-nil, is the session context, cancelled when the player closes.
func (p *ytmProvider) withBody(parent context.Context, stop <-chan struct{}, url string, consume func(io.Reader) error) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	if stop != nil {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-stop:
				cancel()
			case <-done:
			}
		}()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-")

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("ytm: media request returned %s", resp.Status)
	}

	return consume(resp.Body)
}

// ytmStreamBuffer holds one track body as it downloads. A single producer
// appends chunks; each reader takes its own forward cursor, blocking on a
// condition variable, rather than spinning, until the bytes it wants are here
// or the download ends. The data is grow-only and shared, so the seekable
// upgrade reuses it with no second fetch and a reopen starts clean.
type ytmStreamBuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	data []byte
	done bool
	err  error
}

func newYTMStreamBuffer() *ytmStreamBuffer {
	b := &ytmStreamBuffer{}
	b.cond = sync.NewCond(&b.mu)

	return b
}

// append records one chunk and wakes any reader waiting on it.
func (b *ytmStreamBuffer) append(p []byte) {
	if len(p) == 0 {
		return
	}
	b.mu.Lock()
	b.data = append(b.data, p...)
	b.cond.Broadcast()
	b.mu.Unlock()
}

// finish marks the end of the download. err, when non-nil, is what a reader
// gets once the buffered bytes are drained, so a truncated body surfaces as a
// read error rather than as a silent short read.
func (b *ytmStreamBuffer) finish(err error) {
	b.mu.Lock()
	b.done = true
	b.err = err
	b.cond.Broadcast()
	b.mu.Unlock()
}

// reader returns a fresh forward reader over the body. It starts at the
// beginning, so a reopen reads the track again instead of continuing from
// wherever a previous reader stopped.
func (b *ytmStreamBuffer) reader() io.Reader {
	return &ytmStreamReader{buf: b}
}

// wait blocks until the download finishes or ctx is done, then returns the
// complete body. Cancellation is an error rather than a partial result: a
// truncated body cannot back a seekable decoder, so the swap must not happen.
func (b *ytmStreamBuffer) wait(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Waking on cancellation takes the lock, so the wake cannot slip between
	// the check in the loop and the park; Broadcast alone would race.
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	})
	defer stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	for !b.done {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.cond.Wait()
	}
	if b.err != nil {
		return nil, b.err
	}

	return b.data, nil
}

// ytmStreamReader is the forward-only view the WebM factory sees: it implements
// Read but not io.Seeker, which makes the factory choose its forward-only
// reader and is what lets playback start mid-download.
type ytmStreamReader struct {
	buf *ytmStreamBuffer
	off int
}

func (r *ytmStreamReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.buf.mu.Lock()
	defer r.buf.mu.Unlock()
	for r.off >= len(r.buf.data) && !r.buf.done {
		r.buf.cond.Wait()
	}
	n := copy(p, r.buf.data[r.off:])
	r.off += n
	if n > 0 {
		return n, nil
	}
	if r.buf.err != nil {
		return 0, r.buf.err
	}

	return 0, io.EOF
}

// fetchThumb fills in the artwork of a cached track, so a queue row can draw it
// without a second lookup. It is called off the lock, from the UI thread.
func (p *ytmProvider) fetchThumb(ref string) []byte {
	t, ok := p.index.lookup(ref)
	if !ok {
		return nil
	}
	if len(t.Thumb) > 0 {
		return t.Thumb
	}
	data := p.thumbFor(context.Background(), t.ThumbnailURL)
	if len(data) > 0 {
		p.index.setThumb(ref, data)
	}

	return data
}

// ytmFrames converts a duration in milliseconds to the engine's 48 kHz frame
// domain. An unknown duration is -1, which the engine reports as "not yet
// known" rather than as a zero length track.
func ytmFrames(durationMs int64) int64 {
	if durationMs <= 0 {
		return -1
	}

	return durationMs * ytmSampleRate / 1000
}

// YTMResult is one search hit, flattened for the frontend. Kind is the label a
// row shows; only a song is playable today, and a row says so rather than
// offering a control that does nothing.
type YTMResult struct {
	VideoID    string `json:"videoId"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	DurationMs int64  `json:"durationMs"`
	Kind       string `json:"kind"`
	Explicit   bool   `json:"explicit"`
	Thumbnail  string `json:"thumbnail"`
	// ArtistID is the first credited artist's browse id, carried so the
	// presence's avatar lookup has a key without a second search. It is empty
	// when the catalogue named no artist.
	ArtistID string `json:"artistId"`
	Playable bool   `json:"playable"`
}

// ytmResults flattens a search response into the rows the palette shows, songs
// first. A song from the catalogue is playable; an album, artist or playlist is
// listed with its kind and marked not playable, so the palette can show it
// without pretending it can be played. A video is playable too, but it is a
// video, so it sorts after every song.
//
// Order is stable within each bucket, and duplicates are dropped, because the
// same song can appear in more than one shelf.
func ytmResults(res *ytm.SearchResults) []YTMResult {
	if res == nil {
		return nil
	}

	var songs, videos, others []YTMResult
	seen := make(map[string]struct{})

	add := func(r YTMResult, bucket *[]YTMResult) {
		// Dedupe on the item's identity alone, not on the kind: the same song
		// can be offered by a song shelf and a video shelf at once, and both
		// map to one video id. The id is the identity; the title is the
		// fallback for an item the catalogue gave no id.
		key := r.VideoID
		if key == "" {
			key = "\x00" + r.Title
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		*bucket = append(*bucket, r)
	}

	for _, cat := range res.Categories {
		for _, item := range cat.Layout.Items {
			switch it := item.(type) {
			case *ytm.Song:
				if it == nil {
					continue
				}
				r := YTMResult{
					VideoID:    ytm.CleanSongID(it.ID),
					Title:      it.Name,
					Artist:     ytmArtists(it.Artists),
					Album:      ytmAlbum(it.Album),
					DurationMs: it.DurationMs,
					Kind:       "Song",
					Explicit:   it.IsExplicit,
					Thumbnail:  ytmThumb(it.Thumbnail),
					ArtistID:   ytmFirstArtistID(it.Artists),
					Playable:   true,
				}
				if it.Type == ytm.SongTypeVideo {
					r.Kind = "Video"
					add(r, &videos)

					continue
				}
				add(r, &songs)
			case *ytm.Playlist:
				if it == nil {
					continue
				}
				kind := "Playlist"
				if it.Type == ytm.PlaylistTypeAlbum {
					kind = "Album"
				}
				add(YTMResult{
					VideoID:   it.ID,
					Title:     it.Name,
					Artist:    ytmArtists(it.Artists),
					Kind:      kind,
					Thumbnail: ytmThumb(it.Thumbnail),
				}, &others)
			case *ytm.Artist:
				if it == nil {
					continue
				}
				add(YTMResult{
					VideoID:   it.ID,
					Title:     it.Name,
					Kind:      "Artist",
					Thumbnail: ytmThumb(it.Thumbnail),
				}, &others)
			}
		}
	}

	out := make([]YTMResult, 0, len(songs)+len(videos)+len(others))
	out = append(out, songs...)
	out = append(out, videos...)

	return append(out, others...)
}

// ytmPlaylistID extracts a playlist id from the input a listener gives: the
// share URL they copy, a "VL"-prefixed browse id, or a bare id. It returns ""
// when nothing usable is there.
func ytmPlaylistID(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	// A share URL carries the id as its list= parameter; the query string is
	// the only place the id is guaranteed to be, so it is read first.
	if i := strings.Index(input, "list="); i != -1 {
		input = input[i+len("list="):]
		if j := strings.IndexByte(input, '&'); j != -1 {
			input = input[:j]
		}
	}
	// CleanPlaylistID strips the "MPSP" prefix the editor uses; the browse
	// call adds "VL" itself, so a bare id is exactly what LoadPlaylist wants.
	return ytm.CleanPlaylistID(strings.TrimSpace(input))
}

// ytmPlaylistResults flattens a loaded playlist's tracks into the same rows a
// search yields, so the catalogue index and the queue share one shape. Order is
// the catalogue's, and a repeated id is kept once: a playlist can list a track
// twice, but the queue should not.
func ytmPlaylistResults(p *ytm.Playlist) []YTMResult {
	if p == nil {
		return nil
	}
	out := make([]YTMResult, 0, len(p.Items))
	seen := make(map[string]struct{}, len(p.Items))
	for _, it := range p.Items {
		id := ytm.CleanSongID(it.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		album := ytmAlbum(it.Album)
		if album == "" && p.Type == ytm.PlaylistTypeAlbum {
			// An album's tracks often carry no per-track album credit; the
			// playlist name is the album, which is a fact, not a guess.
			album = strings.TrimSpace(p.Name)
		}
		out = append(out, YTMResult{
			VideoID:    id,
			Title:      it.Name,
			Artist:     ytmArtists(it.Artists),
			Album:      album,
			DurationMs: it.DurationMs,
			Kind:       "Song",
			Explicit:   it.IsExplicit,
			Thumbnail:  ytmThumb(it.Thumbnail),
			ArtistID:   ytmFirstArtistID(it.Artists),
			Playable:   true,
		})
	}

	return out
}

// ytmArtists joins the credited artists, dropping blanks so a partly populated
// credit does not render a dangling separator.
func ytmArtists(artists []ytm.Artist) string {
	names := make([]string, 0, len(artists))
	for _, a := range artists {
		if name := strings.TrimSpace(a.Name); name != "" {
			names = append(names, name)
		}
	}

	return strings.Join(names, ", ")
}

// ytmFirstArtistID returns the browse id of the first credited artist, which is
// the key an avatar lookup uses. It is empty when no credit carries an id.
func ytmFirstArtistID(artists []ytm.Artist) string {
	for _, a := range artists {
		if id := strings.TrimSpace(a.ID); id != "" {
			return id
		}
	}

	return ""
}

// ytmAlbum names the album of a song, when the catalogue listed one.
func ytmAlbum(album *ytm.Playlist) string {
	if album == nil {
		return ""
	}

	return strings.TrimSpace(album.Name)
}

// ytmThumb resolves the highest quality art URL, or "" when there is none.
func ytmThumb(t *ytm.ThumbnailProvider) string {
	if t == nil {
		return ""
	}

	return t.GetThumbnailURL(ytm.ThumbnailQualityHigh)
}

// firstNonEmpty returns the first non-blank value.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}
