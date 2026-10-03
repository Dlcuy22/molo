package main

import (
	"fmt"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/dsp"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// The effect window's bridge. It is a thin adapter over the facade's Effects
// surface, the same shape as the rest of this package: no audio logic, only the
// translation between the engine's types and the JSON the frontend reads.
//
// The DTOs carry explicit json tags rather than reusing dsp.Param directly,
// because dsp.Param has no tags and the frontend contract (effect-types.ts)
// names every field in lower camel case. Keeping the mapping here means a change
// to dsp.Param's field order cannot silently re-shape the wire.

// EffectParamInfo is one parameter description for a control to render.
type EffectParamInfo struct {
	Key     string   `json:"key"`
	Kind    int      `json:"kind"`
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Step    float64  `json:"step"`
	Unit    string   `json:"unit"`
	Options []string `json:"options"`
	Default any      `json:"default"`
	Label   string   `json:"label"`
	Group   string   `json:"group"`
	Widget  string   `json:"widget"`
}

// EffectKindInfo is one effect a chooser can offer.
type EffectKindInfo struct {
	Kind     string `json:"kind"`
	Impl     string `json:"impl"`
	Label    string `json:"label"`
	Weight   int    `json:"weight"`
	Scripted bool   `json:"scripted"`
}

// EffectReadingInfo describes one key that Meters reports, so a reading can be
// drawn without the frontend knowing which effect produced it.
type EffectReadingInfo struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Unit  string  `json:"unit"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Kind  string  `json:"kind"`
}

// EffectVisualInfo declares a plot. Params are schema keys the curve is derived
// from; Overlays are reading keys drawn live on top.
type EffectVisualInfo struct {
	Kind     string   `json:"kind"`
	Params   []string `json:"params"`
	Overlays []string `json:"overlays"`
	XMin     float64  `json:"xMin"`
	XMax     float64  `json:"xMax"`
	YMin     float64  `json:"yMin"`
	YMax     float64  `json:"yMax"`
}

// EffectStageInfo is one stage in the chain.
type EffectStageInfo struct {
	ID       string              `json:"id"`
	Kind     string              `json:"kind"`
	Impl     string              `json:"impl"`
	Label    string              `json:"label"`
	Bypassed bool                `json:"bypassed"`
	Schema   []EffectParamInfo   `json:"schema"`
	Values   map[string]any      `json:"values"`
	Meters   map[string]float32  `json:"meters"`
	Readings []EffectReadingInfo `json:"readings"`
	Visuals  []EffectVisualInfo  `json:"visuals"`
}

// EffectChainInfo is the whole chain in processing order.
type EffectChainInfo struct {
	Stages []EffectStageInfo `json:"stages"`
}

// EffectMetersInfo is one stage's live meters with no schema or values. It is
// the payload of the fast meter tick, kept lean so a ~30 Hz read never re-walks
// the parameters the 4 Hz snapshot already carries.
type EffectMetersInfo struct {
	ID     string             `json:"id"`
	Meters map[string]float32 `json:"meters"`
}

// effects returns the editor surface, or an error when the engine was not built
// yet. It mirrors the nil checks the other bound methods use.
func (s *PlayerService) effects() (player.Effects, error) {
	if s.player == nil {
		return nil, fmt.Errorf("no player")
	}

	return s.player, nil
}

// EffectKinds lists every registered effect implementation, so the registry tab
// can offer each (kind, impl) pair with its label and script flag.
func (s *PlayerService) EffectKinds() ([]EffectKindInfo, error) {
	fx, err := s.effects()
	if err != nil {
		return nil, err
	}

	kinds := fx.EffectKindList()
	out := make([]EffectKindInfo, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, EffectKindInfo{
			Kind:     k.Kind,
			Impl:     k.Impl,
			Label:    k.Label,
			Weight:   k.Weight,
			Scripted: k.Scripted,
		})
	}

	return out, nil
}

// EffectChain returns the chain in force with each stage's schema, values and
// live meters. It is read on the snapshot tick, so the panel and its meters
// update without a second subscription.
func (s *PlayerService) EffectChain() (EffectChainInfo, error) {
	fx, err := s.effects()
	if err != nil {
		return EffectChainInfo{}, err
	}

	chain := fx.EffectChain()
	out := EffectChainInfo{Stages: make([]EffectStageInfo, 0, len(chain.Stages))}
	for _, st := range chain.Stages {
		out.Stages = append(out.Stages, effectStageInfo(st))
	}

	return out, nil
}

// EffectMeters returns only the live meters of every stage that meters. It is
// the fast tick's read: no schema, no values, so a moving gain-reduction needle
// can update at ~30 Hz without the cost of the full chain snapshot.
func (s *PlayerService) EffectMeters() ([]EffectMetersInfo, error) {
	fx, err := s.effects()
	if err != nil {
		return nil, err
	}

	meters := fx.EffectMeters()
	out := make([]EffectMetersInfo, 0, len(meters))
	for _, m := range meters {
		out = append(out, EffectMetersInfo{ID: m.ID, Meters: m.Meters})
	}

	return out, nil
}

// AddEffect appends a stage and returns its ID. The stage is live when this
// returns, so the caller can set a parameter on it immediately.
func (s *PlayerService) AddEffect(kind, impl string) (string, error) {
	fx, err := s.effects()
	if err != nil {
		return "", err
	}

	return s.commandID(func() (string, error) { return fx.AddEffect(kind, impl) })
}

// RemoveEffect drops the stage with id.
func (s *PlayerService) RemoveEffect(id string) error {
	fx, err := s.effects()
	if err != nil {
		return err
	}

	return s.command(func() error { return fx.RemoveEffect(id) })
}

// MoveEffect moves the stage with id to index to in processing order. The index
// is a final index in the resulting list, matching the engine.
func (s *PlayerService) MoveEffect(id string, to int) error {
	fx, err := s.effects()
	if err != nil {
		return err
	}

	return s.command(func() error { return fx.MoveEffect(id, to) })
}

// SetEffectParam applies one parameter to the live effect. It does not rebuild
// the chain, so the effect's state survives the change.
func (s *PlayerService) SetEffectParam(id, key string, value any) error {
	fx, err := s.effects()
	if err != nil {
		return err
	}

	return s.command(func() error { return fx.SetEffectParam(id, key, value) })
}

// SetEffectBypass toggles the standard bypass parameter of the live effect.
func (s *PlayerService) SetEffectBypass(id string, bypassed bool) error {
	fx, err := s.effects()
	if err != nil {
		return err
	}

	return s.command(func() error { return fx.SetEffectBypass(id, bypassed) })
}

// OpenEffectWindow shows the effect window from the main window. The window is
// a second view over this same service, so the chain and its meters are one
// state seen twice rather than two engines.
func (s *PlayerService) OpenEffectWindow() error {
	s.openEffectWindow()

	return nil
}

// EffectWindowOpen reports whether the effect window exists, so the main window
// can label its button.
func (s *PlayerService) EffectWindowOpen() bool {
	app := application.Get()
	if app == nil {
		return false
	}
	_, ok := app.Window.GetByName(effectWindowName)

	return ok
}

// effectStageInfo maps one engine stage to its wire shape.
func effectStageInfo(st player.EffectStage) EffectStageInfo {
	schema := make([]EffectParamInfo, 0, len(st.Schema))
	for _, p := range st.Schema {
		schema = append(schema, effectParamInfo(p))
	}

	readings := make([]EffectReadingInfo, 0, len(st.Readings))
	for _, r := range st.Readings {
		readings = append(readings, effectReadingInfo(r))
	}

	visuals := make([]EffectVisualInfo, 0, len(st.Visuals))
	for _, v := range st.Visuals {
		visuals = append(visuals, effectVisualInfo(v))
	}

	return EffectStageInfo{
		ID:       st.ID,
		Kind:     st.Kind,
		Impl:     st.Impl,
		Label:    st.Label,
		Bypassed: st.Bypassed,
		Schema:   schema,
		Values:   st.Values,
		Meters:   st.Meters,
		Readings: readings,
		Visuals:  visuals,
	}
}

// effectReadingInfo maps one engine reading. Kind is kept as the dsp constant's
// string value rather than its iota order, matching how Widget crosses the wire.
func effectReadingInfo(r dsp.Reading) EffectReadingInfo {
	return EffectReadingInfo{
		Key:   r.Key,
		Label: r.Label,
		Unit:  r.Unit,
		Min:   r.Min,
		Max:   r.Max,
		Kind:  string(r.Kind),
	}
}

// effectVisualInfo maps one engine visual. Params and Overlays are normalized
// to empty slices rather than nil so the frontend always sees arrays.
func effectVisualInfo(v dsp.Visual) EffectVisualInfo {
	params := v.Params
	if params == nil {
		params = []string{}
	}
	overlays := v.Overlays
	if overlays == nil {
		overlays = []string{}
	}

	return EffectVisualInfo{
		Kind:     string(v.Kind),
		Params:   params,
		Overlays: overlays,
		XMin:     v.XMin,
		XMax:     v.XMax,
		YMin:     v.YMin,
		YMax:     v.YMax,
	}
}

// effectParamInfo maps one engine parameter. Options is normalized to an empty
// slice rather than nil so the frontend never has to tell the two apart.
func effectParamInfo(p dsp.Param) EffectParamInfo {
	options := p.Options
	if options == nil {
		options = []string{}
	}

	return EffectParamInfo{
		Key:     p.Key,
		Kind:    int(p.Kind),
		Min:     p.Min,
		Max:     p.Max,
		Step:    p.Step,
		Unit:    p.Unit,
		Options: options,
		Default: p.Default,
		Label:   p.Label,
		Group:   p.Group,
		Widget:  string(p.Widget),
	}
}
