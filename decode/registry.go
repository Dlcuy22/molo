// Package decode turns a file path into interleaved float32 PCM frames.
//
// A codec is added by writing one file that implements Factory (and optionally
// Seeker, Prober, ReaderOpener) plus an init that calls Register. The dispatch
// logic in this file is deliberately codec-agnostic.
package decode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dlcuy22/player/core"
)

// ErrUnsupported means no registered factory recognised the file, either by
// extension or by content sniffing.
var ErrUnsupported = errors.New("decode: unsupported format")

// ErrClosed is returned by a decoder that is used after Close. Callers treat
// it as a programming error rather than end of stream.
var ErrClosed = errors.New("decode: decoder is closed")

// ErrEndOfStream is the sentinel a decoder returns once the last frame has been
// delivered. It is an alias of io.EOF so callers can use either spelling.
var ErrEndOfStream = io.EOF

// sniffWindow bounds how much of a file is read to identify it. Every
// container we care about is identifiable from its first bytes, so this stays
// far below a whole-file read.
const sniffWindow = 4096

// Decoder is the streaming contract every codec satisfies. Implementations
// always emit interleaved float32 in the format reported by Info, regardless
// of the source layout.
type Decoder interface {
	Info() core.StreamInfo

	// ReadFrames fills dst with whole interleaved frames and returns how many
	// it wrote. dst is consumed left to right; a short read is not an error.
	ReadFrames(dst []float32) (frames int, err error)

	Close() error
}

// Factory is the registration unit for a codec. Match is a cheap content check
// used to resolve extension conflicts, so it must not open or seek the file.
type Factory interface {
	Name() string
	Exts() []string
	Match(magic []byte) bool
	Open(path string) (Decoder, error)
}

// ReaderOpener is implemented by pure-Go decoders that can work from any
// stream. Path-based codecs (FFI wrappers, for example) simply omit it.
type ReaderOpener interface {
	OpenReader(r io.Reader) (Decoder, error)
}

// Seeker is implemented by decoders that can jump natively. Decoders without
// it rely on the streamer's reopen-and-discard fallback.
type Seeker interface {
	SeekFrame(frame int64) error
}

// Descriptor is implemented by decoders that can name the pieces they are
// built from, for diagnostics. Both labels are free-form and for display.
type Descriptor interface {
	DecoderName() string
	ParserName() string
}

// Describe returns the decoder and parser labels when d implements
// Descriptor, and empty strings otherwise. Test fakes and future codecs are
// free to omit it.
func Describe(d Decoder) (decoder, parser string) {
	desc, ok := d.(Descriptor)
	if !ok {
		return "", ""
	}

	return desc.DecoderName(), desc.ParserName()
}

// ProbeOptions tells a Prober how much work it may do. DurationUnknown must
// stay cheap and may leave TotalFrames at -1.
type ProbeOptions struct {
	Duration core.DurationMode
}

// Prober reports stream metadata without producing PCM. It is separate from
// Factory and Decoder because a probe must not have to decode audio.
type Prober interface {
	Probe(path string, opts ProbeOptions) (core.StreamInfo, error)
}

// Registry dispatches paths to the factories registered at init time.
type Registry interface {
	Register(f Factory)
	Open(path string) (Decoder, error)
	Probe(path string) (Prober, bool)
	Supported() []string
}

// registry is the only implementation of Registry. Factories are kept in
// registration order for deterministic conflict resolution.
type registry struct {
	factories []Factory
}

// NewRegistry returns an empty registry. Codec packages register into the
// package-level Default instead; this exists for tests and for embedders that
// want an isolated set of decoders.
func NewRegistry() Registry {
	return &registry{}
}

// Default is the registry populated by the init functions in this package.
var Default = NewRegistry()

// Register adds a factory to the process-wide Default registry.
func Register(f Factory) {
	Default.Register(f)
}

// Register appends a factory. Later registrations win for overlapping
// extensions and magics, which lets a host override a built-in codec.
func (r *registry) Register(f Factory) {
	r.factories = append(r.factories, f)
}

// Supported lists the extensions this process can decode, lowercased, sorted,
// and free of duplicates.
func (r *registry) Supported() []string {
	seen := make(map[string]struct{}, len(r.factories))
	for _, f := range r.factories {
		for _, ext := range f.Exts() {
			seen[normalizeExt(ext)] = struct{}{}
		}
	}

	exts := make([]string, 0, len(seen))
	for ext := range seen {
		exts = append(exts, ext)
	}
	slices.Sort(exts)

	return exts
}

// Open resolves a path to a factory by extension first, then by sniffing the
// file header. An extension match is only trusted when the magic does not
// contradict it, so a mislabelled file is never handed to the wrong decoder.
func (r *registry) Open(path string) (Decoder, error) {
	f, err := r.factory(path)
	if err != nil {
		return nil, err
	}

	return f.Open(path)
}

// Probe returns the Prober for a path when the resolved factory implements
// one. The bool is false rather than an error because "cannot probe" is a
// normal capability gap, not a failure.
func (r *registry) Probe(path string) (Prober, bool) {
	f, err := r.factory(path)
	if err != nil {
		return nil, false
	}
	p, ok := f.(Prober)

	return p, ok
}

func (r *registry) factory(path string) (Factory, error) {
	header, err := readHeader(path)
	if err != nil {
		return nil, err
	}

	if f := r.byExtension(path, header); f != nil {
		return f, nil
	}
	if f := r.byMagic(header); f != nil {
		return f, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrUnsupported, path)
}

// byExtension prefers the last factory registered for the path's extension,
// but rejects it when the file magic belongs to a different factory. Such a
// file falls through to the magic lookup instead.
func (r *registry) byExtension(path string, header []byte) Factory {
	ext := normalizeExt(filepath.Ext(path))

	var candidate Factory
	for _, f := range r.factories {
		if slices.ContainsFunc(f.Exts(), func(e string) bool { return normalizeExt(e) == ext }) {
			candidate = f
		}
	}
	if candidate == nil {
		return nil
	}

	// Only a factory that positively claims the bytes can contradict the
	// extension; an unreadable or empty header stays with the extension.
	claimed := r.byMagic(header)
	if claimed != nil && claimed != candidate {
		return nil
	}

	return candidate
}

// byMagic returns the last registered factory whose Match accepts the header.
func (r *registry) byMagic(header []byte) Factory {
	var found Factory
	for _, f := range r.factories {
		if f.Match(header) {
			found = f
		}
	}

	return found
}

// readHeader loads a bounded prefix used for sniffing. A file shorter than the
// window is returned in full.
func readHeader(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, sniffWindow)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}

	return bytes.Clone(buf[:n]), nil
}

func normalizeExt(ext string) string {
	return strings.ToLower(ext)
}
