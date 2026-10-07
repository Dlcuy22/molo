package discordrpc

import "time"

// activity is one Rich Presence card, in the terms the UI thinks in. It is
// mapped to the wire shape by wire(), which is the only place the Discord field
// names appear.
type activity struct {
	// Type is Discord's verb: 2 Listening, 0 Playing, 3 Watching.
	Type int
	// Name is the word after the verb in the card header, e.g. the artist in
	// "Listening to <artist>". An empty Name lets Discord fall back to the
	// application name.
	Name string
	// Details is the first body line, the track title.
	Details string
	// State is the second body line, the artist.
	State string

	// LargeImage is the album cover: an https URL or a registered asset key.
	// LargeText is its hover tooltip, usually the album.
	LargeImage string
	LargeText  string
	// SmallImage is the artist avatar, drawn as the badge over the cover.
	SmallImage string
	SmallText  string

	// Timestamps drive the progress bar. They are nil when the clock is frozen,
	// so Discord draws no moving bar.
	Timestamps *timestamps
	// Buttons are the clickable links under the card, at most two.
	Buttons []Button
}

// timestamps is the card's clock: when the track started and when it ends.
type timestamps struct {
	Start *time.Time
	End   *time.Time
}

// wireActivity is the Discord SET_ACTIVITY activity object. The field names and
// omission rules match the RPC schema: a zero value is dropped rather than sent
// empty, because Discord rejects some empty strings and ignores others.
type wireActivity struct {
	Type       int             `json:"type,omitempty"`
	Name       string          `json:"name,omitempty"`
	Details    string          `json:"details,omitempty"`
	State      string          `json:"state,omitempty"`
	Assets     *wireAssets     `json:"assets,omitempty"`
	Timestamps *wireTimestamps `json:"timestamps,omitempty"`
	Buttons    []*wireButton   `json:"buttons,omitempty"`
}

type wireAssets struct {
	LargeImage string `json:"large_image,omitempty"`
	LargeText  string `json:"large_text,omitempty"`
	SmallImage string `json:"small_image,omitempty"`
	SmallText  string `json:"small_text,omitempty"`
}

type wireTimestamps struct {
	Start *uint64 `json:"start,omitempty"`
	End   *uint64 `json:"end,omitempty"`
}

type wireButton struct {
	Label string `json:"label,omitempty"`
	URL   string `json:"url,omitempty"`
}

// wire renders the card for the wire, or nil to clear the presence. A nil
// receiver is the "clear" case, which is how an empty queue is sent.
func (a *activity) wire() *wireActivity {
	if a == nil {
		return nil
	}

	out := &wireActivity{
		Type:    a.Type,
		Name:    a.Name,
		Details: a.Details,
		State:   a.State,
	}

	// The assets block is omitted entirely when there is no image, so Discord
	// does not render an empty hover box.
	if a.LargeImage != "" || a.SmallImage != "" {
		out.Assets = &wireAssets{
			LargeImage: a.LargeImage,
			LargeText:  a.LargeText,
			SmallImage: a.SmallImage,
			SmallText:  a.SmallText,
		}
	}

	if a.Timestamps != nil {
		out.Timestamps = &wireTimestamps{}
		if a.Timestamps.Start != nil {
			start := uint64(a.Timestamps.Start.UnixMilli())
			out.Timestamps.Start = &start
		}
		if a.Timestamps.End != nil {
			end := uint64(a.Timestamps.End.UnixMilli())
			out.Timestamps.End = &end
		}
	}

	for _, b := range a.Buttons {
		if b.Label == "" || b.URL == "" {
			continue
		}
		out.Buttons = append(out.Buttons, &wireButton{Label: b.Label, URL: b.URL})
	}

	return out
}
