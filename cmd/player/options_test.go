package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dlcuy22/player/core"
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
