package decode

import (
	"bytes"
	"slices"
	"testing"
)

// registeredFactory returns the registered factory with the given name, or
// fails. The registry keeps registration order, so a test can read the profiles
// the automatic selection machinery will actually consult.
func registeredFactory(t *testing.T, name string) Factory {
	t.Helper()

	for _, f := range Default.(*registry).factories {
		if f.Name() == name {
			return f
		}
	}
	t.Fatalf("factory %s is not registered", name)

	return nil
}

// TestOpusFactoryIsRegisteredWithProfile pins the registry contract for the
// pure-Go decoder: it claims Ogg Opus, exposes a profile, and carries the
// higher weight that makes it the automatic default over libopusfile.
func TestOpusFactoryIsRegisteredWithProfile(t *testing.T) {
	f := registeredFactory(t, "opus")
	if _, ok := f.(Profile); !ok {
		t.Fatal("opus does not implement Profile")
	}
	friendly, weight := ProfileOf(f)
	if friendly != "Portable" || weight != 90 {
		t.Fatalf("profile = %q/%d, want Portable/90", friendly, weight)
	}
	for _, ext := range []string{".opus", ".ogg"} {
		if !slices.Contains(f.Exts(), ext) {
			t.Fatalf("Exts() = %v, missing %s", f.Exts(), ext)
		}
	}
	if !f.Match([]byte("OggS\x00\x02")) {
		t.Fatal("Match rejected an OggS header")
	}
	if f.Match([]byte("RIFF....WAVE")) {
		t.Fatal("Match accepted a WAVE header")
	}
}

// TestOpusDefaultCodecIsThePureGoOne anchors the default: automatic selection
// opens the pure-Go decoder, and only it is marked Default among the Opus
// codecs. The exact variant is gone, so there is nothing left to keep below it.
func TestOpusDefaultCodecIsThePureGoOne(t *testing.T) {
	d, err := Default.Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Default.Open: %v", err)
	}
	defer d.Close()

	if _, ok := d.(*opusDecoder); !ok {
		t.Fatalf("Default.Open produced %T, want *opusDecoder", d)
	}

	seen := map[string]Codec{}
	for _, c := range Default.Codecs() {
		seen[c.Name] = c
	}
	for _, name := range []string{"opus", "opus-libopusfile"} {
		if _, ok := seen[name]; !ok {
			t.Fatalf("Codecs() is missing %s", name)
		}
	}
	if !seen["opus"].Default {
		t.Fatal("opus is not marked Default for .opus/.ogg")
	}
	if seen["opus"].Weight != 90 {
		t.Fatalf("opus weight = %d, want 90", seen["opus"].Weight)
	}
}

// rangeMaxAbs is the largest sample difference over [from, to) frames.
func rangeMaxAbs(a, b []float32, from, to int) float64 {
	var peak float64
	for i := from * 2; i < to*2 && i < len(a) && i < len(b); i++ {
		if d := abs64(float64(a[i] - b[i])); d > peak {
			peak = d
		}
	}

	return peak
}

// TestOpusSeekTransientIsBoundedAndConfined is the decoder's seek correctness
// property. An 80 ms warm-up cannot rebuild the CELT coarse-energy state
// exactly, so the samples right after the target differ from a straight decode;
// that transient is bounded and decays below a strict floor within the first
// 200 ms. It must not be bit-exact (that would mean the warm-up was skipped
// entirely) and must not leave a residual difference across the whole window.
func TestOpusSeekTransientIsBoundedAndConfined(t *testing.T) {
	const (
		windowFrames = 24000 // 500 ms read after each seek
		headFrames   = 4800  // 100 ms
		tailFrames   = 9600  // 200 ms
	)
	cases := []struct {
		fixture string
		at      int64
	}{
		{"stereo_2s.opus", 5000},
		{"stereo_2s.opus", 30000},
		{"stereo_2s.opus", 48000},
		{"mono_1s.opus", 30000},
	}
	for _, tc := range cases {
		t.Run(tc.fixture+"/"+itoa(tc.at), func(t *testing.T) {
			full := decodeFixture(t, tc.fixture)

			d, err := NewOpusFactory().Open(fixturePath(t, tc.fixture))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()
			if err := d.(Seeker).SeekFrame(tc.at); err != nil {
				t.Fatalf("SeekFrame(%d): %v", tc.at, err)
			}
			n := int(min(int64(windowFrames), int64(len(full))/2-tc.at))
			got := readExactlyFrames(t, d, n)
			want := full[tc.at*2 : (tc.at+int64(n))*2]

			headPeak := rangeMaxAbs(got, want, 0, min(headFrames, n))
			if headPeak == 0 {
				t.Fatal("seek was bit-exact; the 80 ms warm-up cannot reproduce a straight decode")
			}
			if headPeak > 0.2 {
				t.Fatalf("seek transient peak = %.4f, want a bounded click", headPeak)
			}

			// From 100 ms the difference must be far below the transient, and
			// by 200 ms it must be confined to a tiny floor.
			if mid := rangeMaxAbs(got, want, min(headFrames, n), n); mid > 0.01 {
				t.Fatalf("difference after 100 ms = %.5f, want the transient confined to the start", mid)
			}
			if tail := rangeMaxAbs(got, want, min(tailFrames, n), n); tail > 0.001 {
				t.Fatalf("difference after 200 ms = %.6f, want convergence", tail)
			}
			t.Logf("headPeak=%.5f", headPeak)
		})
	}
}

// TestOpusSeekSurvivesSeekSequence runs A -> B -> A on one decoder and compares
// each landing against a straight decode at that target. A decoder whose reused
// codec state leaks between seeks would diverge at the second A.
func TestOpusSeekSurvivesSeekSequence(t *testing.T) {
	full := decodeFixture(t, "stereo_2s.opus")
	d, err := NewOpusFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)

	// A -> B -> A, where B is far enough that stale state would show.
	for _, pass := range []int64{10000, 48000, 10000} {
		if err := seeker.SeekFrame(pass); err != nil {
			t.Fatalf("SeekFrame(%d): %v", pass, err)
		}
		const frames = 12000
		got := readExactlyFrames(t, d, frames)
		want := full[pass*2 : (pass+frames)*2]

		if peak := rangeMaxAbs(got, want, 0, frames); peak > 0.2 {
			t.Fatalf("seek %d: transient peak %.5f is unbounded", pass, peak)
		}
		if tail := rangeMaxAbs(got, want, 9600, frames); tail > 0.001 {
			t.Fatalf("seek %d: residual after 200 ms = %.6f; state leaked across seeks", pass, tail)
		}
	}

	// A final seek back to the head must reproduce the straight decode's head,
	// proving the reader and codec both reset rather than carrying the last
	// landing's state forward.
	if err := seeker.SeekFrame(0); err != nil {
		t.Fatalf("SeekFrame(0): %v", err)
	}
	head := readExactlyFrames(t, d, 2000)
	if !bytes.Equal(float32sToBytes(head), float32sToBytes(full[:2000*2])) {
		t.Fatal("seek back to the head did not restore the straight decode")
	}
}
