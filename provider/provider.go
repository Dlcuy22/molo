// Package provider maps a track reference to the audio bytes and metadata the
// engine needs. It is the one seam that makes a non-file source possible
// without the engine importing a network library: a provider may speak to a
// service, but only bytes and a description cross back into the engine.
package provider

import (
	"context"
	"errors"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/meta"
	"github.com/dlcuy22/player/stream"
)

// ErrNoProvider means a session with providers configured was handed a
// reference no provider claimed. It is never returned when the provider list is
// empty: an empty list is the legacy local-only path, which is behaviourally
// LocalAudio.
var ErrNoProvider = errors.New("provider: no provider matches the reference")

// AudioProvider resolves references it owns into a playable Source. Providers
// are tried in order, so a URL provider claims its own scheme and LocalAudio
// stays the last-resort fallback.
type AudioProvider interface {
	// Name identifies the provider in logs and in a UI list.
	Name() string

	// Match reports whether ref belongs to this provider. It must be cheap and
	// must not open anything: the session calls it for every queued ref.
	Match(ref string) bool

	// Open resolves ref. It may block on the network, so the session runs it on
	// the build worker, never on the control loop. The session cancels ctx only
	// on Close, and Close then waits for the worker, so an implementation that
	// ignores ctx can block Close forever with no timeout. Respect it: return
	// ctx.Err() from a long wait, and close the stop channel passed to the
	// Opener when it fires.
	Open(ctx context.Context, ref string) (Source, error)
}

// Source is one resolved track. Opener is a factory, not a decoder, because the
// streamer calls it again on the reopen-and-discard seek fallback
// (stream/streamer.go:717), on a decoder swap (streamer.go:399) and on the
// restore after a failed swap (streamer.go:626); a provider that cannot re-open
// cannot seek. Returning one decoder would make remote seek impossible, so the
// seam deliberately exposes the way to open again rather than the open result.
//
// Opener must return a non-nil decoder or an error, never (nil, nil): the
// session calls Info on the result, so a nil decoder would panic the producer
// goroutine.
type Source struct {
	Opener stream.Opener

	// Local marks the reference as a filesystem path. The session then keeps its
	// own metadata resolver, duration prober and decoder preference, and ignores
	// Meta and Probe. LocalAudio sets it. A provider that wraps a filesystem
	// source must set it too; the session never inspects the provider's identity,
	// so a wrapper that sets Local behaves exactly like LocalAudio itself.
	Local bool

	// Meta describes the track up front. It is carried because the session's
	// local resolver is useless for a remote ref: there is no file to read, and
	// meta.Default() would invent a title from the ref string. For a local path
	// the session still runs its own resolver, so a provider leaves this zero.
	// Meta.Stream is advisory: the session replaces it with the decoder's real
	// core.StreamInfo, which only the decoder can report truthfully.
	Meta meta.Meta

	// Probe optionally reports the stream shape without decoding. Nil means
	// "unknown": the session then falls back to its own prober, which only works
	// for a local path, and leaves the total unknown for a remote ref. It is
	// carried for the same reason as Meta: only the provider can reach the
	// source.
	Probe func(mode core.DurationMode) (core.StreamInfo, error)

	// Upgrade optionally offers a replacement decoder for the seamless-swap
	// path. When non-nil and the session has the experimental upgrade enabled,
	// the session calls it once, off the control loop, after the track starts.
	// It may block until the replacement is ready and must honour ctx. The
	// returned Opener must build a decoder in the same format as Opener's, or
	// the swap is refused and the original keeps playing. Nil means the source
	// has no upgrade and the track uses the ordinary path.
	Upgrade func(ctx context.Context) (stream.Opener, error)
}

// LocalAudio is the default provider: a filesystem path through the decode
// registry. Match always returns true, so it must stay last in the list. It is
// exported so a consumer can add it explicitly as a fallback after a URL
// provider without giving up the local path's behaviour.
type LocalAudio struct{}

// Name identifies LocalAudio. It matches the historic local path.
func (LocalAudio) Name() string { return "local" }

// Match always reports true: any ref the earlier providers did not claim is
// treated as a local path, which is how the engine behaved before this seam.
func (LocalAudio) Match(string) bool { return true }

// Open resolves ref through the process-wide decode registry. It sets Local so
// the session keeps its own resolver, prober and decoder preference, and leaves
// Meta and Probe nil: duplicating them here would fork the local behaviour.
func (LocalAudio) Open(ctx context.Context, ref string) (Source, error) {
	if err := ctx.Err(); err != nil {
		return Source{}, err
	}

	return Source{
		Local: true,
		Opener: func(<-chan struct{}) (decode.Decoder, error) {
			// The stop channel is not consulted here: OpenNamed reads a regular
			// file, so it cannot block indefinitely, and the session refuses a
			// new open once closing. This matches the contract Config.openDecoder
			// had.
			return decode.Default.OpenNamed("", ref)
		},
	}, nil
}
