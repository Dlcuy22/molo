package session

import (
	"sync"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/meta"
	"github.com/dlcuy22/player/stream"
)

// Event is the sealed interface of everything the controller reports. The
// unexported method means only this package can add a kind, so a UI's type
// switch is exhaustive against a known set.
type Event interface{ isEvent() }

// StateChanged is emitted for every accepted state transition.
type StateChanged struct {
	From State
	To   State
}

// TrackChanged is emitted when a new track becomes current, whether from a
// command or from the queue advancing.
type TrackChanged struct {
	Index int
	Path  string
}

// Seeked confirms a seek after it has been applied and audio resumed. From and
// Elapsed describe the move itself: where playback was, and how long the
// reposition took.
type Seeked struct {
	Position time.Duration // where the seek landed
	From     time.Duration // where playback was before the seek
	Elapsed  time.Duration // how long the reposition took
}

// TrackEnded is emitted once per track when it reaches its natural end.
type TrackEnded struct{}

// Swapped confirms a live component change: the decoder or playback backend of
// the current track was rebuilt. Kind is "decoder" or "backend", Name is the
// forced component name (empty for automatic selection), and Elapsed is how
// long the rebuild took.
type Swapped struct {
	Kind    string
	Name    string
	Elapsed time.Duration
}

// PipelineChanged confirms that a new post-ring effect chain is in force.
// Stages is how many effects the chain runs after the change, so a UI can tell
// an effect being added from the chain being cleared.
type PipelineChanged struct {
	Stages int
}

// Failed reports a decode or device error that stopped playback. The session
// is still usable afterwards: the next Play creates a fresh pipeline.
type Failed struct {
	Err error
}

func (StateChanged) isEvent()    {}
func (TrackChanged) isEvent()    {}
func (Seeked) isEvent()          {}
func (TrackEnded) isEvent()      {}
func (Swapped) isEvent()         {}
func (PipelineChanged) isEvent() {}
func (Failed) isEvent()          {}

// eventQueue is a non-blocking event sink. It is a channel plus a drop-oldest
// policy: a push never waits for a consumer, and when the buffer is full the
// oldest event is discarded in favour of the newest. The dropped count is kept
// so a slow UI is visible instead of silent.
//
// Dropping the oldest rather than the newest is the deliberate choice for a
// player UI: a stale "Playing" is worse than a missing one, and the newest
// state is the one that matches what the user hears.
type eventQueue struct {
	mu     sync.Mutex
	ch     chan Event
	drop   int64
	closed bool
}

func newEventQueue(capacity int) *eventQueue {
	if capacity < 1 {
		capacity = 1
	}

	return &eventQueue{ch: make(chan Event, capacity)}
}

// channel is the consumer side. It is a plain channel, so a UI can range over
// it or select on it like any other.
func (q *eventQueue) channel() <-chan Event { return q.ch }

func (q *eventQueue) push(ev Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}

	select {
	case q.ch <- ev:
		return
	default:
	}

	// The buffer is full. Discard the oldest and retry once. The retry can
	// still fail if a consumer took the freed slot, which is fine: the event
	// is simply dropped and counted, never blocked on.
	select {
	case <-q.ch:
		q.drop++
	default:
	}
	select {
	case q.ch <- ev:
	default:
		q.drop++
	}
}

// droppedCount reports how many events a slow consumer cost.
func (q *eventQueue) droppedCount() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.drop
}

// close ends the event stream. Pushes after this are inert, so a worker that
// outlives Close cannot panic on a closed channel.
func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}

// Snapshot is a read-only view of the controller at one instant. It is cheap:
// the values a UI polls every frame come from atomics and a short-lived lock,
// never from the audio path.
type Snapshot struct {
	State State
	Path  string
	Meta  meta.Meta

	// Position is where the device is reading, in the streamer's output clock.
	Position time.Duration

	// Duration is zero until the async probe has answered, which a UI should
	// read as "unknown" rather than "empty".
	Duration time.Duration

	Volume float64
	Format core.FrameFormat

	QueueIndex int
	QueueLen   int

	Stats stream.Stats

	// Decoder and Parser name the components handling the current track, for a
	// debug view. Empty when the decoder does not describe itself.
	Decoder string
	Parser  string

	// Backend names the playback backend the device was opened against. It is
	// the runtime preference, which a SwapBackend can change while a track
	// plays, so it matches Settings().Backend once the swap lands.
	Backend string

	// DroppedEvents counts events discarded because the consumer fell behind.
	DroppedEvents int64
}
