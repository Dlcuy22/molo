package main

import (
	"errors"
	"io"
	"time"

	"github.com/dlcuy22/molo"
)

// seekStep and volumeStep are the granularity of the two continuous controls.
// They are constants rather than flags because the keyboard is a coarse input
// and a user who needs precision has the exact flags.
const (
	seekStep   = 5 * time.Second
	volumeStep = 0.1
)

// key is a decoded control input. Escape sequences are collapsed into one
// value here so the dispatch below never sees a raw byte.
type key uint8

const (
	keyNone key = iota
	keyTogglePause
	keyQuit
	keyNext
	keyPrev
	keySeekForward
	keySeekBack
	keyVolumeUp
	keyVolumeDown
	keyInterrupt
)

// readKey reads one key from r. A plain byte is returned directly; an ESC
// starts a possible arrow sequence, which is consumed as a whole so the
// following '[' or 'O' is not mistaken for a typed character.
func readKey(r io.Reader) (key, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return keyNone, err
	}

	switch first[0] {
	case ' ':
		return keyTogglePause, nil
	case 'q', 'Q':
		return keyQuit, nil
	case 'n':
		return keyNext, nil
	case 'p':
		return keyPrev, nil
	case 'h':
		return keySeekBack, nil
	case 'l':
		return keySeekForward, nil
	case '+', '=':
		return keyVolumeUp, nil
	case '-', '_':
		return keyVolumeDown, nil
	case 0x03:
		// Raw mode clears ISIG, so a typed Ctrl-C arrives as a byte rather
		// than a SIGINT and must be handled explicitly.
		return keyInterrupt, nil
	case 0x1b:
		return readEscape(r)
	default:
		return keyNone, nil
	}
}

// readEscape consumes the rest of a CSI or SS3 cursor sequence. A sequence
// that ends early, which is what a closed pipe looks like, is not an error: it
// simply carries no command.
func readEscape(r io.Reader) (key, error) {
	var seq [2]byte
	if _, err := io.ReadFull(r, seq[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return keyNone, nil
		}

		return keyNone, err
	}

	if seq[0] != '[' && seq[0] != 'O' {
		return keyNone, nil
	}

	switch seq[1] {
	case 'C':
		return keySeekForward, nil
	case 'D':
		return keySeekBack, nil
	default:
		return keyNone, nil
	}
}

// action is what a key asks the run loop to do beyond applying the command.
type action uint8

const (
	actionNone action = iota
	actionQuit
	actionInterrupt
)

// handleKey applies one key to the player and reports whether the key also
// asks the run loop to terminate. It is deliberately free of terminal and
// ticker concerns so every binding is unit-testable against a fake molo.
func handleKey(p molo.Player, k key) action {
	snap := p.Snapshot()

	switch k {
	case keyTogglePause:
		switch snap.State {
		case molo.Playing:
			_ = p.Pause()
		case molo.Paused:
			_ = p.Resume()
		}
	case keyNext:
		_ = p.Next()
	case keyPrev:
		_ = p.Prev()
	case keySeekForward:
		target := snap.Position + seekStep
		// Clamp to a known duration: the decoder treats a target past the end
		// as a failure, and a user holding 'l' near the end of a track should
		// not be able to end playback with a seek key.
		if snap.Duration > 0 && target > snap.Duration {
			target = snap.Duration
		}
		_ = p.Seek(target)
	case keySeekBack:
		target := snap.Position - seekStep
		if target < 0 {
			target = 0
		}
		_ = p.Seek(target)
	case keyVolumeUp:
		p.SetVolume(clampVolume(snap.Volume + volumeStep))
	case keyVolumeDown:
		p.SetVolume(clampVolume(snap.Volume - volumeStep))
	case keyQuit:
		return actionQuit
	case keyInterrupt:
		return actionInterrupt
	}

	return actionNone
}

func clampVolume(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}

	return v
}
