package discordrpc

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/molo/ui/webui/internal/cover"
)

// jpegCover builds a small opaque PNG, which the encoder will turn into a JPEG
// thumbnail. The exact content does not matter, only that it decodes.
func jpegCover(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}

	return buf.Bytes()
}

// TestUploaderUploadsOncePerCoverHash pins the core promise: the same cover
// bytes are uploaded once, and the second ask reuses the link without calling
// upload again.
func TestUploaderUploadsOncePerCoverHash(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	raw := jpegCover(t)
	var uploads int
	u := &ImageUploader{
		urls:     make(map[string]cachedUpload),
		inflight: make(map[string]*uploadCall),
		upload: func([]byte) (string, error) {
			uploads++

			return "https://host/art.jpg", nil
		},
		alive: func(string) bool { return true },
	}

	first, err := u.GetImageURL(raw)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := u.GetImageURL(raw)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first != "https://host/art.jpg" || second != first {
		t.Fatalf("urls = %q/%q, want the same link", first, second)
	}
	if uploads != 1 {
		t.Fatalf("uploads = %d, want 1", uploads)
	}
}

// TestUploaderReuploadsWhenLinkDies pins the liveness rule: a cached link that
// no longer serves an image is dropped and the cover re-uploaded.
func TestUploaderReuploadsWhenLinkDies(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	raw := jpegCover(t)
	var uploads int
	alive := true
	u := &ImageUploader{
		urls:     make(map[string]cachedUpload),
		inflight: make(map[string]*uploadCall),
		upload: func([]byte) (string, error) {
			uploads++

			return "https://host/art.jpg", nil
		},
		alive: func(string) bool { return alive },
	}

	if _, err := u.GetImageURL(raw); err != nil {
		t.Fatalf("first: %v", err)
	}
	// The link stops answering, so the next ask must re-upload.
	alive = false
	if _, err := u.GetImageURL(raw); err != nil {
		t.Fatalf("second: %v", err)
	}

	if uploads != 2 {
		t.Fatalf("uploads = %d, want 2 (re-upload after a dead link)", uploads)
	}
}

// TestUploaderReuploadsWhenLinkExpires pins the TTL rule: even a live link is
// refreshed once it is older than the TTL.
func TestUploaderReuploadsWhenLinkExpires(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	raw := jpegCover(t)
	var uploads int
	u := &ImageUploader{
		urls:     make(map[string]cachedUpload),
		inflight: make(map[string]*uploadCall),
		upload: func([]byte) (string, error) {
			uploads++

			return "https://host/art.jpg", nil
		},
		alive: func(string) bool { return true },
	}

	if _, err := u.GetImageURL(raw); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Backdate the entry past the TTL.
	key := contentKey(raw)
	u.mu.Lock()
	u.urls[key] = cachedUpload{URL: u.urls[key].URL, SavedAt: time.Now().Add(-2 * urlTTL)}
	u.mu.Unlock()

	if _, err := u.GetImageURL(raw); err != nil {
		t.Fatalf("second: %v", err)
	}

	if uploads != 2 {
		t.Fatalf("uploads = %d, want 2 (refresh after TTL)", uploads)
	}
}

// TestUploaderCoalescesConcurrentAsks pins the single-flight rule: many asks for
// one cover at once cost one upload.
func TestUploaderCoalescesConcurrentAsks(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	raw := jpegCover(t)
	var mu sync.Mutex
	var uploads int
	release := make(chan struct{})
	u := &ImageUploader{
		urls:     make(map[string]cachedUpload),
		inflight: make(map[string]*uploadCall),
		upload: func([]byte) (string, error) {
			<-release
			mu.Lock()
			uploads++
			mu.Unlock()

			return "https://host/art.jpg", nil
		},
		alive: func(string) bool { return true },
	}

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := u.GetImageURL(raw); err != nil {
				t.Errorf("GetImageURL: %v", err)
			}
		}()
	}
	// Let the goroutines pile onto the in-flight call, then release the upload.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if uploads != 1 {
		t.Fatalf("uploads = %d, want 1 (concurrent asks coalesced)", uploads)
	}
}

// TestEncodeThumbBoundsTheUpload pins the size rule: a large cover is downscaled
// to the art edge before upload, so a multi-megabyte original never leaves the
// machine.
func TestEncodeThumbBoundsTheUpload(t *testing.T) {
	// A 1000x1000 opaque image.
	big := image.NewRGBA(image.Rect(0, 0, 1000, 1000))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 1000; x++ {
			big.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x40, A: 0xff})
		}
	}
	var src bytes.Buffer
	if err := png.Encode(&src, big); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}

	encoded, mime, err := cover.EncodeThumb(src.Bytes(), artEdge)
	if err != nil {
		t.Fatalf("EncodeThumb: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg for opaque art", mime)
	}
	img, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if b := img.Bounds(); b.Dx() > artEdge || b.Dy() > artEdge {
		t.Fatalf("bounds = %dx%d, want both <= %d", b.Dx(), b.Dy(), artEdge)
	}
}
