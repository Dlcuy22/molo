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
package main

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/meta"
	"github.com/dlcuy22/player/provider"
	"github.com/dlcuy22/player/ui/webui/internal/cover"
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

	// resolve and search are the network calls, indirected so a test can drive
	// the provider and the service without touching YouTube. In production they
	// are the client's own methods; the client itself is not kept, because
	// nothing else on it is used.
	resolve func(context.Context, string) (*ytm.Streams, error)
	search  func(context.Context, string, string, bool) (*ytm.SearchResults, error)
}

func newYTMProvider(index *ytmIndex) *ytmProvider {
	client := ytm.NewClient()

	return &ytmProvider{
		index:   index,
		http:    &http.Client{Timeout: ytmHTTPTimeout},
		resolve: client.GetStream,
		search:  client.Search,
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
	src.Opener = func(stop <-chan struct{}) (decode.Decoder, error) {
		// A signed URL is bound to this host and dies after a few hours, and
		// the streamer calls the opener again on the reopen-and-discard seek
		// fallback and on a decoder swap. Re-resolving on expiry is what keeps
		// a long session seeking correctly.
		if !expires.IsZero() && time.Now().After(expires) {
			fresh, err := p.resolve(ctx, id)
			if err != nil {
				return nil, err
			}
			f, ok := fresh.Best()
			if !ok {
				return nil, fmt.Errorf("ytm: no audio format for %s", id)
			}
			url = f.URL
			expires = f.ExpiresAt
		}

		data, err := p.fetch(ctx, stop, url, ytmMaxTrackBytes)
		if err != nil {
			return nil, err
		}

		return decode.NewWebMOpusFactory().OpenReader(bytes.NewReader(data))
	}

	return src, nil
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

// fetch reads one URL into memory with a ranged GET. The Range header is not an
// optimisation: googlevideo throttles a GET without one to roughly 32 KiB/s,
// measured at about eighty times slower on the same track.
//
// stop, when non-nil, cancels the request; the streamer closes it on shutdown
// so an opener cannot strand a producer goroutine in a blocked read. parent, if
// non-nil, is the session context, cancelled when the player closes.
func (p *ytmProvider) fetch(parent context.Context, stop <-chan struct{}, url string, limit int64) ([]byte, error) {
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
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("ytm: media request returned %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("ytm: media body exceeds %d bytes", limit)
	}

	return data, nil
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
	Playable   bool   `json:"playable"`
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
