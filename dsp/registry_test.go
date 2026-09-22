package dsp

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
)

// fakeEffect is a minimal Effect that records how it was called and can be
// told to report latency, so the chain's ordering and latency logic are
// testable without a real DSP stage.
type fakeEffect struct {
	name string

	mu      sync.Mutex
	order   *[]string
	latency time.Duration
	bypass  bool
	resets  int
	frames  int
}

func (f *fakeEffect) Name() string { return f.name }

func (f *fakeEffect) Schema() []Param { return withCommon(nil) }

func (f *fakeEffect) Get(string) (any, error) { return nil, ErrUnknownParam }

func (f *fakeEffect) Set(string, any) error { return nil }

func (f *fakeEffect) Configure(in core.FrameFormat) (core.FrameFormat, error) { return in, nil }

func (f *fakeEffect) Process(_ []float32, frames int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames += frames
	if f.order != nil {
		*f.order = append(*f.order, f.name)
	}

	return nil
}

func (f *fakeEffect) Reset() error {
	f.mu.Lock()
	f.resets++
	f.mu.Unlock()

	return nil
}

func (f *fakeEffect) Latency() time.Duration { return f.latency }

func (f *fakeEffect) Bypassed() bool { return f.bypass }

// fakeFactory registers a fake effect under a kind and impl with a weight.
type fakeFactory struct {
	kind       string
	impl       string
	weight     int
	placement  Placement
	latency    time.Duration
	bypass     bool
	newCalls   *int
	newCallsMu *sync.Mutex
}

func (f *fakeFactory) Kind() string         { return f.kind }
func (f *fakeFactory) Impl() string         { return f.impl }
func (f *fakeFactory) FriendlyName() string { return f.impl }
func (f *fakeFactory) Weight() int          { return f.weight }
func (f *fakeFactory) Placement() Placement { return f.placement }
func (f *fakeFactory) Schema() []Param      { return withCommon(nil) }

func (f *fakeFactory) New(Values) (Effect, error) {
	if f.newCalls != nil {
		f.newCallsMu.Lock()
		*f.newCalls++
		f.newCallsMu.Unlock()
	}

	return &fakeEffect{name: f.impl, latency: f.latency, bypass: f.bypass}, nil
}

func TestRegistrySelectsHighestWeight(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{kind: "eq", impl: "eq-plain", weight: 10})
	r.Register(&fakeFactory{kind: "eq", impl: "eq-fancy", weight: 90})

	f, err := r.Winner("eq")
	if err != nil {
		t.Fatalf("Winner: %v", err)
	}
	if f.Impl() != "eq-fancy" {
		t.Fatalf("winner = %q, want eq-fancy", f.Impl())
	}

	e, err := r.New("eq", "", nil)
	if err != nil {
		t.Fatalf("New(auto): %v", err)
	}
	if e.Name() != "eq-fancy" {
		t.Fatalf("auto-selected %q, want eq-fancy", e.Name())
	}
}

func TestRegistryForcedImplIgnoresWeight(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{kind: "eq", impl: "eq-plain", weight: 10})
	r.Register(&fakeFactory{kind: "eq", impl: "eq-fancy", weight: 90})

	e, err := r.New("eq", "eq-plain", nil)
	if err != nil {
		t.Fatalf("New(forced): %v", err)
	}
	if e.Name() != "eq-plain" {
		t.Fatalf("forced name = %q, want eq-plain", e.Name())
	}
}

func TestRegistryTieKeepsLastRegistration(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{kind: "eq", impl: "eq-first", weight: 50})
	r.Register(&fakeFactory{kind: "eq", impl: "eq-second", weight: 50})

	f, err := r.Winner("eq")
	if err != nil {
		t.Fatalf("Winner: %v", err)
	}
	if f.Impl() != "eq-second" {
		t.Fatalf("tie winner = %q, want eq-second", f.Impl())
	}
}

func TestRegistryUnknownKindAndImpl(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{kind: "eq", impl: "eq-plain", weight: 10})

	if _, err := r.New("nope", "", nil); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("New(unknown kind) = %v, want ErrUnknownKind", err)
	}
	if _, err := r.New("eq", "nope", nil); !errors.Is(err, ErrUnknownImpl) {
		t.Fatalf("New(unknown impl) = %v, want ErrUnknownImpl", err)
	}
}

func TestRegistryRejectsIncompleteFactory(t *testing.T) {
	r := NewRegistry()
	for _, f := range []Factory{
		&fakeFactory{kind: "", impl: "x"},
		&fakeFactory{kind: "k", impl: ""},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("Register(%+v) did not panic", f)
				}
			}()
			r.Register(f)
		}()
	}
}

func TestRegistryKindsAreSortedAndUnique(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{kind: "zeta", impl: "z"})
	r.Register(&fakeFactory{kind: "alpha", impl: "a"})
	r.Register(&fakeFactory{kind: "alpha", impl: "a2"})

	kinds := r.Kinds()
	if len(kinds) != 2 || kinds[0] != "alpha" || kinds[1] != "zeta" {
		t.Fatalf("Kinds() = %v, want [alpha zeta]", kinds)
	}
}

func TestRegistrySchemaComesFromWinner(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{kind: "eq", impl: "eq-plain", weight: 10})
	r.Register(&fakeFactory{kind: "eq", impl: "eq-fancy", weight: 90})

	schema, err := r.Schema("eq")
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	// Every schema leads with the standard parameters, in order, so a UI can
	// render them without knowing the effect.
	want := []string{ParamBypass, ParamInputGain, ParamOutputGain}
	if len(schema) != len(want) {
		t.Fatalf("Schema has %d params, want %d: %+v", len(schema), len(want), schema)
	}
	for i, key := range want {
		if schema[i].Key != key {
			t.Fatalf("Schema[%d].Key = %q, want %q", i, schema[i].Key, key)
		}
	}
}

func TestChainRunsEffectsInOrder(t *testing.T) {
	var order []string
	c := NewChain()
	c.Set([]Effect{
		&fakeEffect{name: "first", order: &order},
		&fakeEffect{name: "second", order: &order},
		&fakeEffect{name: "third", order: &order},
	})

	if err := c.Process(make([]float32, 64), 32); err != nil {
		t.Fatalf("Process: %v", err)
	}
	want := []string{"first", "second", "third"}
	if len(order) != len(want) {
		t.Fatalf("ran %d effects, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestChainSwapTakesEffectOnNextProcess(t *testing.T) {
	c := NewChain()
	c.Set([]Effect{&fakeEffect{name: "old"}})

	var order []string
	c.Set([]Effect{&fakeEffect{name: "new", order: &order}})
	if err := c.Process(make([]float32, 8), 4); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(order) != 1 || order[0] != "new" {
		t.Fatalf("after swap ran %v, want [new]", order)
	}
}

func TestChainLatencySkipsBypassedEffects(t *testing.T) {
	c := NewChain()
	c.Set([]Effect{
		&fakeEffect{name: "a", latency: 5 * time.Millisecond},
		&fakeEffect{name: "b", latency: 3 * time.Millisecond, bypass: true},
		&fakeEffect{name: "c", latency: 2 * time.Millisecond},
	})

	if got := c.Latency(); got != 7*time.Millisecond {
		t.Fatalf("Latency() = %v, want 7ms (bypassed effect excluded)", got)
	}
}

func TestChainResetReachesEveryEffect(t *testing.T) {
	a := &fakeEffect{name: "a"}
	b := &fakeEffect{name: "b"}
	c := NewChain()
	c.Set([]Effect{a, b})

	if err := c.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if a.resets != 1 || b.resets != 1 {
		t.Fatalf("resets: a=%d b=%d, want 1 each", a.resets, b.resets)
	}
}

func TestChainProcessIsSafeDuringRepeatedSwap(t *testing.T) {
	cf, err := NewCrossfeedFactory().New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := cf.Configure(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	c := NewChain()
	c.Set([]Effect{cf})
	buf := twoTone(480, 48000)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Swap between an empty chain and the crossfeed, so the audio
			// thread races a rebuild the way a pipeline edit would.
			if _, err := NewEffects(nil); err == nil {
				c.Set([]Effect{cf})
			}
			c.Set(nil)
		}
	}()

	for i := 0; i < 5000; i++ {
		if err := c.Process(buf, 240); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestNewEffectsNamesTheFailingStage(t *testing.T) {
	_, err := NewEffects([]Spec{{Kind: "crossfeed"}, {Kind: "does-not-exist"}})
	if err == nil {
		t.Fatal("NewEffects accepted an unknown kind")
	}
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("error = %v, want ErrUnknownKind", err)
	}
}
