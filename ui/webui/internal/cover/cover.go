// Package cover turns the raw artwork bytes a tag carries into a small inline
// image a webview can put straight into an <img>. The engine hands the bytes
// over untouched; decoding, scaling and encoding are the UI's job because only
// the UI knows how large it will draw them.
package cover

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/jpeg"
	"image/png"
	"strconv"

	// The decoders register with image.Decode through their init, which is what
	// keeps this package format-agnostic: a new art format is one import here.
	_ "image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
)

// ErrNotImage means the bytes were not a decodable image. A tag can hold junk,
// so this is a normal outcome rather than a bug.
var ErrNotImage = errors.New("cover: not a decodable image")

// maxEdge bounds the longest side of a thumbnail. 320px covers the now-playing
// tile at 2x on the largest window this app opens, and keeps the inline data
// URL to a few tens of kilobytes.
const maxEdge = 320

// jpegQuality trades a little fidelity for a much smaller payload than PNG on
// photographic album art.
const jpegQuality = 82

// ID is a short stable identifier for one artwork blob. It is empty when there
// is no artwork, which lets a caller read "empty" as "nothing to fetch".
func ID(data []byte) string {
	if len(data) == 0 {
		return ""
	}

	h := fnv.New64a()
	_, _ = h.Write(data)

	return strconv.FormatUint(h.Sum64(), 16)
}

// DataURL decodes artwork and returns it as a data URL no larger than maxEdge
// on its longest side. It is empty-safe: no bytes means no image and no error,
// so a caller does not have to guard the common untagged case.
func DataURL(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotImage, err)
	}

	thumb := Thumbnail(img, maxEdge)

	var buf bytes.Buffer
	mime := "image/jpeg"
	// PNG only when transparency is real: it preserves alpha, but it is much
	// larger, and album art is opaque in practice.
	if hasTransparency(thumb) {
		mime = "image/png"
		err = png.Encode(&buf, thumb)
	} else {
		err = jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return "", err
	}

	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// Thumbnail scales img to fit inside a max x max box, preserving aspect ratio.
// An image already inside the box is returned unchanged, so a small cover is
// never resampled up into a blurry one.
func Thumbnail(img image.Image, max int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if max <= 0 || w <= 0 || h <= 0 || (w <= max && h <= max) {
		return img
	}

	nw, nh := max, h*max/w
	if h > w {
		nw, nh = w*max/h, max
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	// CatmullRom is the quality choice. Art is scaled once per track, so the
	// extra cost is invisible, and bilinear would look soft on a downscale.
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)

	return dst
}

// hasTransparency reports whether any pixel is not fully opaque. A PNG that
// happens to be opaque should still take the smaller JPEG path, so the pixels
// are checked rather than the source format. The Opaque fast path covers the
// standard image types (including a paletted PNG); only an unknown type falls
// back to scanning every pixel.
func hasTransparency(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}

	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				return true
			}
		}
	}

	return false
}
