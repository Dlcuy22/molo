package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/dlcuy22/molo"
)

func TestReadKeySingleBytes(t *testing.T) {
	cases := []struct {
		in   []byte
		want key
	}{
		{[]byte{' '}, keyTogglePause},
		{[]byte{'q'}, keyQuit},
		{[]byte{'n'}, keyNext},
		{[]byte{'p'}, keyPrev},
		{[]byte{'h'}, keySeekBack},
		{[]byte{'l'}, keySeekForward},
		{[]byte{'+'}, keyVolumeUp},
		{[]byte{'='}, keyVolumeUp},
		{[]byte{'-'}, keyVolumeDown},
		{[]byte{'_'}, keyVolumeDown},
		{[]byte{0x03}, keyInterrupt},
		{[]byte{'x'}, keyNone},
	}

	for _, tc := range cases {
		got, err := readKey(bytes.NewReader(tc.in))
		if err != nil {
			t.Errorf("readKey(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("readKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestReadKeyArrowSequences(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want key
	}{
		{"csi left", []byte{0x1b, '[', 'D'}, keySeekBack},
		{"csi right", []byte{0x1b, '[', 'C'}, keySeekForward},
		{"ss3 left", []byte{0x1b, 'O', 'D'}, keySeekBack},
		{"ss3 right", []byte{0x1b, 'O', 'C'}, keySeekForward},
		{"unrelated escape", []byte{0x1b, '[', 'A'}, keyNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readKey(bytes.NewReader(tc.in))
			if err != nil {
				t.Fatalf("readKey(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("readKey(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestReadKeyTruncatedEscapeDoesNotHang guards the read loop against a lone
// ESC arriving at end of input, which is what a pipe close looks like.
func TestReadKeyTruncatedEscapeDoesNotHang(t *testing.T) {
	for _, in := range [][]byte{{0x1b}, {0x1b, '['}} {
		got, err := readKey(bytes.NewReader(in))
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("readKey(%q): %v", in, err)
		}
		if got != keyNone {
			t.Fatalf("readKey(%q) = %v, want keyNone", in, got)
		}
	}
}

func TestReadKeyEOF(t *testing.T) {
	if _, err := readKey(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("readKey(empty) error = %v, want EOF", err)
	}
}

func TestHandleKeyDispatch(t *testing.T) {
	const snapPos = 10 * time.Second

	cases := []struct {
		name      string
		key       key
		state     molo.State
		wantCall  string
		wantAct   action
		wantValue float64
	}{
		{name: "space pauses while playing", key: keyTogglePause, state: molo.Playing, wantCall: "Pause"},
		{name: "space resumes while paused", key: keyTogglePause, state: molo.Paused, wantCall: "Resume"},
		{name: "n nexts", key: keyNext, state: molo.Playing, wantCall: "Next"},
		{name: "p prevs", key: keyPrev, state: molo.Playing, wantCall: "Prev"},
		{name: "right seeks", key: keySeekForward, state: molo.Playing, wantCall: "Seek"},
		{name: "left seeks", key: keySeekBack, state: molo.Playing, wantCall: "Seek"},
		{name: "plus raises", key: keyVolumeUp, state: molo.Playing, wantCall: "SetVolume", wantValue: 0.6},
		{name: "minus lowers", key: keyVolumeDown, state: molo.Playing, wantCall: "SetVolume", wantValue: 0.4},
		{name: "q quits", key: keyQuit, state: molo.Playing, wantAct: actionQuit},
		{name: "ctrl-c interrupts", key: keyInterrupt, state: molo.Playing, wantAct: actionInterrupt},
		{name: "unknown is inert", key: keyNone, state: molo.Playing},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newFakePlayer()
			p.snap = molo.Snapshot{State: tc.state, Position: snapPos, Volume: 0.5}
			p.snap.QueueIndex = 0
			p.snap.QueueLen = 1

			act := handleKey(p, tc.key)
			if act != tc.wantAct {
				t.Fatalf("handleKey(%v) action = %v, want %v", tc.key, act, tc.wantAct)
			}
			if tc.wantCall == "" {
				if len(p.calls) != 0 {
					t.Fatalf("expected no calls, got %q", p.calls)
				}

				return
			}
			if !p.called(tc.wantCall) {
				t.Fatalf("expected %s, got %q", tc.wantCall, p.calls)
			}
			if tc.wantValue != 0 && p.Snapshot().Volume != tc.wantValue {
				t.Fatalf("volume = %v, want %v", p.Snapshot().Volume, tc.wantValue)
			}
		})
	}
}

// TestHandleKeySeekNeverGoesNegative pins the one arithmetic edge the seek
// keys have: seeking back from near the start must clamp at zero, because the
// engine rejects a negative target.
func TestHandleKeySeekNeverGoesNegative(t *testing.T) {
	p := newFakePlayer()
	p.snap = molo.Snapshot{State: molo.Playing, Position: time.Second, Volume: 1}

	handleKey(p, keySeekBack)

	if got := p.Snapshot().Position; got != 0 {
		t.Fatalf("position after back-seek = %v, want 0", got)
	}
}

// TestHandleKeyForwardSeekClampsToDuration guards the other edge: the decoder
// reports a seek past the end as a failure, so a forward seek with a known
// duration must stop at the end instead of ending playback.
func TestHandleKeyForwardSeekClampsToDuration(t *testing.T) {
	p := newFakePlayer()
	p.snap = molo.Snapshot{State: molo.Playing, Position: 0, Duration: 2 * time.Second, Volume: 1}

	handleKey(p, keySeekForward)

	if got := p.Snapshot().Position; got != 2*time.Second {
		t.Fatalf("forward seek = %v, want the 2s duration", got)
	}
}

func TestHandleKeyVolumeClamps(t *testing.T) {
	p := newFakePlayer()
	p.snap = molo.Snapshot{State: molo.Playing, Volume: 0.95}
	handleKey(p, keyVolumeUp)
	if got := p.Snapshot().Volume; got != 1 {
		t.Fatalf("volume after up = %v, want 1", got)
	}

	p2 := newFakePlayer()
	p2.snap = molo.Snapshot{State: molo.Playing, Volume: 0.05}
	handleKey(p2, keyVolumeDown)
	if got := p2.Snapshot().Volume; got != 0 {
		t.Fatalf("volume after down = %v, want 0", got)
	}
}
