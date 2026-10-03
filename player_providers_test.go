package molo_test

import (
	"context"
	"testing"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/provider"
)

// namedProvider is a stub that only needs to report a name: the facade's
// Providers query does not open anything.
type namedProvider struct{ name string }

func (p namedProvider) Name() string      { return p.name }
func (p namedProvider) Match(string) bool { return false }
func (p namedProvider) Open(context.Context, string) (provider.Source, error) {
	return provider.Source{}, nil
}

// TestProvidersIsReachableThroughTheInterface is the M2 regression: an external
// caller holding the molo.Player interface must be able to ask which sources
// are configured. Before the method was on the interface this did not compile.
func TestProvidersIsReachableThroughTheInterface(t *testing.T) {
	// The assignment to the interface is the point: it fails to compile if
	// Providers is missing from molo.Player.
	var p molo.Player

	var err error
	p, err = molo.New(molo.WithProviders(
		namedProvider{name: "remote"},
		provider.LocalAudio{},
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	got := p.Providers()
	want := []string{"remote", "local"}
	if len(got) != len(want) {
		t.Fatalf("Providers() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Providers() = %v, want %v", got, want)
		}
	}
}

// TestProvidersEmptyByDefault pins the local-only default.
func TestProvidersEmptyByDefault(t *testing.T) {
	p, err := molo.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	if got := p.Providers(); len(got) != 0 {
		t.Fatalf("Providers() = %v, want empty", got)
	}
}
