package tui

import (
	"strings"
	"testing"
)

// TestCodecLabelFormat pins the row format: a family prefix from the registry
// name, then the friendly name, so variants of one codec read as a group.
func TestCodecLabelFormat(t *testing.T) {
	cases := []struct {
		reg, friendly string
		want          string
	}{
		// The shipped Opus and FLAC rows, which is the whole point of the
		// family prefix: three Opus variants sort visually under "Opus".
		{"opus-pion", "Portable", "Opus Portable"},
		{"opus-pion-exact", "Bit-perfect", "Opus Bit-perfect"},
		{"opus-libopusfile", "Fastest", "Opus Fastest"},
		{"flac", "Lossless", "Flac Lossless"},
		// A name that is its own family must not repeat itself.
		{"alpha", "Alpha", "Alpha"},
		{"beta", "Beta", "Beta"},
		// Case differences must not produce a duplicate either.
		{"flac", "flac", "Flac"},
		// No profile: the registry name is the whole label, family-titled.
		{"alpha", "", "Alpha"},
	}

	for _, tc := range cases {
		if got := codecLabelFor(tc.reg, tc.friendly, false); got != tc.want {
			t.Errorf("codecLabelFor(%q, %q, false) = %q, want %q", tc.reg, tc.friendly, got, tc.want)
		}
	}
}

// TestCodecLabelRegistryNameToggle pins the developer switch: the same row with
// the suffix on must carry the registry name, because that is the string a
// config file and the debug panel use.
func TestCodecLabelRegistryNameToggle(t *testing.T) {
	cases := []struct {
		reg, friendly, want string
	}{
		{"opus-libopusfile", "Fastest", "Opus Fastest (opus-libopusfile)"},
		{"flac", "Lossless", "Flac Lossless (flac)"},
		// Even a self-named entry gets the suffix, so the toggle is uniform and
		// a test can always read the registry name back.
		{"alpha", "Alpha", "Alpha (alpha)"},
	}

	for _, tc := range cases {
		if got := codecLabelFor(tc.reg, tc.friendly, true); got != tc.want {
			t.Errorf("codecLabelFor(%q, %q, true) = %q, want %q", tc.reg, tc.friendly, got, tc.want)
		}
	}
}

// TestCodecLabelAutoIsUnchanged guards the one row the format does not apply to.
func TestCodecLabelAutoIsUnchanged(t *testing.T) {
	for _, show := range []bool{false, true} {
		if got := codecLabelFor("", "", show); got != pickerAutoLabel {
			t.Fatalf("auto label with show=%v = %q, want %q", show, got, pickerAutoLabel)
		}
	}
}

// TestCodecPickerRowsShowFamilyPrefix proves the picker renders the new format,
// not just that the helper does.
func TestCodecPickerRowsShowFamilyPrefix(t *testing.T) {
	m := openPicker(t, newFakePlayer())

	frame := stripANSI(m.render())
	for _, want := range []string{"Alpha", "Beta"} {
		if !strings.Contains(frame, want) {
			t.Errorf("picker missing row %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "Alpha (alpha)") {
		t.Errorf("picker shows the registry suffix while the constant is off:\n%s", frame)
	}
}
