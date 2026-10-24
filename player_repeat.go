package molo

// Repeat returns the repeat mode in force.
func (p *player) Repeat() RepeatMode { return p.session.Repeat() }

// SetRepeat replaces the repeat mode. An unknown mode falls back to RepeatOff
// rather than being rejected, so a UI can send what it holds.
func (p *player) SetRepeat(m RepeatMode) error { return p.session.SetRepeat(m) }
