// Package decode turns a file path into interleaved float32 PCM frames.
//
// A codec is added by writing one file that implements Factory (and optionally
// Profile, Seeker, Prober, ReaderOpener) plus an init that calls Register. The
// dispatch logic in this file is deliberately codec-agnostic.
package decode

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dlcuy22/molo/core"
)

// ErrUnsupported means no registered factory recognised the file, either by
// extension or by content sniffing.
var ErrUnsupported = errors.New("decode: unsupported format")

// ErrNotSeekable means the decoder cannot reposition on this source. A decoder
// returns it from SeekFrame for a forward-only stream (a network body or a
// pipe), so the streamer can fall back to reopening and discarding instead of
// failing the track.
var ErrNotSeekable = errors.New("decode: source is not seekable")

// ErrUnknownCodec means OpenNamed was given a name no factory registered.
var ErrUnknownCodec = errors.New("decode: unknown codec")

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

// Profile is implemented by factories that describe themselves for a UI.
// Both values are optional: a factory without it still works and is listed by
// Name, with an empty label and weight 0.
type Profile interface {
	FriendlyName() string // "Portable"
	Weight() int          // higher wins the automatic default
}

// ProfileOf reports a factory's profile, or zero values when it has none.
func ProfileOf(f Factory) (friendly string, weight int) {
	p, ok := f.(Profile)
	if !ok {
		return "", 0
	}

	return p.FriendlyName(), p.Weight()
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

// Codec describes one registered factory for a UI that has to offer a choice.
type Codec struct {
	Name         string // "opus"  <- configuration key
	FriendlyName string // "Portable"   <- UI label
	Weight       int    // 90
	Exts         []string

	// Default is true when this codec wins automatic selection for at least
	// one of the extensions it claims. Selection is per-path, so more than one
	// codec can be a default at once: a codec can win one extension and lose
	// another.
	Default bool
}

// Registry dispatches paths to the factories registered at init time.
type Registry interface {
	Register(f Factory)
	Open(path string) (Decoder, error)

	// OpenNamed opens a path through the named factory, bypassing weight
	// entirely. An empty name means automatic selection, identical to Open.
	OpenNamed(name, path string) (Decoder, error)

	Probe(path string) (Prober, bool)
	Supported() []string

	// Codecs lists the registered factories for a UI, sorted by weight
	// descending and then by registration order.
	Codecs() []Codec
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

// Register appends a factory. Automatic selection picks the highest weight
// among the factories that match a path; equal weights fall back to the last
// registration, which lets a host override a built-in codec. A factory without
// a Profile has weight 0, so any positive weight beats it. Weight only applies
// to automatic selection: OpenNamed opens the named factory directly and
// ignores it.
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
	return r.OpenNamed("", path)
}

// OpenNamed opens a path through a named factory. An empty name means automatic
// selection, identical to Open. A known name opens that factory directly and
// never consults weight, which is what lets a caller force the native codec
// over the higher-weighted pure-Go one. An unknown name fails before the file
// is touched and lists the registered names.
func (r *registry) OpenNamed(name, path string) (Decoder, error) {
	if name == "" {
		f, err := r.factory(path)
		if err != nil {
			return nil, err
		}

		return f.Open(path)
	}

	f := r.byName(name)
	if f == nil {
		return nil, fmt.Errorf("%w: %q (available: %s)", ErrUnknownCodec, name, strings.Join(r.names(), ", "))
	}

	return f.Open(path)
}

// byName returns the last factory registered under name, so a host that
// re-registers a name overrides the built-in one for explicit selection too.
func (r *registry) byName(name string) Factory {
	var found Factory
	for _, f := range r.factories {
		if f.Name() == name {
			found = f
		}
	}

	return found
}

// names lists the registered factory names, sorted for a stable error message.
func (r *registry) names() []string {
	names := make([]string, 0, len(r.factories))
	for _, f := range r.factories {
		names = append(names, f.Name())
	}
	slices.Sort(names)

	return names
}

// Codecs describes the registered factories for a UI. The order is weight
// descending, with registration order breaking a tie, so the list is stable
// and matches automatic selection.
func (r *registry) Codecs() []Codec {
	codecs := make([]Codec, 0, len(r.factories))
	for i, f := range r.factories {
		friendly, weight := ProfileOf(f)
		codecs = append(codecs, Codec{
			Name:         f.Name(),
			FriendlyName: friendly,
			Weight:       weight,
			Exts:         slices.Clone(f.Exts()),
			Default:      r.isExtensionWinner(i),
		})
	}
	slices.SortStableFunc(codecs, func(a, b Codec) int { return cmp.Compare(b.Weight, a.Weight) })

	return codecs
}

// isExtensionWinner reports whether the factory at index i wins automatic
// selection for at least one of the extensions it claims. It is computed from
// extensions alone: the per-file magic check needs a file and cannot be
// answered here.
func (r *registry) isExtensionWinner(i int) bool {
	for _, ext := range r.factories[i].Exts() {
		if r.extensionWinner(ext) == i {
			return true
		}
	}

	return false
}

// extensionWinner returns the index of the factory automatic selection would
// try for ext, or -1. Selection is highest weight, with the last registration
// breaking a tie. Indices are compared instead of factories so an
// uncomparable factory value cannot panic.
func (r *registry) extensionWinner(ext string) int {
	ext = normalizeExt(ext)

	winner := -1
	winnerWeight := 0
	for i, f := range r.factories {
		if !slices.ContainsFunc(f.Exts(), func(e string) bool { return normalizeExt(e) == ext }) {
			continue
		}
		_, weight := ProfileOf(f)
		if winner < 0 || weight >= winnerWeight {
			winner = i
			winnerWeight = weight
		}
	}

	return winner
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

// byExtension picks the factory automatic selection would try for the path's
// extension, but rejects it when the file magic belongs to a different factory.
// Such a file falls through to the magic lookup instead.
func (r *registry) byExtension(path string, header []byte) Factory {
	ext := normalizeExt(filepath.Ext(path))

	i := r.extensionWinner(ext)
	if i < 0 {
		return nil
	}
	candidate := r.factories[i]

	// Only a factory that positively claims the bytes can contradict the
	// extension; an unreadable or empty header stays with the extension.
	claimed := r.byMagic(header)
	if claimed != nil && claimed != candidate {
		return nil
	}

	return candidate
}

// byMagic picks the factory automatic selection would try among those whose
// Match accepts the header: highest weight, with the last registration
// breaking a tie.
func (r *registry) byMagic(header []byte) Factory {
	var winner Factory
	winnerWeight := 0
	for _, f := range r.factories {
		if !f.Match(header) {
			continue
		}
		_, weight := ProfileOf(f)
		if winner == nil || weight >= winnerWeight {
			winner = f
			winnerWeight = weight
		}
	}

	return winner
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
