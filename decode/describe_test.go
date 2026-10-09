package decode

import "testing"

// TestDescribeNamesOpus proves the pure-Go decoder can name the pieces it
// is built from, and that the labels reach the caller through the helper.
func TestDescribeNamesOpus(t *testing.T) {
	d, err := NewOpusFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	decoder, parser := Describe(d)
	if decoder != "go-opus" {
		t.Fatalf("DecoderName = %q, want %q", decoder, "go-opus")
	}
	if parser != "molo/decode (oggopus)" {
		t.Fatalf("ParserName = %q, want %q", parser, "molo/decode (oggopus)")
	}
}

// TestDescribeNamesLibopusfile proves the native decoder names the library that
// actually does the work rather than reusing the pure-Go labels.
func TestDescribeNamesLibopusfile(t *testing.T) {
	requireLibopusfile(t)

	d, err := NewLibopusfileFactory().Open(fixturePath(t, "stereo_2s.opus"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	decoder, parser := Describe(d)
	if decoder != "libopusfile" {
		t.Fatalf("DecoderName = %q, want %q", decoder, "libopusfile")
	}
	if parser != "libopusfile (bundled)" {
		t.Fatalf("ParserName = %q, want %q", parser, "libopusfile (bundled)")
	}
}

// TestDescribeWithoutDescriptorIsEmpty is the compatibility guarantee: a
// decoder that implements only Decoder must report empty labels instead of
// panicking or being guessed at. Test fakes and third-party codecs rely on it.
func TestDescribeWithoutDescriptorIsEmpty(t *testing.T) {
	decoder, parser := Describe(&fakeDecoder{info: fakeStreamInfo()})
	if decoder != "" || parser != "" {
		t.Fatalf("Describe on a plain Decoder = %q/%q, want empty", decoder, parser)
	}
}
