package molo

import "github.com/dlcuy22/molo/internal/session"

// CrossfadeSettings is the overlap between tracks: whether it is on, how long
// it runs, and whether two tracks from the same album are left unblended. It is
// a runtime setting, like the volume, so it can change while a track plays.
type CrossfadeSettings = session.CrossfadeSettings

// The crossfade duration bounds, re-exported so a UI can render the slider
// without importing internal/session.
const (
	MinCrossfadeMs     = session.MinCrossfadeMs
	MaxCrossfadeMs     = session.MaxCrossfadeMs
	DefaultCrossfadeMs = session.DefaultCrossfadeMs
)

// DefaultCrossfade returns the crossfade settings a fresh install starts with:
// off, at the default duration, with the same-album skip armed. A UI can show
// it as the reset value without hard-coding the numbers.
func DefaultCrossfade() CrossfadeSettings { return session.DefaultCrossfade() }

// Crossfade returns the crossfade settings in force.
func (p *player) Crossfade() CrossfadeSettings { return p.session.Crossfade() }

// SetCrossfade replaces the crossfade settings. An out-of-range duration is
// clamped rather than rejected, so a UI can send what it holds.
func (p *player) SetCrossfade(c CrossfadeSettings) error { return p.session.SetCrossfade(c) }
