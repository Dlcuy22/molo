package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/dlcuy22/player/core"
)

// Exit codes. They are distinct because a caller such as Recordan needs to
// tell "you invoked me wrong" from "there was nothing to play" from "audio
// broke"; the usage text documents the same numbers.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitNoInput = 3
)

// errUsage marks a command-line problem. It is wrapped by parseArgs so the
// caller can classify it without matching on message text.
var errUsage = errors.New("usage error")

// errNoInput means the arguments named no playable file.
var errNoInput = errors.New("no playable input")

// options is the parsed command line.
type options struct {
	paths   []string
	volume  float64
	backend string
	probe   core.DurationMode
	list    bool
	help    bool
}

const usageText = `usage: player [flags] <path>...

Plays each path as a queue. A directory expands to a recursive scan of the
decodable extensions.

flags:
  -backend name   playback backend (default "oto")
  -list           print the resolved queue and exit
  -probe mode     duration preflight for the progress display:
                  unknown, probe, or scan (default "probe")
  -volume v       initial volume in [0, 1] (default 1)

keys (only when stdin is a terminal):
  space           pause or resume
  q               quit
  n / p           next or previous track
  left / h        seek back
  right / l       seek forward
  + / -           volume up or down
  Ctrl-C          stop and exit

exit codes:
  0  success
  1  playback failure
  2  usage error
  3  no playable input
`

// parseArgs parses the command line. It writes nothing: the caller owns where
// usage and errors go, so `-h` can land on stdout while a bad flag lands on
// stderr. The returned error is always errUsage, wrapped, which is what
// classify turns into exitUsage.
func parseArgs(args []string) (options, error) {
	var (
		opts    options
		probe   string
		volume  float64
		backend string
	)

	fs := flag.NewFlagSet("player", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.Float64Var(&volume, "volume", 1, "initial volume in [0, 1]")
	fs.BoolVar(&opts.list, "list", false, "print the resolved queue and exit")
	fs.StringVar(&backend, "backend", "oto", "playback backend")
	fs.StringVar(&probe, "probe", "probe", "duration preflight: unknown, probe, or scan")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			opts.help = true

			return opts, nil
		}

		return opts, fmt.Errorf("%w: %v", errUsage, err)
	}

	if volume < 0 || volume > 1 {
		return opts, fmt.Errorf("%w: -volume must be in [0, 1], got %v", errUsage, volume)
	}
	if backend == "" {
		return opts, fmt.Errorf("%w: -backend must not be empty", errUsage)
	}

	mode, err := parseDurationMode(probe)
	if err != nil {
		return opts, err
	}

	opts.paths = fs.Args()
	opts.volume = volume
	opts.backend = backend
	opts.probe = mode

	if len(opts.paths) == 0 {
		return opts, fmt.Errorf("%w: no paths given", errUsage)
	}

	return opts, nil
}

func parseDurationMode(s string) (core.DurationMode, error) {
	switch strings.ToLower(s) {
	case "unknown":
		return core.DurationUnknown, nil
	case "probe":
		return core.DurationProbe, nil
	case "scan":
		return core.DurationScan, nil
	default:
		return 0, fmt.Errorf("%w: -probe must be unknown, probe, or scan, got %q", errUsage, s)
	}
}

// classify maps a terminal error onto the process exit code.
func classify(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, errNoInput):
		return exitNoInput
	default:
		return exitFailure
	}
}
