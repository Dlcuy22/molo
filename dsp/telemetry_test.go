package dsp

import (
	"encoding/json"
	"testing"
)

// TestReadingKindJSONRoundTrips pins the wire form of every reading kind. The
// constants cross the JSON bridge, so a change to a string value is a breaking
// change to the frontend contract, not an internal rename.
func TestReadingKindJSONRoundTrips(t *testing.T) {
	for _, kind := range []ReadingKind{ReadingLevel, ReadingGainReduction, ReadingScalar} {
		if kind == "" {
			t.Fatalf("a ReadingKind constant is empty")
		}
		data, err := json.Marshal(kind)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", kind, err)
		}
		if string(data) != `"`+string(kind)+`"` {
			t.Fatalf("Marshal(%q) = %s, want the string form", kind, data)
		}
		var back ReadingKind
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("Unmarshal(%s): %v", data, err)
		}
		if back != kind {
			t.Fatalf("round trip of %q = %q", kind, back)
		}
	}
}

// TestVisualKindJSONRoundTrips pins the wire form of every visual kind, for the
// same reason as the reading kinds.
func TestVisualKindJSONRoundTrips(t *testing.T) {
	for _, kind := range []VisualKind{VisualTransfer, VisualGainReduction} {
		if kind == "" {
			t.Fatalf("a VisualKind constant is empty")
		}
		data, err := json.Marshal(kind)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", kind, err)
		}
		if string(data) != `"`+string(kind)+`"` {
			t.Fatalf("Marshal(%q) = %s, want the string form", kind, data)
		}
		var back VisualKind
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("Unmarshal(%s): %v", data, err)
		}
		if back != kind {
			t.Fatalf("round trip of %q = %q", kind, back)
		}
	}
}

// TestReadingAndVisualJSONTags pins the field names the frontend reads. A tag
// change here would silently reshape the wire, which is exactly what the
// bridge's explicit DTOs exist to prevent.
func TestReadingAndVisualJSONTags(t *testing.T) {
	data, err := json.Marshal(Reading{
		Key: "gr", Label: "Gain Reduction", Unit: "dB", Min: -30, Max: 0, Kind: ReadingGainReduction,
	})
	if err != nil {
		t.Fatalf("Marshal(Reading): %v", err)
	}
	for _, want := range []string{`"key"`, `"label"`, `"unit"`, `"min"`, `"max"`, `"kind"`} {
		if !containsJSONField(data, want) {
			t.Fatalf("Reading JSON %s is missing %s", data, want)
		}
	}

	vdata, err := json.Marshal(Visual{
		Kind: VisualTransfer, Params: []string{"threshold"}, Overlays: []string{"in"},
		XMin: -60, XMax: 0, YMin: -60, YMax: 0,
	})
	if err != nil {
		t.Fatalf("Marshal(Visual): %v", err)
	}
	for _, want := range []string{`"kind"`, `"params"`, `"overlays"`, `"xMin"`, `"xMax"`, `"yMin"`, `"yMax"`} {
		if !containsJSONField(vdata, want) {
			t.Fatalf("Visual JSON %s is missing %s", vdata, want)
		}
	}
}

// containsJSONField reports whether a marshalled object carries a field name.
func containsJSONField(data []byte, field string) bool {
	return len(data) > 0 && stringContains(string(data), field)
}

func stringContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
