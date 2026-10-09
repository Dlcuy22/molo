// Network as an audio source: direct http/https file streaming.
//
// This is the built-in provider for a URL that points straight at an audio
// file. It is the network counterpart of LocalAudio and the file counterpart of
// the YouTube Music provider: the reference is a URL, the bytes arrive over
// HTTP, and the container is decoded by the same decode registry every local
// file uses.
//
// Bytes are read through ranged GETs (httpreader.go), not fetched whole. That
// keeps the source seekable, so the decoder's own container index works
// unchanged and a seek costs a Range request rather than a re-download. Only
// factories that implement decode.ReaderOpener can serve a stream; today those
// are the Ogg Opus and WebM/Opus readers. A URL holding anything else fails at
// open with a clear error instead of being misread.
//
// The provider holds no library knowledge of its own: it does not import a
// container parser or a codec. The URL stops here; what crosses into the engine
// is a decoder over the bytes and a description of the track.

package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"github.com/dlcuy22/molo/meta"
)

// NetworkProvider resolves http and https references into a playable Source. It
// implements AudioProvider.
type NetworkProvider struct {
	client *http.Client
}

// NewNetworkProvider returns a provider that streams audio over HTTP. A nil
// client selects http.DefaultClient, which has no timeout; a caller that wants
// one should pass its own, because a media transfer is legitimately long-lived
// and a fixed whole-request timeout would cut it off.
func NewNetworkProvider(client *http.Client) *NetworkProvider {
	return &NetworkProvider{client: client}
}

// Name identifies the provider. It is the label a UI shows and the key a log
// line carries.
func (p *NetworkProvider) Name() string { return "Network" }

// Match reports whether ref is an http or https URL. It is the cheap prefix
// check the session runs for every queued reference; it never opens the URL, so
// a wrong or dead link is not discovered until Open.
func (p *NetworkProvider) Match(ref string) bool {
	return hasHTTPPrefix(ref)
}

func hasHTTPPrefix(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// Open resolves a URL into a Source. The URL is validated here, on the session's
// build worker, but no bytes are fetched: the first request happens when the
// streamer calls the Opener, so Open stays cheap and the decoder open is the one
// place a network failure surfaces.
//
// The provider cannot know the stream shape until a decoder opens it, because
// the container is sniffed from the bytes. It therefore captures the shape from
// the first decoder it builds and reports it through Probe, so the duration is
// known without a second index walk.
func (p *NetworkProvider) Open(ctx context.Context, ref string) (Source, error) {
	if err := ctx.Err(); err != nil {
		return Source{}, err
	}
	u, err := url.Parse(ref)
	if err != nil {
		return Source{}, fmt.Errorf("network: parse %q: %w", ref, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Source{}, fmt.Errorf("network: unsupported scheme %q", u.Scheme)
	}

	client := p.client
	if client == nil {
		client = http.DefaultClient
	}

	// The shape is learned from the decoder, so it is recorded on the first
	// open and replayed by Probe. The zero value reports "unknown", which is
	// the honest answer if Probe is ever called before an Opener.
	var (
		shapeMu   sync.Mutex
		shape     core.StreamInfo
		haveShape bool
	)
	record := func(d decode.Decoder) {
		shapeMu.Lock()
		if !haveShape {
			shape = d.Info()
			haveShape = true
		}
		shapeMu.Unlock()
	}

	src := Source{
		Meta: networkMeta(ref, u),
	}
	src.Opener = func(stop <-chan struct{}) (decode.Decoder, error) {
		// A per-decoder context so the ranged requests stop when the decoder is
		// closed or the streamer shuts this opener's stop channel. It is derived
		// from the session context, so a player Close cancels them too.
		octx, cancel := context.WithCancel(ctx)
		if stop != nil {
			go func() {
				select {
				case <-stop:
					cancel()
				case <-octx.Done():
				}
			}()
		}
		rr := newHTTPRangeReader(octx, client, ref)
		d, err := decode.Default.OpenReader(rr)
		if err != nil {
			cancel()

			return nil, err
		}
		record(d)

		return &networkDecoder{Decoder: d, cancel: cancel}, nil
	}
	src.Probe = func(core.DurationMode) (core.StreamInfo, error) {
		shapeMu.Lock()
		defer shapeMu.Unlock()
		if haveShape {
			return shape, nil
		}

		// The probe ran before any Opener, which the session never does, but
		// reporting unknown is better than guessing.
		return core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: -1}, nil
	}

	return src, nil
}

// networkDecoder ties the ranged reader's context to the decoder's lifetime, so
// closing the decoder cancels any request still in flight instead of leaving a
// watcher goroutine parked until the session ends.
//
// It embeds the inner decoder but must not hide its optional interfaces: the
// streamer type-asserts on decode.Seeker to choose native seek over
// reopen-and-discard, and on decode.Descriptor for the debug labels. An embedded
// interface promotes only its own methods, so both are forwarded explicitly. A
// hidden seeker would turn every seek on a ranged URL into a re-download.
type networkDecoder struct {
	decode.Decoder
	cancel context.CancelFunc
}

func (d *networkDecoder) Close() error {
	err := d.Decoder.Close()
	d.cancel()

	return err
}

// SeekFrame forwards to the inner decoder when it can seek and reports
// ErrNotSeekable otherwise, matching how a forward-only container decoder
// advertises the method and refuses it.
func (d *networkDecoder) SeekFrame(frame int64) error {
	if s, ok := d.Decoder.(decode.Seeker); ok {
		return s.SeekFrame(frame)
	}

	return decode.ErrNotSeekable
}

func (d *networkDecoder) DecoderName() string {
	decoder, _ := decode.Describe(d.Decoder)

	return decoder
}

func (d *networkDecoder) ParserName() string {
	_, parser := decode.Describe(d.Decoder)

	return parser
}

// networkMeta describes the track from the URL alone. Container and Codec are
// best-effort guesses from the path extension: they are display labels, and the
// decoder's real stream info replaces the shape regardless.
func networkMeta(ref string, u *url.URL) meta.Meta {
	container, codec := networkLabels(u.Path)

	return meta.Meta{
		Path:      ref,
		Container: container,
		Codec:     codec,
		Tags:      meta.Tags{Title: networkTitle(u.Path)},
	}
}

// networkLabels maps a URL path extension to a container and codec label. It is
// a display hint only; an unknown extension leaves both empty.
func networkLabels(p string) (container, codec string) {
	switch strings.ToLower(path.Ext(p)) {
	case ".opus", ".ogg", ".oga":
		return "ogg", "opus"
	case ".webm", ".weba":
		return "webm", "opus"
	default:
		return "", ""
	}
}

// networkTitle is the last path segment of the URL, or "" when the path ends at
// a directory. It is what a queue row shows until a tag resolver or the UI
// supplies a better name.
func networkTitle(p string) string {
	base := path.Base(p)
	if base == "." || base == "/" {
		return ""
	}

	return base
}

// The reader the provider hands the decode registry must satisfy the contracts
// the container readers type-assert on, or a URL would silently take the
// forward-only path and lose native seek.
var (
	_ AudioProvider = (*NetworkProvider)(nil)
	_ io.Reader     = (*httpRangeReader)(nil)
	_ io.ReaderAt   = (*httpRangeReader)(nil)
	_ io.ReadSeeker = (*httpRangeReader)(nil)
)
