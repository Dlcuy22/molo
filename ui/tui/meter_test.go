package tui

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestRMS(t *testing.T) {
	cases := []struct {
		name string
		in   []float32
		want float64
	}{
		{"empty", nil, 0},
		{"silence", []float32{0, 0, 0, 0}, 0},
		{"full scale square", []float32{1, -1, 1, -1}, 1},
		{"half scale square", []float32{0.5, -0.5, 0.5, -0.5}, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rms(tc.in)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("rms(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMeterDecaysRatherThanSnapping(t *testing.T) {
	t0 := time.Unix(0, 0)

	var m meter
	m.push(1, t0)
	if m.level != 1 {
		t.Fatalf("first push = %v, want 1", m.level)
	}

	// Silence must fall gradually, not jump to zero: a meter that snaps looks
	// like a glitch, and the release is the one piece of smoothing the UI does.
	// The time-based shape is pinned in TestMeterReleaseIsTimeBased; here it is
	// enough that a quiet reading decays partway rather than holding or snapping.
	m.push(0, t0.Add(meterHalfLife/2))
	if m.level <= 0 || m.level >= 1 {
		t.Fatalf("release = %v, want strictly between 0 and 1", m.level)
	}
}

func TestMeterResets(t *testing.T) {
	var m meter
	m.push(1, time.Time{})
	m.reset()
	if m.level != 0 {
		t.Fatalf("reset level = %v, want 0", m.level)
	}
}

func TestMeterPushClamps(t *testing.T) {
	var m meter
	m.push(2, time.Time{})
	if m.level != 1 {
		t.Fatalf("push above one = %v, want 1", m.level)
	}
	m.push(-1, time.Time{})
	if m.level < 0 {
		t.Fatalf("push below zero produced %v", m.level)
	}
}

// TestMeterBarsMonotonic is the load-bearing meter assertion: a louder sample
// must never draw fewer bars, and silence must draw none.
func TestMeterBarsMonotonic(t *testing.T) {
	const width = 20

	silent := strings.Count(meterBars(0, width), meterFull)
	quiet := strings.Count(meterBars(0.01, width), meterFull)
	loud := strings.Count(meterBars(0.5, width), meterFull)

	if silent != 0 {
		t.Fatalf("silence drew %d bars: %q", silent, meterBars(0, width))
	}
	if !(quiet <= loud) {
		t.Fatalf("louder sample drew fewer bars: quiet=%d loud=%d", quiet, loud)
	}
	if loud == 0 {
		t.Fatalf("0.5 RMS drew no bars: %q", meterBars(0.5, width))
	}
}

func TestMeterBarsWidth(t *testing.T) {
	for _, width := range []int{0, 1, 5, 20} {
		got := []rune(meterBars(1, width))
		if len(got) != width {
			t.Errorf("meterBars(width=%d) rendered %d runes: %q", width, len(got), string(got))
		}
	}
	// A negative RMS (impossible from rms, but a defensive path) must not panic
	// and must render the same width.
	if got := []rune(meterBars(-1, 5)); len(got) != 5 {
		t.Errorf("negative level rendered %d runes", len(got))
	}
}
