package player

import (
	"github.com/dlcuy22/player/internal/session"
)

// The editor errors are re-exported so a UI outside the module can tell a
// stale stage ID apart from an out-of-range move without importing
// internal/session. Match them with errors.Is.
var (
	// ErrUnknownEffect means an editor command named a stage that is not in the
	// chain in force, either because it was removed or because a rebuild is
	// still installing.
	ErrUnknownEffect = session.ErrUnknownEffect

	// ErrEffectIndexOutOfRange means MoveEffect named a position outside the
	// chain.
	ErrEffectIndexOutOfRange = session.ErrEffectIndexOutOfRange
)

// EffectKind is one registered effect implementation: the (kind, impl) pair a
// chooser offers and AddEffect accepts, plus the label and script flag a UI
// groups by.
type EffectKind = session.EffectKindInfo

// EffectStage is one stage of the chain in force, with its schema, current
// values, live meters and described readings and visual.
type EffectStage = session.EffectStage

// EffectChain is the chain in force in processing order. It is a value, not a
// live handle: mutating it cannot reach the engine, and the editor commands are
// the only way to change anything.
type EffectChain struct {
	Stages []EffectStage
}

// EffectMeters is one stage's live meters with no schema or values attached,
// for a UI that wants to drive a meter faster than the full snapshot.
type EffectMeters = session.EffectMeters

// Effects is the effect-chain editor surface. It is separate from Player so a
// fake that only exercises transport need not implement an editor it never
// uses, matching the engine's other capability interfaces (decode.Seeker,
// dsp.Metered, dsp.Latent).
//
// The kind accessor is named EffectKindList rather than EffectKinds because the
// Player interface already owns EffectKinds for the plain []string list. One
// type cannot carry two methods of the same name, and the plain list must keep
// working for the existing chooser, so the detailed list gets its own name.
// A UI that wants both reads Player.EffectKinds for the names and
// Effects.EffectKindList for the per-implementation detail.
type Effects interface {
	// EffectKindList lists every registered implementation, so a chooser can
	// offer each (kind, impl) pair.
	EffectKindList() []EffectKind

	// EffectChain returns the chain in force, in processing order.
	EffectChain() EffectChain

	// EffectMeters returns only the live meters of every stage that meters, for
	// a UI that drives a needle faster than the full snapshot. It reads no
	// schema and no values, so it is cheap enough for a high tick rate.
	EffectMeters() []EffectMeters

	// AddEffect appends a stage and returns its generated ID. The stage is
	// built on the caller's goroutine; the control loop installs the rebuild.
	AddEffect(kind, impl string) (string, error)

	// RemoveEffect drops the stage with id.
	RemoveEffect(id string) error

	// MoveEffect moves the stage with id to index to in processing order.
	MoveEffect(id string, to int) error

	// SetEffectParam applies one parameter to the live effect, without
	// rebuilding the chain, so effect state such as a filter's memory is not
	// discarded.
	SetEffectParam(id, key string, value any) error

	// SetEffectBypass toggles the standard bypass parameter of the live effect,
	// on the same no-rebuild path as any other parameter.
	SetEffectBypass(id string, bypassed bool) error
}

// Effects exposes the chain editor over this player. It returns the same
// underlying value as the Player, so the two surfaces share one engine.
func (p *player) Effects() Effects { return p }

var _ Effects = (*player)(nil)

// EffectKindList lists every registered effect implementation.
func (p *player) EffectKindList() []EffectKind { return p.session.EffectKinds() }

// EffectChain returns the chain in force in processing order.
func (p *player) EffectChain() EffectChain {
	return EffectChain{Stages: p.session.EffectStages()}
}

// EffectMeters returns only the live meters of every stage that meters.
func (p *player) EffectMeters() []EffectMeters { return p.session.EffectMeters() }

// AddEffect appends a stage to the chain and returns its generated ID.
func (p *player) AddEffect(kind, impl string) (string, error) {
	return p.session.AddEffect(kind, impl)
}

// RemoveEffect drops the stage with id from the chain.
func (p *player) RemoveEffect(id string) error { return p.session.RemoveEffect(id) }

// MoveEffect moves the stage with id to index to in processing order.
func (p *player) MoveEffect(id string, to int) error {
	return p.session.MoveEffect(id, to)
}

// SetEffectParam applies one parameter to the live effect without rebuilding
// the chain, so state that a rebuild would wipe is preserved.
func (p *player) SetEffectParam(id, key string, value any) error {
	return p.session.SetEffectParam(id, key, value)
}

// SetEffectBypass toggles the bypass parameter of the live effect.
func (p *player) SetEffectBypass(id string, bypassed bool) error {
	return p.session.SetEffectBypass(id, bypassed)
}
