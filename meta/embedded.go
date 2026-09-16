package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/dhowden/tag"
)

// ErrNoTags means the file parsed but carries no usable metadata. It is a
// deliberate step-aside, not a hard failure: a tagless file should fall down
// to the filename resolver instead of surfacing an empty title.
var ErrNoTags = errors.New("meta: no embedded tags")

// EmbeddedTags reads the container's own metadata through dhowden/tag, which
// covers ID3 (MP3), Vorbis comments (Ogg/Opus/FLAC), MP4 atoms and DSF. One
// library covers every format we decode today.
type EmbeddedTags struct{}

// NewEmbeddedTags returns the content-aware resolver.
func NewEmbeddedTags() *EmbeddedTags { return &EmbeddedTags{} }

func (e *EmbeddedTags) Name() string { return "embedded-tags" }

// Priority puts container metadata above a filename guess. It stays below a
// future FFprobe resolver only because probing is a heavier, more certain
// source; when that lands its priority goes above this one.
func (e *EmbeddedTags) Priority() int { return 100 }

// supportedExts is the set of extensions dhowden/tag can parse. Matching on
// the extension keeps this resolver from opening files it cannot read, which
// is the whole point of the Match contract.
var supportedExts = map[string]struct{}{
	".mp3":  {},
	".m4a":  {},
	".m4b":  {},
	".m4p":  {},
	".flac": {},
	".ogg":  {},
	".oga":  {},
	".opus": {},
	".dsf":  {},
}

func (e *EmbeddedTags) Match(path string) bool {
	_, ok := supportedExts[strings.ToLower(filepath.Ext(path))]

	return ok
}

// Resolve reads the tags and cover art. A parser error and a file with no tags
// both return an error, so the chain falls through either way.
func (e *EmbeddedTags) Resolve(ctx context.Context, path string) (*Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m, err := tag.ReadFrom(f)
	if err != nil {
		return nil, err
	}

	track, _ := m.Track()
	disc, _ := m.Disc()
	tags := Tags{
		Title:       m.Title(),
		Artist:      m.Artist(),
		Album:       m.Album(),
		AlbumArtist: m.AlbumArtist(),
		Track:       track,
		Disc:        disc,
		Year:        m.Year(),
	}
	if pic := m.Picture(); pic != nil {
		tags.Cover = pic.Data
		tags.CoverMIME = pic.MIMEType
	}
	if !hasTagContent(tags) {
		return nil, ErrNoTags
	}

	return &Meta{
		Path:      path,
		Container: strings.ToLower(string(m.FileType())),
		Codec:     codecForExt(filepath.Ext(path)),
		Source:    e.Name(),
		Tags:      tags,
	}, nil
}

// hasTagContent reports whether anything a UI could display was found. A
// container with a header but no fields must not win the chain, or the title
// the filename resolver could have provided is lost.
func hasTagContent(t Tags) bool {
	return t.Title != "" || t.Artist != "" || t.Album != "" || t.AlbumArtist != "" ||
		t.Track != 0 || t.Disc != 0 || t.Year != 0 || len(t.Cover) > 0
}
