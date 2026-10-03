package dsp

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrUnknownKind means no effect is registered under the requested kind.
var ErrUnknownKind = errors.New("dsp: unknown effect kind")

// ErrUnknownImpl means a kind exists but has no implementation by that name.
var ErrUnknownImpl = errors.New("dsp: unknown effect implementation")

// Factory builds one effect implementation. It mirrors decode.Factory: the
// registry dispatches on Kind and Impl, Weight decides the automatic choice,
// and Schema lets a UI render the controls before anything is built.
type Factory interface {
	// Kind is the pipeline slot a preset names, such as "crossfeed". Two
	// implementations of one kind are interchangeable in a preset.
	Kind() string

	// Impl is this realisation of the kind, such as "crossfeed-bs2b". It is
	// the name a caller forces when the automatic pick is not wanted.
	Impl() string

	// FriendlyName is the label a UI shows.
	FriendlyName() string

	// Weight orders the implementations of a kind; higher wins. It is only
	// read for automatic selection, exactly as decode weights are.
	Weight() int

	// Placement says which slot the effect may occupy.
	Placement() Placement

	// Schema describes the parameters. It must be stable for the life of the
	// process, because a UI may cache it.
	Schema() []Param

	// New builds an effect from a parameter set. Missing keys take their
	// defaults; an unknown key is an error.
	New(values Values) (Effect, error)
}

// Registry dispatches (kind, impl) pairs to the factories registered at init
// time. An empty impl means the highest-weight factory for that kind.
type Registry interface {
	Register(f Factory)

	// Kinds lists the registered effect kinds, sorted.
	Kinds() []string

	// Schema returns the parameter set of the implementation that an empty
	// impl would select, so a UI can render before building.
	Schema(kind string) ([]Param, error)

	// SchemaFor returns the parameter schema of one (kind, impl) pair. An
	// empty impl means the highest-weight implementation, identical to Schema.
	SchemaFor(kind, impl string) ([]Param, error)

	// Impls lists every registered factory in registration order, so a chooser
	// can offer each (kind, impl) pair. It is the effect-side analogue of
	// decode's Codecs.
	Impls() []Factory

	// Winner reports the implementation automatic selection would use.
	Winner(kind string) (Factory, error)

	// New builds the chosen implementation. An empty impl selects by weight.
	New(kind, impl string, values Values) (Effect, error)
}

type registry struct {
	factories []Factory
}

// NewRegistry returns an empty registry. Implementations register into the
// package-level Default instead; this exists for tests that want an isolated
// set.
func NewRegistry() Registry { return &registry{} }

// Default is the registry populated by the init functions in this package.
var Default = NewRegistry()

// Register adds a factory to the process-wide Default registry.
func Register(f Factory) { Default.Register(f) }

// Register appends a factory. It panics only on a nil factory or on an empty
// kind or impl. A duplicate (kind, impl) pair is not a panic: the pair is
// appended, and byImpl resolves the last registration, so a later registration
// overrides the earlier one for lookups.
func (r *registry) Register(f Factory) {
	if f == nil {
		panic("dsp: Register requires a factory")
	}
	if f.Kind() == "" || f.Impl() == "" {
		panic("dsp: Register requires a kind and an impl")
	}

	r.factories = append(r.factories, f)
}

// Kinds lists the registered kinds, sorted and free of duplicates, for a UI
// that offers a pipeline editor.
func (r *registry) Kinds() []string {
	seen := make(map[string]struct{}, len(r.factories))
	for _, f := range r.factories {
		seen[f.Kind()] = struct{}{}
	}

	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)

	return kinds
}

// byImpl returns the last factory registered under (kind, impl), so a host
// that re-registers the pair overrides the built-in one.
func (r *registry) byImpl(kind, impl string) Factory {
	var found Factory
	for _, f := range r.factories {
		if f.Kind() == kind && f.Impl() == impl {
			found = f
		}
	}

	return found
}

// best returns the highest-weight factory for a kind, with the last
// registration breaking a tie. That matches decode's extensionWinner.
func (r *registry) best(kind string) Factory {
	var winner Factory
	winnerWeight := 0
	for _, f := range r.factories {
		if f.Kind() != kind {
			continue
		}
		if winner == nil || f.Weight() >= winnerWeight {
			winner = f
			winnerWeight = f.Weight()
		}
	}

	return winner
}

func (r *registry) Winner(kind string) (Factory, error) {
	f := r.best(kind)
	if f == nil {
		return nil, fmt.Errorf("%w: %q (available: %s)", ErrUnknownKind, kind, strings.Join(r.Kinds(), ", "))
	}

	return f, nil
}

func (r *registry) Schema(kind string) ([]Param, error) {
	return r.SchemaFor(kind, "")
}

// Impls lists every registered factory, in registration order, so a chooser can
// offer each (kind, impl) pair. The returned slice is a copy: mutating it cannot
// reach the registry.
func (r *registry) Impls() []Factory {
	return append([]Factory(nil), r.factories...)
}

// SchemaFor resolves one (kind, impl) pair and returns its schema. An empty
// impl is the automatic choice, so this is the one lookup both Schema and the
// chain editor use. A named impl that does not exist is ErrUnknownImpl rather
// than a silent fallback to the winner, because a chain stage that names an
// implementation must get that one or be rejected.
//
// An unknown kind is reported as ErrUnknownKind even when an impl is named: the
// kind is the outer failure and is the more useful one to a caller, which is why
// the kind is checked before the impl.
func (r *registry) SchemaFor(kind, impl string) ([]Param, error) {
	if impl == "" {
		winner, err := r.Winner(kind)
		if err != nil {
			return nil, err
		}

		return cloneParams(winner.Schema()), nil
	}

	if r.best(kind) == nil {
		return nil, fmt.Errorf("%w: %q (available: %s)", ErrUnknownKind, kind, strings.Join(r.Kinds(), ", "))
	}
	f := r.byImpl(kind, impl)
	if f == nil {
		return nil, fmt.Errorf("%w: %q for kind %q", ErrUnknownImpl, impl, kind)
	}

	return cloneParams(f.Schema()), nil
}

func (r *registry) New(kind, impl string, values Values) (Effect, error) {
	var f Factory
	if impl == "" {
		winner, err := r.Winner(kind)
		if err != nil {
			return nil, err
		}
		f = winner
	} else {
		f = r.byImpl(kind, impl)
		if f == nil {
			return nil, fmt.Errorf("%w: %q for kind %q", ErrUnknownImpl, impl, kind)
		}
	}

	return f.New(values)
}

// Spec is one stage of a pipeline: which kind, which implementation, and the
// parameter set. It is the value a preset serialises.
type Spec struct {
	// ID names the instance, so two effects of one kind can coexist.
	ID string

	// Kind is required and names the slot, such as "crossfeed".
	Kind string

	// Impl is optional; empty selects the highest-weight implementation.
	Impl string

	// Params are the effect's settings. Validation happens at build time.
	Params Values
}

// Pipeline is an ordered set of stages split by placement. It is data, not
// code: a caller composes any chain by filling the slices, and a preset file
// is one serialisation of it.
//
// The Post half belongs to the real-time chain this package builds; the Pre
// half belongs to the streamer's producer, which is why it lives here as the
// shared description rather than inside either one.
type Pipeline struct {
	Pre  []Spec
	Post []Spec
}

// NewEffects builds every spec in order. A failure names the offending spec so
// a preset with one bad stage does not look like a total failure.
func NewEffects(specs []Spec) ([]Effect, error) {
	out := make([]Effect, 0, len(specs))
	for i, s := range specs {
		e, err := buildSpec(s)
		if err != nil {
			return nil, fmt.Errorf("dsp: stage %d (%s): %w", i, s.Kind, err)
		}
		out = append(out, e)
	}

	return out, nil
}

// BuildPost builds the post-ring half of the pipeline: the ordered effects the
// real-time chain runs. The pre-ring half is the streamer's business, so this
// deliberately ignores it and a caller that wants both builds them from the
// same Pipeline in their own two places.
func BuildPost(p Pipeline) ([]Effect, error) {
	return NewEffects(p.Post)
}

// buildSpec resolves one spec against the default registry and builds it.
func buildSpec(s Spec) (Effect, error) {
	if s.Kind == "" {
		return nil, fmt.Errorf("%w: stage has no kind", ErrUnknownKind)
	}

	return Default.New(s.Kind, s.Impl, s.Params)
}
