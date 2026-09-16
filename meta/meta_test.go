package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubResolver is a resolver whose behaviour each test scripts directly. calls
// counts Resolve invocations so a test can prove a lower-priority resolver was
// never consulted.
type stubResolver struct {
	name     string
	priority int
	match    bool
	meta     *Meta
	err      error
	calls    int
}

func (r *stubResolver) Name() string  { return r.name }
func (r *stubResolver) Priority() int { return r.priority }

func (r *stubResolver) Match(string) bool {
	return r.match
}

func (r *stubResolver) Resolve(context.Context, string) (*Meta, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}

	return r.meta, nil
}

func TestChainReturnsTheFirstSuccessfulResolver(t *testing.T) {
	low := &stubResolver{name: "low", priority: 1, match: true, meta: &Meta{Source: "low"}}
	mid := &stubResolver{name: "mid", priority: 50, match: true, meta: &Meta{Source: "mid"}}
	high := &stubResolver{name: "high", priority: 100, match: true, err: errors.New("boom")}

	c := NewChain(low, mid, high)
	m, err := c.Resolve(context.Background(), "x.opus")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Source != "mid" {
		t.Fatalf("Resolve picked %q, want the highest successful resolver %q", m.Source, "mid")
	}
	if high.calls != 1 {
		t.Fatalf("high-priority resolver ran %d times, want 1", high.calls)
	}
	if low.calls != 0 {
		t.Fatalf("low-priority resolver ran %d times; a success must stop the chain", low.calls)
	}
}

func TestChainOrdersByPriorityNotArguments(t *testing.T) {
	first := &stubResolver{name: "first", priority: 1, match: true, meta: &Meta{Source: "first"}}
	last := &stubResolver{name: "last", priority: 99, match: true, meta: &Meta{Source: "last"}}

	// The lower-priority resolver is passed first on purpose; the chain must
	// sort and consult the higher one.
	m, err := NewChain(first, last).Resolve(context.Background(), "x.opus")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Source != "last" {
		t.Fatalf("Resolve picked %q, want %q", m.Source, "last")
	}
}

func TestChainSkipsNonMatchingResolvers(t *testing.T) {
	skip := &stubResolver{name: "skip", priority: 100, match: false, meta: &Meta{Source: "skip"}}
	hit := &stubResolver{name: "hit", priority: 10, match: true, meta: &Meta{Source: "hit"}}

	m, err := NewChain(skip, hit).Resolve(context.Background(), "x.opus")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Source != "hit" {
		t.Fatalf("Resolve picked %q, want %q", m.Source, "hit")
	}
	if skip.calls != 0 {
		t.Fatalf("a non-matching resolver ran %d times, want 0", skip.calls)
	}
}

func TestChainFallsThroughEveryFailure(t *testing.T) {
	a := &stubResolver{name: "a", priority: 3, match: true, err: errors.New("a failed")}
	b := &stubResolver{name: "b", priority: 2, match: true, err: errors.New("b failed")}
	c := &stubResolver{name: "c", priority: 1, match: true, meta: &Meta{Source: "c"}}

	m, err := NewChain(a, b, c).Resolve(context.Background(), "x.opus")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Source != "c" {
		t.Fatalf("Resolve picked %q, want %q", m.Source, "c")
	}
	for _, r := range []*stubResolver{a, b, c} {
		if r.calls != 1 {
			t.Fatalf("resolver %q ran %d times, want exactly 1", r.name, r.calls)
		}
	}
}

func TestChainReportsWhenNothingCanAnswer(t *testing.T) {
	c := NewChain(&stubResolver{name: "only", priority: 1, match: true, err: errors.New("nope")})
	if _, err := c.Resolve(context.Background(), "x.opus"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Resolve error = %v, want ErrNoMatch", err)
	}
}

func TestChainEmptyIsErrNoMatch(t *testing.T) {
	if _, err := NewChain().Resolve(context.Background(), "x.opus"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Resolve error = %v, want ErrNoMatch", err)
	}
}

func TestChainSatisfiesResolver(t *testing.T) {
	var _ Resolver = NewChain()
}

func TestDefaultResolvesEmbeddedTagsFirst(t *testing.T) {
	m, err := Default().Resolve(context.Background(), fixturePath(t, "tagged.opus"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Source != NewEmbeddedTags().Name() {
		t.Fatalf("Source = %q, want the embedded resolver to answer first", m.Source)
	}
}

func TestDefaultFallsBackToFilenameForUntaggedFiles(t *testing.T) {
	// The Phase 1 fixtures carry an OpusTags packet but no fields, so the
	// embedded resolver has nothing to say and the filename must answer.
	m, err := Default().Resolve(context.Background(), filepath.Join("..", "decode", "testdata", "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Source != NewFilename().Name() {
		t.Fatalf("Source = %q, want the filename resolver", m.Source)
	}
	if m.Tags.Title != "stereo_2s" {
		t.Fatalf("Title = %q, want %q", m.Tags.Title, "stereo_2s")
	}
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s is missing: %v", name, err)
	}

	return path
}
