// Command molo is the headless CLI for the molo engine. It plays the files
// or directories named on the command line as a queue, offers single-key
// controls when stdin is a terminal, and degrades to plain line output when it
// is not.
//
// Nothing here imports internal/session: the whole program talks to the public
// facade, which is the same API a TUI or a GUI would use.
package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/decode"
	"golang.org/x/term"
)

// displayInterval is the progress refresh period. 10 Hz is smooth to the eye
// and cheap enough that polling costs nothing next to decoding.
const displayInterval = 100 * time.Millisecond

func main() {
	os.Exit(run(os.Args[1:], realEnv()))
}

// realEnv binds the seams to the actual process. It is the only place os and
// x/term appear, so run itself stays testable.
func realEnv() env {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	// Interactive needs both ends to be a terminal: reading single keys is only
	// meaningful with a terminal on stdin, and the repainting display only with
	// one on stdout. If either is redirected the CLI must behave as a plain
	// filter, which is the contract Recordan relies on.
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))

	return env{
		stdin:        os.Stdin,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		interactive:  interactive,
		makeRaw:      makeRaw,
		newPlayer:    molo.New,
		probe:        probeDuration,
		signals:      signals,
		pollInterval: displayInterval,
	}
}

// makeRaw puts stdin into single-key mode. Restoring the previous state is the
// returned function's whole purpose; a leaked raw terminal is a defect, which
// is why run defers it before any other work.
func makeRaw() (func(), error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}

	return func() { _ = term.Restore(fd, state) }, nil
}

// probeDuration is the CLI's own duration preflight. The facade exposes no
// probe-mode knob, so the CLI resolves durations itself when asked to; the
// engine still probes asynchronously for the live display.
func probeDuration(path string, mode core.DurationMode) (time.Duration, error) {
	p, ok := decode.Default.Probe(path)
	if !ok {
		return 0, nil
	}

	info, err := p.Probe(path, decode.ProbeOptions{Duration: mode})
	if err != nil {
		return 0, err
	}

	return info.Duration(), nil
}
