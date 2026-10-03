package tui

import (
	"errors"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/meta"
)

// errFailed stands in for any engine error a test feeds the model.
var errFailed = errors.New("test failure")

func playerSnapshot(path string, pos, dur time.Duration, m meta.Meta) molo.Snapshot {
	return molo.Snapshot{
		State:      molo.Playing,
		Path:       path,
		Meta:       m,
		Position:   pos,
		Duration:   dur,
		Volume:     1,
		QueueIndex: 0,
		QueueLen:   1,
	}
}
