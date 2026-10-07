// Package discordrpc renders the player's now-playing state into a Discord
// Rich Presence activity over the local Discord IPC socket.
//
// It is a desktop integration, not the account-level presence the Android
// client uses: Discord's IPC SET_ACTIVITY command needs no OAuth, only a
// registered application id and a running Discord client. Artwork travels as
// either a public https URL Discord fetches itself or a registered asset key;
// raw bytes are uploaded to a temporary host first, because the IPC command
// takes a string. The card header reads "Listening to <artist>" because the
// activity carries the artist as its name, the same field the reference client
// sets.
//
// Key components:
//   - Manager: owns the IPC connection and the periodic re-sync
//   - TrackInfo: the snapshot of playback the UI pushes
//   - activity: the card, mapped to the wire shape in activity.go
//   - ipcClient: the handshake and SET_ACTIVITY transport in ipc.go
//   - ImageUploader: turns raw bytes into an https URL Discord will render
package discordrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dlcuy22/molo/ui/webui/internal/artcache"
	"github.com/dlcuy22/molo/ui/webui/internal/cover"
)

// uploadAPI is the temporary host raw artwork is posted to. Discord refuses a
// data URL, so bytes have to become a public link; the host serves the file
// with an image content type, which is what makes Discord render it.
const uploadAPI = "https://tmpfiles.org/api/v1/upload"

// uploadTimeout bounds one upload and its scrape, so a hung host cannot pin the
// sync goroutine.
const uploadTimeout = 20 * time.Second

// checkTimeout bounds the liveness probe of a cached URL. It is shorter than an
// upload because a slow answer is treated as "not alive" and the art is simply
// re-uploaded.
const checkTimeout = 8 * time.Second

// urlTTL bounds how long a cached upload URL is trusted. The temporary host
// expires links, and Discord silently refuses a dead one, so a stale entry is
// dropped and the art re-uploaded rather than shown broken forever.
const urlTTL = 24 * time.Hour

// artEdge bounds the artwork side sent to Discord. The presence draws the cover
// a few dozen pixels wide, so 200px is already generous at any display scale
// and keeps the upload to tens of kilobytes instead of the multi-megabyte
// original.
const artEdge = 200

type apiResponse struct {
	Status string `json:"status"`
	Data   struct {
		URL string `json:"url"`
	} `json:"data"`
}

// cachedUpload is one remembered upload: the public URL and when it was
// resolved, so an expired or dead link can be discarded.
type cachedUpload struct {
	URL     string    `json:"url"`
	SavedAt time.Time `json:"savedAt"`
}

// ImageUploader turns raw artwork into an https URL Discord renders. The cache
// is keyed by a hash of the original cover bytes, so the same album cover
// across many tracks is uploaded once, and the manager's periodic re-send never
// re-uploads anything. The cache is consulted before the network, and a cached
// link is reused only while it still answers as an image.
type ImageUploader struct {
	client *http.Client
	log    *slog.Logger

	// upload posts a resized cover and returns its link; alive reports whether a
	// cached link still serves an image. They are fields so a test can drive the
	// cache without the network; production sets them in NewImageUploader.
	upload func(raw []byte) (string, error)
	alive  func(url string) bool

	mu        sync.Mutex
	urls      map[string]cachedUpload // cover hash -> resolved URL
	inflight  map[string]*uploadCall  // cover hash -> in-progress upload
	cachePath string
}

// uploadCall lets concurrent asks for one cover share a single upload.
type uploadCall struct {
	done chan struct{}
	url  string
	err  error
}

func NewImageUploader(log *slog.Logger) *ImageUploader {
	if log == nil {
		log = slog.Default()
	}
	u := &ImageUploader{
		client:   &http.Client{Timeout: uploadTimeout},
		log:      log,
		urls:     make(map[string]cachedUpload),
		inflight: make(map[string]*uploadCall),
	}
	u.upload = u.uploadCover
	u.alive = u.urlAlive
	if dir, err := artcache.Dir(); err == nil {
		u.cachePath = filepath.Join(dir, "uploads.json")
		u.load()
	}

	return u
}

// GetImageURL returns an https URL for the artwork bytes, uploading a resized
// copy the first time. The key is a hash of the original bytes, so identical
// covers map to one upload. A cached URL is reused while it is unexpired and
// still answers as an image; otherwise the art is re-uploaded.
func (u *ImageUploader) GetImageURL(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	key := contentKey(raw)

	if url, ok := u.cachedAlive(key); ok {
		return url, nil
	}

	// One upload per cover even under concurrent asks: the first caller does
	// the work, the rest wait for its result.
	u.mu.Lock()
	if call, ok := u.inflight[key]; ok {
		u.mu.Unlock()
		<-call.done

		return call.url, call.err
	}
	call := &uploadCall{done: make(chan struct{})}
	u.inflight[key] = call
	u.mu.Unlock()

	url, err := u.upload(raw)

	u.mu.Lock()
	call.url, call.err = url, err
	delete(u.inflight, key)
	if err == nil && url != "" {
		u.urls[key] = cachedUpload{URL: url, SavedAt: time.Now()}
	}
	snapshot := u.snapshotLocked()
	u.mu.Unlock()
	close(call.done)

	if err == nil && url != "" {
		u.persist(snapshot)
	}

	return url, err
}

// cachedAlive returns a cached URL when it is unexpired and still serves an
// image. A dead or expired link returns false so the caller re-uploads.
func (u *ImageUploader) cachedAlive(key string) (string, bool) {
	u.mu.Lock()
	entry, ok := u.urls[key]
	u.mu.Unlock()
	if !ok || time.Since(entry.SavedAt) >= urlTTL {
		return "", false
	}
	if !u.alive(entry.URL) {
		u.mu.Lock()
		delete(u.urls, key)
		u.mu.Unlock()

		return "", false
	}

	return entry.URL, true
}

// uploadCover resizes the artwork and posts it, returning the direct link.
func (u *ImageUploader) uploadCover(raw []byte) (string, error) {
	thumb, mime, err := cover.EncodeThumb(raw, artEdge)
	if err != nil {
		return "", err
	}
	if len(thumb) == 0 {
		return "", nil
	}

	return u.UploadImage(thumb, mime)
}

// urlAlive probes a cached URL: a 2xx answer whose content type is an image
// means Discord can still fetch it. Any other outcome, including a network
// error, reads as "not alive", so the cover is re-uploaded rather than shown
// broken.
func (u *ImageUploader) urlAlive(url string) bool {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "molo/1.0")

	client := &http.Client{Timeout: checkTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Drain a little so the connection can be reused, but do not read a body we
	// are throwing away.
	_, _ = io.CopyN(io.Discard, resp.Body, 512)

	return resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		strings.HasPrefix(resp.Header.Get("Content-Type"), "image/")
}

// UploadImage posts the encoded bytes and resolves the direct download link.
func (u *ImageUploader) UploadImage(raw []byte, mime string) (string, error) {
	ext := "jpg"
	if strings.Contains(mime, "png") {
		ext = "png"
	}
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "art."+ext)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err = part.Write(raw); err != nil {
		return "", fmt.Errorf("write image bytes: %w", err)
	}
	writer.Close()

	req, err := http.NewRequest(http.MethodPost, uploadAPI, body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload request: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var parsed apiResponse
	if err = json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if parsed.Data.URL == "" {
		return "", fmt.Errorf("upload API returned no url (status=%q)", parsed.Status)
	}

	// tmpfiles.org serves the raw file only at a timestamped download link;
	// the page URL returns HTML, which Discord will not render. Scrape the real
	// href, and fall back to the legacy /dl/ rewrite if the page changes.
	if url, err := u.scrapeDownloadURL(parsed.Data.URL); err == nil {
		return url, nil
	}

	url := strings.Replace(parsed.Data.URL, "org/", "org/dl/", 1)

	return strings.Replace(url, "http://", "https://", 1), nil
}

var dlHrefRe = regexp.MustCompile(`href="([^"]*dl/[^"]+)"`)

// scrapeDownloadURL fetches the temporary file page and extracts the direct
// download link it embeds.
func (u *ImageUploader) scrapeDownloadURL(pageURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return "", fmt.Errorf("build scrape request: %w", err)
	}
	req.Header.Set("User-Agent", "molo/1.0")

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("scrape page request: %w", err)
	}
	defer resp.Body.Close()

	html, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read page: %w", err)
	}

	m := dlHrefRe.FindSubmatch(html)
	if m == nil {
		return "", fmt.Errorf("no download link found in page")
	}

	dl := string(m[1])
	if !strings.HasPrefix(dl, "http") {
		dl = "https://tmpfiles.org" + dl
	}

	return strings.Replace(dl, "http://", "https://", 1), nil
}

// load reads the persisted hash-to-url map. A missing or corrupt file is
// ignored: the cache only saves uploads, so starting empty is harmless.
func (u *ImageUploader) load() {
	if u.cachePath == "" {
		return
	}
	raw, err := os.ReadFile(u.cachePath)
	if err != nil {
		return
	}
	var m map[string]cachedUpload
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	u.urls = m
}

// snapshotLocked copies the map for a write outside the lock. The caller holds
// u.mu.
func (u *ImageUploader) snapshotLocked() map[string]cachedUpload {
	out := make(map[string]cachedUpload, len(u.urls))
	for k, v := range u.urls {
		out[k] = v
	}

	return out
}

// persist writes the cache. It is best effort: a failure only means an upload
// is repeated next run.
func (u *ImageUploader) persist(m map[string]cachedUpload) {
	if u.cachePath == "" {
		return
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := os.WriteFile(u.cachePath, raw, 0o644); err != nil {
		u.log.Debug("artcache: could not persist upload map", "error", err)
	}
}

// contentKey is a stable id for one blob of original cover bytes, so the same
// artwork maps to one cached URL and one upload.
func contentKey(raw []byte) string {
	return artcache.Key(raw)
}
