package tui

import (
	"errors"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/meta"
)

// errFailed stands in for any engine error a test feeds the model.
var errFailed = errors.New("test failure")

func playerSnapshot(path string, pos, dur time.Duration, m meta.Meta) player.Snapshot {
	return player.Snapshot{
		State:      player.Playing,
		Path:       path,
		Meta:       m,
		Position:   pos,
		Duration:   dur,
		Volume:     1,
		QueueIndex: 0,
		QueueLen:   1,
	}
}
