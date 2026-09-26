package session

import (
	"errors"
	"fmt"
	"sync"

	"github.com/dlcuy22/player/dsp"
)

// ErrUnknownEffect means an effect stage ID does not name a stage in the chain
// in force. It is returned by every editor command that addresses a stage, so a
// UI can tell a stale ID apart from a bad parameter or an out-of-range move.
var ErrUnknownEffect = errors.New("session: unknown effect stage")

// ErrEffectIndexOutOfRange means MoveEffect named a position outside the chain.
var ErrEffectIndexOutOfRange = errors.New("session: effect index out of range")

// scriptedKind is the pipeline slot every scripted effect shares. A kind is
// scripted when its factory registers under this name. The session compares the
// string rather than importing the script package, which would pull a Lua
// interpreter into every build that only wants an editor.
const scriptedKind = "script"

// EffectKindInfo describes one registered implementation for a chooser. It is
// the (kind, impl) pair a caller passes to AddEffect, plus the label and the
// script flag a UI needs to group the list.
type EffectKindInfo struct {
	Kind     string
	Impl     string
	Label    string
	Weight   int
	Scripted bool
}

// EffectStage is one stage of the chain in force: its identity, its schema, its
// current parameter values and its live meters. Schema and Values are snapshots
// taken when the stage is read, so a UI can render it without holding an
// effect. Meters is nil for an effect that does not implement dsp.Metered.
type EffectStage struct {
	ID       string
	Kind     string
	Impl     string
	Label    string
	Bypassed bool
	Schema   []dsp.Param
	Values   dsp.Values
	Meters   map[string]float32
}

// EffectKinds lists every registered implementation, one entry per (kind, impl)
// pair, so a chooser can offer crossfeed-bs2b and a scripted effect side by
// side. It reads the process-wide registry, which never changes after init.
func (s *Session) EffectKinds() []EffectKindInfo {
	factories := dsp.Default.Impls()
	out := make([]EffectKindInfo, 0, len(factories))
	for _, f := range factories {
		out = append(out, EffectKindInfo{
			Kind:     f.Kind(),
			Impl:     f.Impl(),
			Label:    f.FriendlyName(),
			Weight:   f.Weight(),
			Scripted: f.Kind() == scriptedKind,
		})
	}

	return out
}

// EffectStages returns the chain in force in processing order, each stage with
// its schema, values and live meters.
//
// The runtime pipeline and the live chain are two views of one thing, published
// by different steps: the pipeline is written when a change is accepted and the
// chain is swapped by the control loop a moment later. A rebuild in flight can
// therefore leave the two briefly out of step. Pairing them by index in that
// window would report one stage's identity with another stage's meters, so this
// pairs them only while the pipeline generation matches the generation the
// control loop last installed. A chain caught mid-install reports the accepted
// description with no meters rather than a wrong pairing, and the next read
// settles.
func (s *Session) EffectStages() []EffectStage {
	pipeline, _ := s.runtime.pipelineSnapshot()
	specs := pipeline.Post
	inst := s.installed.Load()
	if inst == nil {
		inst = s.runtime.pendingSnapshot()
	}

	out := make([]EffectStage, 0, len(specs))
	for _, spec := range specs {
		// Resolve by ID, not by index: the description and the live chain can
		// come from different accepted changes, and an ID says exactly which
		// effect belongs to which stage. A stage with no live effect yet
		// reports its description with no meters rather than a wrong pairing.
		if inst != nil {
			if e, ok := inst.effect(spec); ok {
				out = append(out, stageFromEffect(spec, e))

				continue
			}
		}
		out = append(out, stageFromSpec(spec))
	}

	return out
}

// stageFromSpec describes a stage with no live effect behind it. The schema
// comes from the registry and the values from the spec, which already carries
// the caller's parameter set.
func stageFromSpec(spec dsp.Spec) EffectStage {
	stage := EffectStage{
		ID:    spec.ID,
		Kind:  spec.Kind,
		Impl:  spec.Impl,
		Label: specLabel(spec.Kind, spec.Impl),
	}
	if schema, err := dsp.Default.SchemaFor(spec.Kind, spec.Impl); err == nil {
		stage.Schema = schema
	}
	if len(spec.Params) > 0 {
		stage.Values = make(dsp.Values, len(spec.Params))
		for k, v := range spec.Params {
			stage.Values[k] = v
		}
	}
	if b, ok := spec.Params[dsp.ParamBypass].(bool); ok {
		stage.Bypassed = b
	}

	return stage
}

// stageFromEffect describes one live stage: identity from the spec, schema and
// values from the built effect so a value the effect itself derived is reported
// correctly, and meters when the effect measures them.
func stageFromEffect(spec dsp.Spec, e dsp.Effect) EffectStage {
	schema := e.Schema()
	stage := EffectStage{
		ID:       spec.ID,
		Kind:     spec.Kind,
		Impl:     spec.Impl,
		Label:    specLabel(spec.Kind, spec.Impl),
		Bypassed: effectBypassed(e),
		Schema:   schema,
		Values:   effectValues(e, schema),
	}
	if m, ok := e.(dsp.Metered); ok {
		stage.Meters = m.Meters()
	}

	return stage
}

// effectValues reads every schema parameter from a live effect, so the reported
// set reflects what the effect actually stored rather than what the spec asked
// for when the two differ after a Set.
func effectValues(e dsp.Effect, schema []dsp.Param) dsp.Values {
	out := make(dsp.Values, len(schema))
	for _, p := range schema {
		v, err := e.Get(p.Key)
		if err != nil {
			continue
		}
		out[p.Key] = v
	}

	return out
}

// effectBypassed reports the standard bypass flag. Shipped effects implement
// dsp.Bypassable; the Get fallback covers one that only carries the parameter.
func effectBypassed(e dsp.Effect) bool {
	if b, ok := e.(dsp.Bypassable); ok {
		return b.Bypassed()
	}
	v, err := e.Get(dsp.ParamBypass)

	return err == nil && v == true
}

// specLabel resolves the friendly name a UI shows for a (kind, impl) pair, or
// an empty string when the pair is no longer registered.
func specLabel(kind, impl string) string {
	for _, f := range dsp.Default.Impls() {
		if f.Kind() == kind && f.Impl() == impl {
			return f.FriendlyName()
		}
	}

	return ""
}

// AddEffect appends one stage to the chain and returns its new ID. The stage is
// built on the caller's goroutine, exactly as ApplyPipeline builds, and the
// install is waited for, so the returned stage is already live: the caller can
// set a parameter on it immediately. Waiting is bounded by the control loop,
// which only does bounded work, and by Close.
//
// The whole read-modify-write is serialized, so two concurrent calls each see
// the other's stage instead of starting from the same list and dropping one.
func (s *Session) AddEffect(kind, impl string) (string, error) {
	if kind == "" {
		return "", fmt.Errorf("%w: stage has no kind", dsp.ErrUnknownKind)
	}

	s.editorMu.Lock()
	defer s.editorMu.Unlock()

	specs := s.chainSpecs()
	id := s.newEffectID(kind, impl, specs)
	specs = append(specs, dsp.Spec{ID: id, Kind: kind, Impl: impl})
	if err := s.applyPipelineWait(dsp.Pipeline{Post: specs}); err != nil {
		return "", err
	}

	return id, nil
}

// RemoveEffect drops the stage with id and rebuilds the chain without it. Only
// the first match is removed, so a duplicate ID cannot silently delete more
// than the caller asked for.
func (s *Session) RemoveEffect(id string) error {
	s.editorMu.Lock()
	defer s.editorMu.Unlock()

	specs := s.chainSpecs()
	next := make([]dsp.Spec, 0, len(specs))
	found := false
	for _, spec := range specs {
		if spec.ID == id && !found {
			found = true

			continue
		}
		next = append(next, spec)
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrUnknownEffect, id)
	}

	return s.applyPipelineWait(dsp.Pipeline{Post: next})
}

// MoveEffect moves the stage with id so it lands at index to in processing
// order. Reordering is a new chain, so it rebuilds through ApplyPipeline and
// its atomic install rather than mutating the live list the audio thread reads.
func (s *Session) MoveEffect(id string, to int) error {
	s.editorMu.Lock()
	defer s.editorMu.Unlock()

	specs := s.chainSpecs()
	from := -1
	for i, spec := range specs {
		if spec.ID == id {
			from = i

			break
		}
	}
	if from < 0 {
		return fmt.Errorf("%w: %q", ErrUnknownEffect, id)
	}
	if to < 0 || to >= len(specs) {
		return fmt.Errorf("%w: %d (chain has %d stages)", ErrEffectIndexOutOfRange, to, len(specs))
	}
	if from == to {
		return nil
	}

	moved := specs[from]
	next := make([]dsp.Spec, 0, len(specs))
	for i, spec := range specs {
		if i == from {
			continue
		}
		if len(next) == to {
			next = append(next, moved)
		}
		next = append(next, spec)
	}
	if len(next) == to {
		next = append(next, moved)
	}

	return s.applyPipelineWait(dsp.Pipeline{Post: next})
}

// SetEffectParam applies one parameter to the live effect named by id. It does
// not rebuild the chain: a rebuild constructs a new effect, which discards
// state such as a filter's memory or a fade's ramp position. dsp.Effect.Set is
// documented safe while Process runs, so the change lands on the running effect
// and its state survives.
//
// Any lock the lookup takes is released before Set runs, so Set is never called
// while holding a lock the audio thread could want.
func (s *Session) SetEffectParam(id, key string, value any) error {
	e, err := s.liveEffect(id)
	if err != nil {
		return err
	}

	return e.Set(key, value)
}

// SetEffectBypass toggles the standard bypass parameter of the live effect.
// Bypass is just another key on the effect, so it takes the same no-rebuild
// path as any other parameter and a bypassed stage keeps its state.
func (s *Session) SetEffectBypass(id string, bypassed bool) error {
	return s.SetEffectParam(id, dsp.ParamBypass, bypassed)
}

// liveEffect resolves a stage ID to the effect currently installed. It looks the
// ID up in the live chain and, when the control loop has not published the
// newest change yet, in the pending chain for that same change. Resolving by ID
// rather than by a generation and an index means a stage is found whenever its
// effect exists, even if the caller read a description from an older snapshot.
// The returned effect is safe to Set while audio runs.
func (s *Session) liveEffect(id string) (dsp.Effect, error) {
	// Confirm the ID is a real stage before resolving it, so a stale or made-up
	// ID is an error even when a same-index stage exists.
	pipeline, _ := s.runtime.pipelineSnapshot()
	var spec dsp.Spec
	found := false
	for _, sp := range pipeline.Post {
		if sp.ID == id {
			spec = sp
			found = true

			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrUnknownEffect, id)
	}

	if inst := s.installed.Load(); inst != nil {
		if e, ok := inst.effect(spec); ok {
			return e, nil
		}
	}
	// The live chain does not hold it, which is the window between accepting a
	// change and the control loop publishing it. The pending chain is the
	// effects for exactly that change, so a Set right after an Add resolves to
	// the effect about to go live instead of being refused.
	if pend := s.runtime.pendingSnapshot(); pend != nil {
		if e, ok := pend.effect(spec); ok {
			return e, nil
		}
	}

	return nil, fmt.Errorf("%w: %q (chain is mid-install)", ErrUnknownEffect, id)
}

// chainSpecs returns a fresh copy of the Post stages in force. A caller edits
// the copy and hands it back through ApplyPipeline, so the stored pipeline is
// never mutated in place.
func (s *Session) chainSpecs() []dsp.Spec {
	return append([]dsp.Spec(nil), s.runtime.Pipeline().Post...)
}

// newEffectID hands out a unique stage ID. The counter is monotonic so an ID is
// never reused after a remove: a reused ID would let a stale UI reference
// address whichever stage next took the slot. It also skips any ID already in
// specs, so a caller-supplied ID from ApplyPipeline or SetSettings cannot be
// collided with, which would otherwise make RemoveEffect and liveEffect address
// different stages.
func (s *Session) newEffectID(kind, impl string, specs []dsp.Spec) string {
	s.effectIDs.mu.Lock()
	defer s.effectIDs.mu.Unlock()
	base := impl
	if base == "" {
		base = kind
	}
	for {
		s.effectIDs.next++
		id := fmt.Sprintf("%s-%d", base, s.effectIDs.next)
		if !specHasID(specs, id) {
			return id
		}
	}
}

// specHasID reports whether a stage ID is already in use.
func specHasID(specs []dsp.Spec, id string) bool {
	for _, spec := range specs {
		if spec.ID == id {
			return true
		}
	}

	return false
}

// effectIDs is the per-session ID counter. It carries its own lock rather than
// using the control loop's, because AddEffect mints an ID on the caller's
// goroutine without touching the loop.
type effectIDs struct {
	mu   sync.Mutex
	next int
}
