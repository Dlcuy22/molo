package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dlcuy22/molo"
)

// debugConsole drains the engine's consumer-gated debug stream to the process's
// terminal, so the desktop app can be watched without a second webview window.
// It is the terminal counterpart of the TUI's debug panel: the same records,
// one line each.
type debugConsole struct {
	out io.Writer
}

// consoleAttached records whether main attached the process to a terminal. It
// is set once, before the logger is built, and read when the service starts the
// debug pump.
var consoleAttached bool

// startConsole starts the debug pump when the process has a terminal. It does
// nothing when there is none, which is the normal double-clicked GUI case: no
// console is allocated and the consumer-gated stream is never attached.
func (s *PlayerService) startConsole() {
	if !consoleAttached {
		return
	}

	// Attaching the console is what also makes the slog lines written
	// elsewhere in this package visible: main.go points slog at os.Stderr, and
	// attachConsole has pointed that at the terminal.
	c := &debugConsole{out: os.Stderr}
	s.wg.Add(1)
	go s.pumpDebug(c)
}

// pumpDebug reads the debug stream until shutdown. The stream is
// consumer-gated, so this call is what turns it on, and it drops its oldest
// record when the terminal falls behind, so a slow writer never stalls the
// engine.
func (s *PlayerService) pumpDebug(c *debugConsole) {
	defer s.wg.Done()

	events := s.player.DebugEvents()
	for {
		select {
		case <-s.closing:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			fmt.Fprintln(c.out, formatDebugEvent(ev))
		}
	}
}

// formatDebugEvent renders one record as a single line: the wall-clock time, the
// kind as a fixed-width label, the record's own sentence, and the resolved
// components when the record names them. The shape is stable so the output can
// be grepped and read at a glance.
func formatDebugEvent(ev molo.DebugEvent) string {
	var b strings.Builder
	b.WriteString(ev.Time.Format("15:04:05.000"))
	b.WriteString("  ")
	b.WriteString(fmt.Sprintf("%-8s  ", ev.Kind.String()))
	b.WriteString(ev.Message)

	// A track record names the decoder, parser and backend it resolved; they
	// are what a listener checks when a file plays through the wrong path.
	if ev.Decoder != "" {
		b.WriteString("  decoder  ")
		b.WriteString(ev.Decoder)
	}
	if ev.Parser != "" {
		b.WriteString("  parser  ")
		b.WriteString(ev.Parser)
	}
	if ev.Backend != "" {
		b.WriteString("  backend  ")
		b.WriteString(ev.Backend)
	}

	return b.String()
}
