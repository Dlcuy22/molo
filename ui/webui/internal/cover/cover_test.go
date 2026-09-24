package cover

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// encodePNG builds a solid PNG of the given size, optionally with a transparent
// top-left pixel so the alpha path can be exercised.
func encodePNG(t *testing.T, w, h int, transparent bool) []byte {
	t.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: 0x40, G: 0x80, B: 0xC0, A: 0xFF})
		}
	}
	if transparent {
		img.Set(0, 0, color.NRGBA{A: 0x00})
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}

	return buf.Bytes()
}

func TestIDIsEmptyForNoArtwork(t *testing.T) {
	if got := ID(nil); got != "" {
		t.Fatalf("ID(nil) = %q, want empty", got)
	}
	if got := ID([]byte{}); got != "" {
		t.Fatalf("ID(empty) = %q, want empty", got)
	}
}

func TestIDIsStableAndDistinct(t *testing.T) {
	a := []byte("one blob")
	b := []byte("another blob")
	if ID(a) != ID(a) {
		t.Fatal("ID is not stable for identical bytes")
	}
	if ID(a) == ID(b) {
		t.Fatal("ID collided for different bytes")
	}
	if ID(a) == "" {
		t.Fatal("ID of non-empty bytes is empty")
	}
}

func TestDataURLEmptyIsNoImage(t *testing.T) {
	got, err := DataURL(nil)
	if err != nil {
		t.Fatalf("DataURL(nil): %v", err)
	}
	if got != "" {
		t.Fatalf("DataURL(nil) = %q, want empty", got)
	}
}

func TestDataURLRejectsNonImage(t *testing.T) {
	_, err := DataURL([]byte("this is not an image"))
	if !errors.Is(err, ErrNotImage) {
		t.Fatalf("DataURL error = %v, want ErrNotImage", err)
	}
}

func TestDataURLScalesDownAndKeepsAspect(t *testing.T) {
	src := encodePNG(t, 640, 480, false)

	url, err := DataURL(src)
	if err != nil {
		t.Fatalf("DataURL: %v", err)
	}
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("prefix = %q, want a jpeg data URL", url[:min(len(url), 40)])
	}

	img := decodeDataURL(t, url)
	b := img.Bounds()
	if b.Dx() != 320 || b.Dy() != 240 {
		t.Fatalf("thumbnail = %dx%d, want 320x240", b.Dx(), b.Dy())
	}
}

func TestDataURLKeepsTransparencyAsPNG(t *testing.T) {
	src := encodePNG(t, 64, 64, true)

	url, err := DataURL(src)
	if err != nil {
		t.Fatalf("DataURL: %v", err)
	}
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("prefix = %q, want a png data URL for transparent art", url[:min(len(url), 40)])
	}
}

// TestDataURLKeepsPalettedTransparency guards the type the old type switch
// missed: a paletted PNG with a transparent index must not be flattened to a
// black JPEG.
func TestDataURLKeepsPalettedTransparency(t *testing.T) {
	pal := color.Palette{
		color.NRGBA{R: 0x40, G: 0x80, B: 0xC0, A: 0xFF},
		color.NRGBA{},
	}
	img := image.NewPaletted(image.Rect(0, 0, 32, 32), pal)
	for i := range img.Pix {
		img.Pix[i] = 0
	}
	img.SetColorIndex(0, 0, 1) // one transparent pixel

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode paletted fixture: %v", err)
	}

	url, err := DataURL(buf.Bytes())
	if err != nil {
		t.Fatalf("DataURL: %v", err)
	}
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("paletted transparency lost: prefix = %q", url[:min(len(url), 40)])
	}
}

func TestDataURLDoesNotUpscaleSmallArt(t *testing.T) {
	src := encodePNG(t, 32, 32, false)

	url, err := DataURL(src)
	if err != nil {
		t.Fatalf("DataURL: %v", err)
	}
	if got := decodeDataURL(t, url).Bounds().Dx(); got != 32 {
		t.Fatalf("small art was resized to %d, want 32", got)
	}
}

func TestThumbnailFitsWithinBox(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1000, 100))
	got := Thumbnail(img, 200).Bounds()
	if got.Dx() != 200 || got.Dy() != 20 {
		t.Fatalf("Thumbnail = %dx%d, want 200x20", got.Dx(), got.Dy())
	}
}

func decodeDataURL(t *testing.T, url string) image.Image {
	t.Helper()

	_, b64, ok := strings.Cut(url, ",")
	if !ok {
		t.Fatalf("data URL has no comma: %q", url)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode image: %v", err)
	}

	return img
}
