package decode

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// profiledFactory adds a Profile to the shared fake so selection tests can set
// a label and a weight without pulling in a real codec.
type profiledFactory struct {
	*fakeFactory
	friendly string
	weight   int
}

func (f *profiledFactory) FriendlyName() string { return f.friendly }
func (f *profiledFactory) Weight() int          { return f.weight }

// TestProfileOfOpusFactories pins every Opus codec's UI labels and weights, so a
// change to any of them is a deliberate edit rather than a silent default flip.
func TestProfileOfOpusFactories(t *testing.T) {
	friendly, weight := ProfileOf(NewOpusFactory())
	if friendly != "Portable" || weight != 90 {
		t.Fatalf("opus profile = %q/%d, want Portable/90", friendly, weight)
	}

	friendly, weight = ProfileOf(NewLibopusfileFactory())
	if friendly != "Fastest" || weight != 80 {
		t.Fatalf("libopusfile profile = %q/%d, want Fastest/80", friendly, weight)
	}
}

// TestProfileOfWithoutProfileIsZero is the compatibility guarantee: a factory
// that omits the optional interface is not a failure, just a weight-0 entry.
func TestProfileOfWithoutProfileIsZero(t *testing.T) {
	friendly, weight := ProfileOf(&fakeFactory{name: "plain"})
	if friendly != "" || weight != 0 {
		t.Fatalf("ProfileOf on a plain factory = %q/%d, want empty/0", friendly, weight)
	}
}

func TestCodecsSortedByWeightThenRegistration(t *testing.T) {
	r := NewRegistry()
	newProfiled := func(name, friendly string, weight int, exts ...string) *profiledFactory {
		return &profiledFactory{
			fakeFactory: &fakeFactory{name: name, exts: exts},
			friendly:    friendly,
			weight:      weight,
		}
	}

	a := newProfiled("a", "A", 10, ".a")
	b := newProfiled("b", "B", 30, ".b", ".shared")
	c := newProfiled("c", "C", 30, ".shared")
	lo := newProfiled("lo", "Lo", 0, ".shared")
	r.Register(a)
	r.Register(b)
	r.Register(c)
	r.Register(lo)

	got := r.Codecs()
	want := []string{"b", "c", "a", "lo"}
	if names := codecNames(got); !slices.Equal(names, want) {
		t.Fatalf("Codecs order = %v, want %v", names, want)
	}

	// Default is per-extension, so more than one codec can win at once. b
	// claims .b alone and ties c on .shared; c wins the tie as the later
	// registration. "lo" claims .shared but loses it, so it is not a default.
	wantDefault := map[string]bool{"a": true, "b": true, "c": true, "lo": false}
	for _, c := range got {
		if c.Default != wantDefault[c.Name] {
			t.Fatalf("Codec %s Default = %v, want %v", c.Name, c.Default, wantDefault[c.Name])
		}
	}

	// FriendlyName is carried through from the Profile.
	if got[0].FriendlyName != "B" || got[0].Weight != 30 {
		t.Fatalf("Codecs[0] = %+v, want label B weight 30", got[0])
	}

	// Exts is a copy: mutating the returned slice must not reach the factory.
	got[0].Exts[0] = "MUTATED"
	if b.Exts()[0] != ".b" {
		t.Fatal("Codecs Exts aliases the factory's own slice")
	}
}

// TestAutomaticSelectionPrefersHigherWeight covers both registration orders.
// The second case is the load-bearing one: opus is registered first, so the
// old last-registration rule would pick libopusfile, and only weight makes
// opus win. Registering the native factory first matches the Default registry,
// where its init runs before the pure-Go one.
func TestAutomaticSelectionPrefersHigherWeight(t *testing.T) {
	cases := []struct {
		name  string
		order []Factory
	}{
		{"libopusfile first", []Factory{NewLibopusfileFactory(), NewOpusFactory()}},
		{"opus first", []Factory{NewOpusFactory(), NewLibopusfileFactory()}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			for _, f := range tt.order {
				r.Register(f)
			}

			d, err := r.Open(fixturePath(t, "stereo_2s.opus"))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer d.Close()

			if _, ok := d.(*opusDecoder); !ok {
				t.Fatalf("Open produced %T, want the higher-weight *opusDecoder", d)
			}
		})
	}
}

// TestAutomaticSelectionTieBreaksOnLastRegistration covers both selection
// paths. Equal weights must fall back to the last registration on the
// extension path and the magic path alike.
func TestAutomaticSelectionTieBreaksOnLastRegistration(t *testing.T) {
	t.Run("extension", func(t *testing.T) {
		dir := t.TempDir()
		// The bytes match neither factory, so byMagic stays nil and the
		// extension rule decides without the magic-contradiction fallback.
		path := writeFile(t, dir, "song.tst", []byte("QQQQ"))

		r := NewRegistry()
		first := &profiledFactory{fakeFactory: &fakeFactory{name: "first", exts: []string{".tst"}, magic: []byte("ZZZZ")}, weight: 50}
		second := &profiledFactory{fakeFactory: &fakeFactory{name: "second", exts: []string{".tst"}, magic: []byte("ZZZZ")}, weight: 50}
		r.Register(first)
		r.Register(second)

		if _, err := r.Open(path); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if first.opens != 0 || second.opens != 1 {
			t.Fatalf("opens first=%d second=%d, want 0 and 1", first.opens, second.opens)
		}
	})

	t.Run("magic", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "song.bin", []byte("ZZZZ payload"))

		r := NewRegistry()
		first := &profiledFactory{fakeFactory: &fakeFactory{name: "first", magic: []byte("ZZZZ")}, weight: 50}
		second := &profiledFactory{fakeFactory: &fakeFactory{name: "second", magic: []byte("ZZZZ")}, weight: 50}
		r.Register(first)
		r.Register(second)

		if _, err := r.Open(path); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if first.opens != 0 || second.opens != 1 {
			t.Fatalf("opens first=%d second=%d, want 0 and 1", first.opens, second.opens)
		}
	})
}

// TestAutomaticSelectionProfileBeatsNoProfile registers the weight-0 factory
// last so the old rule would pick it; a positive weight must win regardless of
// order.
func TestAutomaticSelectionProfileBeatsNoProfile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "song.tst", []byte("ZZZZ"))

	r := NewRegistry()
	strong := &profiledFactory{fakeFactory: &fakeFactory{name: "strong", exts: []string{".tst"}, magic: []byte("ZZZZ")}, weight: 50}
	plain := &fakeFactory{name: "plain", exts: []string{".tst"}, magic: []byte("ZZZZ")}
	r.Register(strong)
	r.Register(plain)

	if _, err := r.Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if strong.opens != 1 || plain.opens != 0 {
		t.Fatalf("opens strong=%d plain=%d, want 1 and 0", strong.opens, plain.opens)
	}
}

// TestOpenNamedForcesLowerWeightCodec is the whole point of the named path:
// opus outweighs libopusfile, yet naming the native codec must still open it.
func TestOpenNamedForcesLowerWeightCodec(t *testing.T) {
	requireLibopusfile(t)

	r := NewRegistry()
	r.Register(NewOpusFactory())
	r.Register(NewLibopusfileFactory())

	d, err := r.OpenNamed("opus-libopusfile", fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("OpenNamed: %v", err)
	}
	defer d.Close()

	if _, ok := d.(*libopusfileDecoder); !ok {
		t.Fatalf("OpenNamed produced %T, want *libopusfileDecoder", d)
	}
}

func TestOpenNamedEmptyMatchesOpen(t *testing.T) {
	r := NewRegistry()
	r.Register(NewLibopusfileFactory())
	r.Register(NewOpusFactory())

	auto, err := r.Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer auto.Close()

	named, err := r.OpenNamed("", fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("OpenNamed(\"\"): %v", err)
	}
	defer named.Close()

	_, autoOpus := auto.(*opusDecoder)
	_, namedOpus := named.(*opusDecoder)
	if !autoOpus || !namedOpus {
		t.Fatalf("types = %T and %T, want both *opusDecoder", auto, named)
	}
	if auto.Info() != named.Info() {
		t.Fatalf("Info = %+v and %+v, want identical", auto.Info(), named.Info())
	}
}

// TestOpenNamedUnknownCodecFailsBeforeFileAccess passes a path that does not
// exist: the unknown name must be rejected on its own, never by attempting to
// open the file.
func TestOpenNamedUnknownCodecFailsBeforeFileAccess(t *testing.T) {
	r := NewRegistry()
	r.Register(NewOpusFactory())
	r.Register(NewLibopusfileFactory())

	_, err := r.OpenNamed("nope", "does-not-exist.opus")
	if !errors.Is(err, ErrUnknownCodec) {
		t.Fatalf("OpenNamed error = %v, want ErrUnknownCodec", err)
	}
	for _, want := range []string{"opus", "opus-libopusfile"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name available codec %q", err, want)
		}
	}
}

func codecNames(codecs []Codec) []string {
	names := make([]string, len(codecs))
	for i, c := range codecs {
		names[i] = c.Name
	}

	return names
}
