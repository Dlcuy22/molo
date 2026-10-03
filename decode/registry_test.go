package decode

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dlcuy22/molo/core"
)

func writeFile(t *testing.T, dir, name string, body []byte) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

type fakeDecoder struct {
	info core.StreamInfo
}

func (d *fakeDecoder) Info() core.StreamInfo { return d.info }

func (d *fakeDecoder) ReadFrames(dst []float32) (int, error) { return 0, io.EOF }

func (d *fakeDecoder) Close() error { return nil }

func fakeStreamInfo() core.StreamInfo {
	return core.StreamInfo{
		Format:      core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		TotalFrames: -1,
	}
}

// fakeFactory claims a fixed extension set and magic prefix without touching
// the filesystem, so registry dispatch can be tested in isolation.
type fakeFactory struct {
	name  string
	exts  []string
	magic []byte
	opens int
}

func (f *fakeFactory) Name() string   { return f.name }
func (f *fakeFactory) Exts() []string { return f.exts }

func (f *fakeFactory) Match(magic []byte) bool {
	return len(magic) >= len(f.magic) && slices.Equal(magic[:len(f.magic)], f.magic)
}

func (f *fakeFactory) Open(path string) (Decoder, error) {
	f.opens++

	return &fakeDecoder{info: fakeStreamInfo()}, nil
}

func TestRegistryOpenByExtension(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "song.tst", []byte("ZZZZnot-a-real-file"))

	r := NewRegistry()
	f := &fakeFactory{name: "tst", exts: []string{".tst"}, magic: []byte("ZZZZ")}
	r.Register(f)

	dec, err := r.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer dec.Close()

	if f.opens != 1 {
		t.Fatalf("factory opened %d times, want 1", f.opens)
	}
}

func TestRegistryOpenExtensionIsCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "SONG.TST", []byte("ZZZZ"))

	r := NewRegistry()
	f := &fakeFactory{name: "tst", exts: []string{".tst"}, magic: []byte("ZZZZ")}
	r.Register(f)

	if _, err := r.Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if f.opens != 1 {
		t.Fatalf("factory opened %d times, want 1", f.opens)
	}
}

func TestRegistryOpenByMagicWhenExtensionUnknown(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "noext", []byte("ZZZZ payload"))

	r := NewRegistry()
	f := &fakeFactory{name: "tst", exts: []string{".tst"}, magic: []byte("ZZZZ")}
	r.Register(f)

	if _, err := r.Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if f.opens != 1 {
		t.Fatalf("factory opened %d times, want 1", f.opens)
	}
}

func TestRegistryMagicBeatsExtension(t *testing.T) {
	dir := t.TempDir()
	// The name claims ".ext" but the bytes belong to the "magic" factory;
	// trusting the extension here would route a foreign container to the wrong
	// decoder.
	path := writeFile(t, dir, "mislabelled.ext", []byte("ZZZZ payload"))

	r := NewRegistry()
	byExt := &fakeFactory{name: "ext", exts: []string{".ext"}, magic: []byte("QQQQ")}
	byMagic := &fakeFactory{name: "magic", exts: []string{".other"}, magic: []byte("ZZZZ")}
	r.Register(byExt)
	r.Register(byMagic)

	if _, err := r.Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if byExt.opens != 0 {
		t.Fatal("extension-matching factory was used despite contradicting magic")
	}
	if byMagic.opens != 1 {
		t.Fatalf("magic-matching factory opened %d times, want 1", byMagic.opens)
	}
}

func TestRegistryOpenUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "junk.bin", []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07})

	r := NewRegistry()
	r.Register(&fakeFactory{name: "tst", exts: []string{".tst"}, magic: []byte("ZZZZ")})

	_, err := r.Open(path)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open error = %v, want ErrUnsupported", err)
	}
}

func TestRegistryOpenMissingFile(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{name: "tst", exts: []string{".tst"}, magic: []byte("ZZZZ")})

	_, err := r.Open("does-not-exist.tst")
	if err == nil {
		t.Fatal("Open of a missing file returned nil error")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing file reported as unsupported: %v", err)
	}
}

func TestRegistrySniffStaysBounded(t *testing.T) {
	dir := t.TempDir()
	// A large body behind the magic prefix: sniffing must read a bounded window
	// instead of pulling the whole file in on every extensionless open.
	body := append([]byte("ZZZZ"), make([]byte, 1<<20)...)
	path := writeFile(t, dir, "big.bin", body)

	r := NewRegistry()
	f := &countingFactory{fakeFactory: fakeFactory{name: "tst", magic: []byte("ZZZZ")}}
	r.Register(f)

	if _, err := r.Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if f.magicSeen == 0 {
		t.Fatal("magic probe never ran")
	}
	if f.magicSeen > sniffWindow {
		t.Fatalf("magic probe saw %d bytes, want at most %d", f.magicSeen, sniffWindow)
	}
}

type countingFactory struct {
	fakeFactory
	magicSeen int
}

func (f *countingFactory) Match(magic []byte) bool {
	f.magicSeen = len(magic)

	return f.fakeFactory.Match(magic)
}

func TestRegistrySupportedIsSortedAndStable(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{name: "zeta", exts: []string{".z", ".a"}})
	r.Register(&fakeFactory{name: "alpha", exts: []string{".m"}})
	r.Register(&fakeFactory{name: "mid", exts: []string{".q"}})

	got := r.Supported()
	want := []string{".a", ".m", ".q", ".z"}
	if !slices.Equal(got, want) {
		t.Fatalf("Supported() = %v, want %v", got, want)
	}
	if again := r.Supported(); !slices.Equal(again, want) {
		t.Fatalf("Supported() unstable: %v", again)
	}
}

func TestRegistryLastRegistrationWinsForSameExtension(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "song.tst", []byte("ZZZZ"))

	r := NewRegistry()
	first := &fakeFactory{name: "first", exts: []string{".tst"}, magic: []byte("ZZZZ")}
	second := &fakeFactory{name: "second", exts: []string{".tst"}, magic: []byte("ZZZZ")}
	r.Register(first)
	r.Register(second)

	if _, err := r.Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if first.opens != 0 || second.opens != 1 {
		t.Fatalf("opens first=%d second=%d, want 0 and 1", first.opens, second.opens)
	}
	if got := r.Supported(); !slices.Equal(got, []string{".tst"}) {
		t.Fatalf("Supported() = %v, want one entry", got)
	}
}

func TestRegistryProbeReturnsRegisteredProber(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "song.tst", []byte("ZZZZ"))

	r := NewRegistry()
	r.Register(&proberFactory{fakeFactory{name: "tst", exts: []string{".tst"}, magic: []byte("ZZZZ")}})

	prober, ok := r.Probe(path)
	if !ok {
		t.Fatal("Probe() found no prober for a registered factory")
	}
	info, err := prober.Probe(path, ProbeOptions{Duration: core.DurationProbe})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.TotalFrames != 48000 {
		t.Fatalf("TotalFrames = %d, want 48000", info.TotalFrames)
	}
}

func TestRegistryProbeReportsFalseWithoutProber(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "song.tst", []byte("ZZZZ"))

	r := NewRegistry()
	r.Register(&fakeFactory{name: "tst", exts: []string{".tst"}, magic: []byte("ZZZZ")})

	if _, ok := r.Probe(path); ok {
		t.Fatal("Probe() claimed a prober for a factory that does not implement one")
	}
}

type proberFactory struct {
	fakeFactory
}

func (f *proberFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	return core.StreamInfo{
		Format:      core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		TotalFrames: 48000,
	}, nil
}

func TestDefaultRegistryIsReachable(t *testing.T) {
	r := NewRegistry()
	if got := r.Supported(); len(got) != 0 {
		t.Fatalf("fresh registry supports %v, want empty", got)
	}
	if _, err := r.Open("whatever.tst"); err == nil {
		t.Fatal("fresh registry opened an unknown extension without error")
	}
}
