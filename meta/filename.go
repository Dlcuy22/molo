package meta

import (
	"context"
	"path/filepath"
	"strings"
)

// Filename is the last-resort resolver: it always matches and turns the base
// name into a title. It exists so a UI never has to show an empty line, and it
// guarantees the chain can answer for a file no other resolver understands.
type Filename struct{}

// NewFilename returns the fallback resolver.
func NewFilename() *Filename { return &Filename{} }

func (f *Filename) Name() string { return "filename" }

// Priority puts the filename below every content-aware resolver. Content wins
// when it is available; the name is a guess and should never outrank it.
func (f *Filename) Priority() int { return 0 }

// Match always reports true. Requiring the file to exist here would make a
// queued but not-yet-created path fail to describe itself, which is not this
// resolver's job to police: the decoder reports a missing file.
func (f *Filename) Match(string) bool { return true }

// Resolve derives a title from the base name without the extension and a
// container hint from the extension. It never opens the file.
func (f *Filename) Resolve(ctx context.Context, path string) (*Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	base := filepath.Base(path)
	title := strings.TrimSuffix(base, filepath.Ext(base))

	return &Meta{
		Path:      path,
		Container: containerForExt(filepath.Ext(path)),
		Source:    f.Name(),
		Tags:      Tags{Title: title},
	}, nil
}

// containerForExt maps a file extension to the container name used across the
// package, so the fallback and the embedded resolver agree on one spelling.
func containerForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".opus", ".ogg", ".oga":
		return "ogg"
	case ".flac":
		return "flac"
	case ".mp3":
		return "mp3"
	case ".m4a", ".m4b", ".m4p":
		return "mp4"
	case ".dsf":
		return "dsf"
	default:
		return ""
	}
}

// codecForExt is the codec guess that belongs with containerForExt. Only the
// extensions we actually decode are mapped; an unknown one returns empty
// rather than a wrong label.
func codecForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".opus":
		return "opus"
	case ".flac":
		return "flac"
	case ".mp3":
		return "mp3"
	case ".m4a", ".m4b", ".m4p":
		return "aac"
	case ".dsf":
		return "dsd"
	default:
		return ""
	}
}
