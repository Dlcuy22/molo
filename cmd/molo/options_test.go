package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
)

func TestParseArgsDefaults(t *testing.T) {
	opts, err := parseArgs([]string{"a.opus"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}

	if opts.volume != 1 {
		t.Errorf("default volume = %v, want 1", opts.volume)
	}
	if opts.backend != "oto" {
		t.Errorf("default backend = %q, want oto", opts.backend)
	}
	if opts.probe != core.DurationProbe {
		t.Errorf("default probe = %v, want DurationProbe", opts.probe)
	}
	if opts.list {
		t.Error("default list = true, want false")
	}
	if opts.help {
		t.Error("default help = true, want false")
	}
	if opts.codecs {
		t.Error("default codecs = true, want false")
	}
	if opts.decoder != "" {
		t.Errorf("default decoder = %q, want empty (automatic)", opts.decoder)
	}
	if len(opts.paths) != 1 || opts.paths[0] != "a.opus" {
		t.Errorf("paths = %q, want [a.opus]", opts.paths)
	}
}

func TestParseArgsVolume(t *testing.T) {
	opts, err := parseArgs([]string{"-volume", "0.25", "a.opus"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.volume != 0.25 {
		t.Fatalf("volume = %v, want 0.25", opts.volume)
	}
}

func TestParseArgsVolumeRejectsOutOfRange(t *testing.T) {
	for _, v := range []string{"-0.1", "1.5"} {
		if _, err := parseArgs([]string{"-volume", v, "a.opus"}); !errors.Is(err, errUsage) {
			t.Errorf("parseArgs(-volume %s) error = %v, want errUsage", v, err)
		}
	}
}

func TestParseArgsVolumeRejectsGarbage(t *testing.T) {
	if _, err := parseArgs([]string{"-volume", "loud", "a.opus"}); !errors.Is(err, errUsage) {
		t.Fatalf("error = %v, want errUsage", err)
	}
}

func TestParseArgsBackend(t *testing.T) {
	opts, err := parseArgs([]string{"-backend", "null", "a.opus"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.backend != "null" {
		t.Fatalf("backend = %q, want null", opts.backend)
	}
}

func TestParseArgsRejectsEmptyBackend(t *testing.T) {
	if _, err := parseArgs([]string{"-backend", "", "a.opus"}); !errors.Is(err, errUsage) {
		t.Fatalf("error = %v, want errUsage", err)
	}
}

func TestParseArgsList(t *testing.T) {
	opts, err := parseArgs([]string{"-list", "a.opus"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.list {
		t.Fatal("list = false, want true")
	}
}

func TestParseArgsProbeModes(t *testing.T) {
	cases := map[string]core.DurationMode{
		"unknown": core.DurationUnknown,
		"probe":   core.DurationProbe,
		"scan":    core.DurationScan,
	}

	for in, want := range cases {
		opts, err := parseArgs([]string{"-probe", in, "a.opus"})
		if err != nil {
			t.Fatalf("parseArgs(-probe %s): %v", in, err)
		}
		if opts.probe != want {
			t.Errorf("-probe %s = %v, want %v", in, opts.probe, want)
		}
	}
}

func TestParseArgsRejectsUnknownProbeMode(t *testing.T) {
	if _, err := parseArgs([]string{"-probe", "deep", "a.opus"}); !errors.Is(err, errUsage) {
		t.Fatalf("error = %v, want errUsage", err)
	}
}

func TestParseArgsDecoder(t *testing.T) {
	opts, err := parseArgs([]string{"-decoder", "opus", "a.opus"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.decoder != "opus" {
		t.Fatalf("decoder = %q, want opus", opts.decoder)
	}
}

func TestParseArgsDecoderEmptyIsAutomatic(t *testing.T) {
	for _, args := range [][]string{
		{"a.opus"},
		{"-decoder", "", "a.opus"},
	} {
		opts, err := parseArgs(args)
		if err != nil {
			t.Fatalf("parseArgs(%q): %v", args, err)
		}
		if opts.decoder != "" {
			t.Errorf("decoder = %q, want empty for %q", opts.decoder, args)
		}
	}
}

func TestParseArgsRejectsUnknownDecoder(t *testing.T) {
	_, err := parseArgs([]string{"-decoder", "not-a-codec", "a.opus"})
	if !errors.Is(err, errUsage) {
		t.Fatalf("error = %v, want errUsage", err)
	}
	// The message must name the valid choices, and they come from the
	// registry rather than a hand-written list.
	for _, c := range decode.Default.Codecs() {
		if !strings.Contains(err.Error(), c.Name) {
			t.Errorf("error %q does not name the available codec %q", err, c.Name)
		}
	}
}

func TestParseArgsCodecsNeedsNoPaths(t *testing.T) {
	opts, err := parseArgs([]string{"-codecs"})
	if err != nil {
		t.Fatalf("parseArgs(-codecs): %v", err)
	}
	if !opts.codecs {
		t.Fatal("codecs = false, want true")
	}
}

func TestParseArgsHelp(t *testing.T) {
	opts, err := parseArgs([]string{"-h"})
	if err != nil {
		t.Fatalf("parseArgs(-h): %v", err)
	}
	if !opts.help {
		t.Fatal("help = false, want true")
	}
}

func TestParseArgsUnknownFlagIsUsageError(t *testing.T) {
	if _, err := parseArgs([]string{"-nope"}); !errors.Is(err, errUsage) {
		t.Fatalf("error = %v, want errUsage", err)
	}
}

func TestParseArgsRequiresAtLeastOnePath(t *testing.T) {
	if _, err := parseArgs(nil); !errors.Is(err, errUsage) {
		t.Fatalf("error = %v, want errUsage", err)
	}
}

func TestUsageDocumentsExitCodes(t *testing.T) {
	for _, want := range []string{"usage error", "no playable input", "playback failure"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usage text does not mention %q:\n%s", want, usageText)
		}
	}
	for _, code := range []string{"0", "1", "2", "3"} {
		if !strings.Contains(usageText, code) {
			t.Errorf("usage text does not mention exit code %q:\n%s", code, usageText)
		}
	}
}

func TestUsageDocumentsDecoderAndCodecs(t *testing.T) {
	for _, want := range []string{"-decoder", "-codecs"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usage text does not mention %q:\n%s", want, usageText)
		}
	}
	// The decoder choice applies to this run's tracks. The usage must not
	// promise a mid-playback switch, which the engine does not do.
	for _, word := range []string{"mid-playback", "while playing", "immediately"} {
		if strings.Contains(usageText, word) {
			t.Errorf("usage text claims a decoder change is immediate (%q):\n%s", word, usageText)
		}
	}
}

func TestClassifyExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{errUsage, exitUsage},
		{errNoInput, exitNoInput},
		{errors.New("boom"), exitFailure},
	}

	for _, tc := range cases {
		if got := classify(tc.err); got != tc.want {
			t.Errorf("classify(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}
