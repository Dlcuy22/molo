package session

// Crossfade bounds. The duration is the overlap window in milliseconds: the
// outgoing track fades out while the incoming one fades in. The 1 to 12 second
// range is the one most players expose, so a listener finds the same control.
const (
	// MinCrossfadeMs is the shortest accepted overlap.
	MinCrossfadeMs = 1000
	// MaxCrossfadeMs is the longest accepted overlap, twelve seconds.
	MaxCrossfadeMs = 12000

	// DefaultCrossfadeMs is the starting duration when none was chosen: long
	// enough to read as a blend, short enough not to swallow a song's end.
	DefaultCrossfadeMs = 5000
)

// CrossfadeSettings is the overlap between tracks. It is a runtime setting,
// like Volume: it can change while a track plays and applies to the next
// handoff.
//
// Enabled turns the overlap on. DurationMs is the overlap length in
// milliseconds, bounded by MinCrossfadeMs and MaxCrossfadeMs. SkipSameAlbum
// suppresses the overlap for two consecutive tracks from the same album, so a
// continuous album is not cut by a blend.
type CrossfadeSettings struct {
	Enabled       bool
	DurationMs    int
	SkipSameAlbum bool
}

// DefaultCrossfade returns the settings in force before a caller changes
// anything: off, with the default duration and same-album skip armed for the
// moment it is switched on. A fresh install therefore behaves exactly as it
// did before the feature existed.
func DefaultCrossfade() CrossfadeSettings {
	return CrossfadeSettings{
		Enabled:       false,
		DurationMs:    DefaultCrossfadeMs,
		SkipSameAlbum: true,
	}
}

// ValidateCrossfade returns the settings with the duration clamped into the
// accepted range. A duration outside it is corrected rather than rejected,
// matching how the gain clamps a bad volume: the nearest bound is always a
// usable value.
func ValidateCrossfade(c CrossfadeSettings) CrossfadeSettings {
	switch {
	case c.DurationMs < MinCrossfadeMs:
		c.DurationMs = MinCrossfadeMs
	case c.DurationMs > MaxCrossfadeMs:
		c.DurationMs = MaxCrossfadeMs
	}

	return c
}
