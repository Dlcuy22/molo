package decode

import (
	"slices"
	"testing"

	"github.com/dlcuy22/player/core"
)

// thirdFactory is a stand-in for a future codec. It only has to implement the
// Factory contract plus Register in init; nothing in the registry changes.
type thirdFactory struct{}

func (thirdFactory) Name() string   { return "third" }
func (thirdFactory) Exts() []string { return []string{".third"} }

func (thirdFactory) Match(magic []byte) bool {
	return len(magic) >= 4 && string(magic[:4]) == "THRD"
}

func (thirdFactory) Open(path string) (Decoder, error) {
	return &fakeDecoder{info: fakeStreamInfo()}, nil
}

func init() {
	Register(thirdFactory{})
}

func TestRegisteredDecodersShareOneContract(t *testing.T) {
	// Every registered factory must satisfy Factory; Prober is optional, which
	// is why Registry.Probe reports a bool. This test fails if a future codec
	// is added with an inconsistent shape.
	factories := Default.(*registry).factories
	if len(factories) < 3 {
		t.Fatalf("only %d factories registered, want at least 3", len(factories))
	}
	for _, f := range factories {
		if _, ok := f.(Factory); !ok {
			t.Fatalf("%s does not implement Factory", f.Name())
		}
		if f.Name() == "" || len(f.Exts()) == 0 {
			t.Fatalf("%s has no name or extensions", f.Name())
		}
	}
	for _, want := range []string{"opus-pion", "opus-pion-exact", "opus-libopusfile"} {
		if !slices.ContainsFunc(factories, func(f Factory) bool { return f.Name() == want }) {
			t.Fatalf("factory %s is not registered", want)
		}
	}
}

func TestDefaultRegistryDispatchesOggOpus(t *testing.T) {
	d, err := Default.Open(fixturePath(t, "short_stereo.opus"))
	if err != nil {
		t.Fatalf("Default.Open: %v", err)
	}
	defer d.Close()

	info := d.Info()
	if want := (core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}); info.Format != want {
		t.Fatalf("Format = %+v, want %+v", info.Format, want)
	}
}

func TestDefaultRegistryPrefersPionByHigherWeight(t *testing.T) {
	// Both factories claim Ogg Opus. The pure-Go one wins automatic selection
	// by weight (90 over 80), not by registration order, because it has no
	// runtime library dependency. Anchoring on the resulting decoder means a
	// weight change cannot silently flip the default.
	d, err := Default.Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Default.Open: %v", err)
	}
	defer d.Close()

	if _, isPion := d.(*pionOpusDecoder); !isPion {
		t.Fatalf("Default.Open produced %T, want the pure-Go *pionOpusDecoder", d)
	}
}

func TestDefaultRegistryProbeUsesOggTail(t *testing.T) {
	prober, ok := Default.Probe(fixturePath(t, "stereo_2s.opus"))
	if !ok {
		t.Fatal("Default registry found no prober for an .opus fixture")
	}
	info, err := prober.Probe(fixturePath(t, "stereo_2s.opus"), ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != 96000 {
		t.Fatalf("TotalFrames = %d, want 96000", info.TotalFrames)
	}
}

func TestSupportedIncludesOpusExtensions(t *testing.T) {
	got := Default.Supported()
	for _, ext := range []string{".ogg", ".opus", ".third"} {
		if !slices.Contains(got, ext) {
			t.Fatalf("Supported() = %v, missing %s", got, ext)
		}
	}
	if !slices.IsSorted(got) {
		t.Fatalf("Supported() = %v, want sorted", got)
	}
}

func TestThirdFactoryIsDispatchedByMagic(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mystery.bin", []byte("THRD payload"))

	d, err := Default.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
}
