package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dlcuy22/molo"
)

// TestFormatDebugEvent pins the console line shape: a timestamp, a fixed-width
// kind label, the record's sentence, and the resolved components only when the
// record names them. A change here is a deliberate edit to what a user reads.
func TestFormatDebugEvent(t *testing.T) {
	at := time.Date(2026, 10, 9, 14, 3, 7, 420_000_000, time.UTC)

	cases := []struct {
		name string
		ev   molo.DebugEvent
		want string
	}{
		{
			name: "track names its resolved components",
			ev: molo.DebugEvent{
				Time:    at,
				Kind:    molo.DebugTrack,
				Message: "track /music/a.opus",
				Decoder: "go-opus",
				Parser:  "molo/decode (oggopus)",
				Backend: "oto",
			},
			want: "14:03:07.420  track     track /music/a.opus  decoder  go-opus  parser  molo/decode (oggopus)  backend  oto",
		},
		{
			name: "a bare record carries no component tail",
			ev: molo.DebugEvent{
				Time:    at,
				Kind:    molo.DebugState,
				Message: "idle -> playing",
			},
			want: "14:03:07.420  state     idle -> playing",
		},
		{
			name: "underrun uses the fixed-width kind column",
			ev: molo.DebugEvent{
				Time:    at,
				Kind:    molo.DebugUnderrun,
				Message: "underrun +2",
			},
			want: "14:03:07.420  underrun  underrun +2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatDebugEvent(tc.ev); got != tc.want {
				t.Fatalf("formatDebugEvent =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// TestPumpDebugWritesEachRecord proves the pump drains the debug stream to its
// writer and stops on close, which is the whole bridge between the engine's
// consumer-gated stream and the terminal.
func TestPumpDebugWritesEachRecord(t *testing.T) {
	ch := make(chan molo.DebugEvent, 2)
	ch <- molo.DebugEvent{Time: time.Unix(0, 0), Kind: molo.DebugState, Message: "idle -> playing"}
	ch <- molo.DebugEvent{Time: time.Unix(0, 0), Kind: molo.DebugError, Message: "boom"}
	close(ch)

	f := newFakePlayer()
	f.debugCh = ch

	var buf bytes.Buffer
	svc := &PlayerService{player: f, closing: make(chan struct{})}
	svc.wg.Add(1)

	done := make(chan struct{})
	go func() {
		svc.pumpDebug(&debugConsole{out: &buf})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpDebug did not return after the stream closed")
	}

	out := buf.String()
	if !strings.Contains(out, "idle -> playing") || !strings.Contains(out, "boom") {
		t.Fatalf("pumpDebug output missing a record:\n%s", out)
	}
}
