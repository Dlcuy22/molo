// Package meta turns a media path into the descriptive metadata a UI shows:
// tags, cover art and the stream shape. Resolvers are pluggable, so a new
// source (a sidecar file, a music database, an FFprobe wrapper) is added
// without touching the chain or the player.
//
// A resolver that fails is not fatal: the chain moves down the priority order
// and lets a cheaper answer stand in. That is why resolving a path almost
// always succeeds even for a file with no tags at all.
package meta

import (
	"context"
	"errors"
	"sort"

	"github.com/dlcuy22/player/core"
)

// ErrNoMatch means every matching resolver failed. It is not returned for an
// untagged file, because the filename resolver always has an answer.
var ErrNoMatch = errors.New("meta: no resolver could describe the path")

// Tags is the human-facing part of the metadata. Zero values mean "the source
// did not provide this", which lets a UI decide what to show instead of
// guessing.
type Tags struct {
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	Track       int
	Disc        int
	Year        int

	// Cover is the raw image bytes and CoverMIME the type the source declared.
	// A UI hands both to an image decoder; the engine never touches them.
	Cover     []byte
	CoverMIME string
}

// Meta is everything known about one media file. Stream is filled by whoever
// opened the file, not by a resolver, because only the decoder can report the
// true stream shape.
type Meta struct {
	Path      string
	Container string
	Codec     string
	Stream    core.StreamInfo
	Tags      Tags

	// Source names the resolver that answered, for debugging and for a UI that
	// wants to show where the title came from.
	Source string
}

// Resolver is one source of metadata. Match is a cheap path-only check, so a
// resolver is never opened for a file it cannot handle.
type Resolver interface {
	Name() string
	Priority() int
	Match(path string) bool
	Resolve(ctx context.Context, path string) (*Meta, error)
}

// Chain runs resolvers from highest priority to lowest and returns the first
// success. Failure is a signal to fall through, never an abort.
type Chain struct {
	resolvers []Resolver
}

// NewChain sorts a copy of resolvers by descending priority so Resolve never
// has to. Ties keep the caller's order, which makes a chain with two equal
// priorities predictable.
func NewChain(resolvers ...Resolver) *Chain {
	sorted := append([]Resolver(nil), resolvers...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority() > sorted[j].Priority()
	})

	return &Chain{resolvers: sorted}
}

// Name identifies the chain in Meta.Source when a caller uses it directly.
func (c *Chain) Name() string { return "chain" }

// Priority is zero: a chain is a container for resolvers, not a peer of them.
func (c *Chain) Priority() int { return 0 }

// Match always reports true; the chain delegates matching to its members.
func (c *Chain) Match(string) bool { return true }

// Resolve walks the chain in priority order and returns the first resolver
// that both matches and succeeds.
func (c *Chain) Resolve(ctx context.Context, path string) (*Meta, error) {
	for _, r := range c.resolvers {
		if !r.Match(path) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		m, err := r.Resolve(ctx, path)
		if err != nil || m == nil {
			continue
		}

		return m, nil
	}

	return nil, ErrNoMatch
}

// Resolvers returns the chain's members in the order Resolve will try them.
func (c *Chain) Resolvers() []Resolver {
	return append([]Resolver(nil), c.resolvers...)
}

// defaultChain is built once because sorting it on every call would be waste;
// the resolvers are stateless.
var defaultChain = NewChain(NewEmbeddedTags(), NewFilename())

// Default is the chain a player uses unless the caller supplies another.
func Default() *Chain { return defaultChain }

// Resolve runs the default chain. It is the entry point for callers that have
// no reason to customise their resolvers.
func Resolve(ctx context.Context, path string) (*Meta, error) {
	return defaultChain.Resolve(ctx, path)
}
