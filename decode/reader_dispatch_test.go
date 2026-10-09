package decode

import (
	"bytes"
	"io"
	"testing"
)

// readerFactory is a fakeFactory that also implements ReaderOpener, recording
// the reader it was handed and the bytes it read, so a test can prove the
// sniffed header reached the factory intact.
type readerFactory struct {
	fakeFactory

	got     []byte
	gotSeek bool
}

func (f *readerFactory) OpenReader(r io.Reader) (Decoder, error) {
	f.opens++
	if _, ok := r.(io.ReadSeeker); ok {
		f.gotSeek = true
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.got = data

	return &fakeDecoder{info: fakeStreamInfo()}, nil
}

// TestRegistryOpenReaderDispatchesByMagic proves a stream with no path is
// routed by its leading bytes, and that the header the sniff consumed is not
// lost: the factory reads the stream from its first byte.
func TestRegistryOpenReaderDispatchesByMagic(t *testing.T) {
	body := []byte("ZZZZthe rest of the stream")

	r := NewRegistry()
	f := &readerFactory{fakeFactory: fakeFactory{name: "tst", magic: []byte("ZZZZ")}}
	r.Register(f)

	dec, err := r.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	_ = dec.Close()

	if f.opens != 1 {
		t.Fatalf("OpenReader calls = %d, want 1", f.opens)
	}
	if !bytes.Equal(f.got, body) {
		t.Fatalf("factory read %q, want the whole stream %q", f.got, body)
	}
}

// TestRegistryOpenReaderKeepsSeeker proves a seekable source reaches the factory
// as an io.ReadSeeker, which is what keeps native seek for a range-backed URL.
func TestRegistryOpenReaderKeepsSeeker(t *testing.T) {
	r := NewRegistry()
	f := &readerFactory{fakeFactory: fakeFactory{name: "tst", magic: []byte("ZZZZ")}}
	r.Register(f)

	if _, err := r.OpenReader(bytes.NewReader([]byte("ZZZZbody"))); err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if !f.gotSeek {
		t.Fatal("a seekable source was not handed to the factory as an io.ReadSeeker")
	}
}

// TestRegistryOpenReaderForwardOnly proves a non-seekable stream is still
// sniffed and delivered whole, without the factory seeing a seeker it cannot
// honour.
func TestRegistryOpenReaderForwardOnly(t *testing.T) {
	r := NewRegistry()
	f := &readerFactory{fakeFactory: fakeFactory{name: "tst", magic: []byte("ZZZZ")}}
	r.Register(f)

	body := []byte("ZZZZforward-only body")
	if _, err := r.OpenReader(&readerOnly{bytes.NewReader(body)}); err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if f.gotSeek {
		t.Fatal("a forward-only source was handed to the factory as an io.ReadSeeker")
	}
	if !bytes.Equal(f.got, body) {
		t.Fatalf("factory read %q, want %q", f.got, body)
	}
}

// TestRegistryOpenReaderSkipsPathOnlyFactories proves a factory without
// ReaderOpener is never chosen for a stream, even when its magic matches.
func TestRegistryOpenReaderSkipsPathOnlyFactories(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeFactory{name: "pathonly", magic: []byte("ZZZZ")})

	if _, err := r.OpenReader(bytes.NewReader([]byte("ZZZZbody"))); err == nil {
		t.Fatal("OpenReader used a factory that cannot open a stream")
	}
}

// TestRegistryOpenReaderUnknownIsUnsupported proves an unclaimed stream is a
// clear error rather than a nil decoder.
func TestRegistryOpenReaderUnknownIsUnsupported(t *testing.T) {
	r := NewRegistry()
	r.Register(&readerFactory{fakeFactory: fakeFactory{name: "tst", magic: []byte("ZZZZ")}})

	if _, err := r.OpenReader(bytes.NewReader([]byte("NOPEbody"))); err == nil {
		t.Fatal("OpenReader accepted an unclaimed stream")
	}
}

// TestRegistryOpenReaderNamedBypassesSniffing proves the named entry point opens
// the named stream codec directly and rejects a name that has no reader.
func TestRegistryOpenReaderNamedBypassesSniffing(t *testing.T) {
	r := NewRegistry()
	f := &readerFactory{fakeFactory: fakeFactory{name: "tst", magic: []byte("ZZZZ")}}
	r.Register(f)
	r.Register(&fakeFactory{name: "pathonly", magic: []byte("ZZZZ")})

	// The magic is absent, so sniffing would reject this; naming the codec
	// skips the check, which is what a caller forcing a codec expects.
	if _, err := r.OpenReaderNamed("tst", bytes.NewReader([]byte("anything"))); err != nil {
		t.Fatalf("OpenReaderNamed: %v", err)
	}
	if _, err := r.OpenReaderNamed("pathonly", bytes.NewReader([]byte("anything"))); err == nil {
		t.Fatal("OpenReaderNamed accepted a codec that cannot open a stream")
	}
	if _, err := r.OpenReaderNamed("nope", bytes.NewReader([]byte("anything"))); err == nil {
		t.Fatal("OpenReaderNamed accepted an unknown codec name")
	}
}

// readerOnly hides io.Seeker, standing in for a forward-only network body.
type readerOnly struct{ r io.Reader }

func (r readerOnly) Read(p []byte) (int, error) { return r.r.Read(p) }

var _ io.Reader = readerOnly{}
