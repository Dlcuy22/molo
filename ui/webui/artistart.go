// Artist avatars for the Discord presence's small image.
//
// YouTube Music's search and watch responses name the credited artists but do
// not carry their images: the avatar lives behind the artist's browse page. So
// this is a second, deliberate lookup, done once per artist and cached twice:
// in memory for the session, and on disk so a later run does not spend the
// request again. A miss is a normal outcome and reads as "no avatar".
//
// The cached value is the resolved https URL, not the image bytes. Discord
// fetches an https URL itself, so the URL is the only thing the presence needs;
// the disk entry is what stops the browse call from being repeated.
package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/dlcuy22/molo/ui/webui/internal/artcache"
	"github.com/dlcuy22/molo/ui/webui/internal/cover"
)

// artistCacheKey namespaces the disk entries, so an artist id can never collide
// with a content hash from another cache user.
func artistCacheKey(artistID string) string { return "artist:" + artistID }

// ytmArtistIndex caches the resolved avatar URL per artist browse id. A present
// entry with an empty URL means the artist was looked up and has none, which is
// cached too so a coverless artist is not re-queried on every track.
type ytmArtistIndex struct {
	mu   sync.Mutex
	urls map[string]string
}

func newYTMArtistIndex() *ytmArtistIndex {
	return &ytmArtistIndex{urls: make(map[string]string)}
}

func (a *ytmArtistIndex) lookup(id string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	url, ok := a.urls[id]

	return url, ok
}

func (a *ytmArtistIndex) put(id, url string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.urls[id] = url
}

// artistAvatarDataURL resolves the artist's avatar and returns it as an inline
// data URL, or "" when there is none. It is the settings preview's source: the
// webview cannot be relied on to load the remote image directly, and a data URL
// keeps the preview off the network after the first fetch. The decoded form is
// cached in memory, separate from the URL cache, because a preview may redraw
// often.
func (p *ytmProvider) artistAvatarDataURL(ctx context.Context, artistID string) string {
	if artistID == "" {
		return ""
	}

	p.avatarMu.Lock()
	if url, ok := p.avatarData[artistID]; ok {
		p.avatarMu.Unlock()

		return url
	}
	p.avatarMu.Unlock()

	url := p.artistArt(ctx, artistID)
	if url == "" {
		return ""
	}
	data := p.thumbFor(ctx, url)
	if len(data) == 0 {
		return ""
	}
	dataURL, err := cover.DataURL(data)
	if err != nil {
		return ""
	}

	p.avatarMu.Lock()
	p.avatarData[artistID] = dataURL
	p.avatarMu.Unlock()

	return dataURL
}

// logger returns the provider's logger, falling back to the default so a
// provider built directly in a test cannot nil-panic on a log line.
func (p *ytmProvider) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}

	return slog.Default()
}

// artistArt resolves an artist's avatar URL. It reads the in-memory index, then
// the disk cache, then the browse endpoint, caching the outcome at every level.
// It never returns an error: an avatar is decorative, so a failure is "no
// avatar" rather than a reason to disturb the track.
func (p *ytmProvider) artistArt(ctx context.Context, artistID string) string {
	if artistID == "" || p.loadArtist == nil {
		return ""
	}
	if url, ok := p.artists.lookup(artistID); ok {
		return url
	}

	// The disk cache is consulted before the network, so a restart reuses the
	// avatar a previous run resolved.
	if data := artcache.Load(artistCacheKey(artistID)); len(data) > 0 {
		url := string(data)
		p.artists.put(artistID, url)

		return url
	}

	artist, err := p.loadArtist(ctx, artistID)
	if err != nil || artist == nil {
		p.artists.put(artistID, "")

		return ""
	}
	url := ytmThumb(artist.Thumbnail)
	p.artists.put(artistID, url)
	if url != "" {
		if err := artcache.Store(artistCacheKey(artistID), []byte(url)); err != nil {
			p.logger().Warn("artist art: cache write failed", "artist", artistID, "error", err)
		}
	}

	return url
}
