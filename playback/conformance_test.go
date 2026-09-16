package playback_test

import (
	"github.com/dlcuy22/player/playback"
	"github.com/dlcuy22/player/stream"
)

// The streamer is the only Provider in the tree today. Pinning the structural
// match here, in a test, means a change to either side breaks the build instead
// of the runtime, and playback never has to import stream. See the same trick
// at the bottom of stream/streamer.go, which states the contract without
// naming playback.
var _ playback.Provider = (*stream.Streamer)(nil)
