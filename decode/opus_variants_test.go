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

// TestPionVariantsAreRegisteredWithDistinctProfiles pins the registry contract
// for the two pure-Go variants: both claim Ogg Opus, both expose a profile, and
// only the fast one carries the higher weight that keeps it the default. The
// exact variant is an explicit opt-in, not an automatic choice.
func TestPionVariantsAreRegisteredWithDistinctProfiles(t *testing.T) {
	cases := []struct {
		name     string
		friendly string
		weight   int
	}{
		{"opus-pion", "Portable", 90},
		{"opus-pion-exact", "Bit-perfect", 85},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := registeredFactory(t, tc.name)
			if _, ok := f.(Profile); !ok {
				t.Fatalf("%s does not implement Profile", tc.name)
			}
			friendly, weight := ProfileOf(f)
			if friendly != tc.friendly || weight != tc.weight {
				t.Fatalf("profile = %q/%d, want %q/%d", friendly, weight, tc.friendly, tc.weight)
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
		})
	}

	_, fastWeight := ProfileOf(registeredFactory(t, "opus-pion"))
	_, exactWeight := ProfileOf(registeredFactory(t, "opus-pion-exact"))
	if fastWeight <= exactWeight {
		t.Fatalf("fast weight %d must exceed exact weight %d so the default does not move", fastWeight, exactWeight)
	}
}

// TestOpenNamedPionVariants opens both variants by name and checks the warm-up
// window each one actually installs. The window is what separates the two, so
// asserting it on the decoder is the end-to-end proof, not just the metadata.
func TestOpenNamedPionVariants(t *testing.T) {
	cases := []struct {
		name     string
		friendly string
		warmup   int64
	}{
		{"opus-pion", "Portable", pionWarmupFast},
		{"opus-pion-exact", "Bit-perfect", pionWarmupExact},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Default.OpenNamed(tc.name, fixturePath(t, "stereo_2s.opus"))
			if err != nil {
				t.Fatalf("OpenNamed(%s): %v", tc.name, err)
			}
			defer d.Close()

			p, ok := d.(*pionOpusDecoder)
			if !ok {
				t.Fatalf("OpenNamed(%s) produced %T, want *pionOpusDecoder", tc.name, d)
			}
			if p.warmup != tc.warmup {
				t.Fatalf("%s warmup = %d, want %d", tc.name, p.warmup, tc.warmup)
			}
			if friendly, _ := ProfileOf(registeredFactory(t, tc.name)); friendly != tc.friendly {
				t.Fatalf("%s friendly name = %q, want %q", tc.name, friendly, tc.friendly)
			}
		})
	}
}

// TestPionDefaultCodecIsStillFast anchors the unchanged default: automatic
// selection opens the fast variant (80 ms window), and only it is marked
// Default among the Opus codecs.
func TestPionDefaultCodecIsStillFast(t *testing.T) {
	d, err := Default.Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Default.Open: %v", err)
	}
	defer d.Close()

	p, ok := d.(*pionOpusDecoder)
	if !ok {
		t.Fatalf("Default.Open produced %T, want *pionOpusDecoder", d)
	}
	if p.warmup != pionWarmupFast {
		t.Fatalf("default warmup = %d, want the fast %d", p.warmup, pionWarmupFast)
	}

	seen := map[string]Codec{}
	for _, c := range Default.Codecs() {
		seen[c.Name] = c
	}
	for _, name := range []string{"opus-pion", "opus-pion-exact", "opus-libopusfile"} {
		if _, ok := seen[name]; !ok {
			t.Fatalf("Codecs() is missing %s", name)
		}
	}
	if !seen["opus-pion"].Default {
		t.Fatal("opus-pion is not marked Default for .opus/.ogg")
	}
	if seen["opus-pion-exact"].Default {
		t.Fatal("opus-pion-exact is marked Default; the bit-perfect variant must stay an opt-in")
	}
	if seen["opus-pion"].Weight != 90 || seen["opus-pion-exact"].Weight != 85 {
		t.Fatalf("weights changed: fast=%d exact=%d", seen["opus-pion"].Weight, seen["opus-pion-exact"].Weight)
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

// TestPionFastSeekTransientIsBoundedAndConfined is the fast variant's
// correctness property. An 80 ms warm-up cannot rebuild the CELT coarse-energy
// state exactly, so the samples right after the target differ from a straight
// decode; that transient is bounded and decays below a strict floor within the
// first 200 ms. It must not be bit-exact (that would mean the exact warm-up was
// used) and must not leave a residual difference across the whole window.
func TestPionFastSeekTransientIsBoundedAndConfined(t *testing.T) {
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

			d, err := NewPionOpusFactory().Open(fixturePath(t, tc.fixture))
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
				t.Fatal("fast seek was bit-exact; the 80 ms warm-up cannot reproduce a straight decode")
			}
			if headPeak > 0.2 {
				t.Fatalf("fast transient peak = %.4f, want a bounded click", headPeak)
			}

			// From 100 ms the difference must be far below the transient, and
			// by 200 ms it must be confined to a tiny floor.
			if mid := rangeMaxAbs(got, want, min(headFrames, n), n); mid > 0.01 {
				t.Fatalf("fast difference after 100 ms = %.5f, want the transient confined to the start", mid)
			}
			if tail := rangeMaxAbs(got, want, min(tailFrames, n), n); tail > 0.001 {
				t.Fatalf("fast difference after 200 ms = %.6f, want convergence", tail)
			}
			t.Logf("headPeak=%.5f", headPeak)
		})
	}
}

// TestPionVariantsSurviveSeekSequence runs A -> B -> A on one decoder and
// compares each landing against a straight decode at that target. A decoder
// whose reused codec state leaks between seeks would diverge at the second A.
// The exact variant must reproduce the straight decode byte for byte; the fast
// variant must stay inside its confined transient.
func TestPionVariantsSurviveSeekSequence(t *testing.T) {
	cases := []struct {
		name  string
		fast  bool
		build func() Factory
	}{
		{"opus-pion", true, func() Factory { return NewPionOpusFactory() }},
		{"opus-pion-exact", false, func() Factory { return NewPionOpusExactFactory() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := decodeFixture(t, "stereo_2s.opus")
			d, err := tc.build().Open(fixturePath(t, "stereo_2s.opus"))
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

				if tc.fast {
					if peak := rangeMaxAbs(got, want, 0, frames); peak > 0.2 {
						t.Fatalf("seek %d: fast transient peak %.5f is unbounded", pass, peak)
					}
					if tail := rangeMaxAbs(got, want, 9600, frames); tail > 0.001 {
						t.Fatalf("seek %d: fast residual after 200 ms = %.6f; state leaked across seeks", pass, tail)
					}

					continue
				}
				if !bytes.Equal(float32sToBytes(got), float32sToBytes(want)) {
					t.Fatalf("seek %d: exact variant diverged from the straight decode; state leaked across seeks", pass)
				}
			}
		})
	}
}
