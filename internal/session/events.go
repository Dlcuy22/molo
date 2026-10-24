package session

import (
	"sync"
	"time"

	"github.com/dlcuy22/molo/core"
	"github.com/dlcuy22/molo/meta"
	"github.com/dlcuy22/molo/stream"
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

// Seeked confirms a seek after it has been applied and audio resumed. Position
// is where the seek landed, From is where playback was, and Elapsed is how long
// the reposition took.
type Seeked struct {
	Position time.Duration
	From     time.Duration
	Elapsed  time.Duration
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

// RepeatChanged confirms that the repeat mode in force changed. It is emitted by
// the control loop, so a UI sees the same value the end-of-track decision uses
// rather than whatever it last asked for, and a set that changed nothing emits
// no event.
type RepeatChanged struct {
	Mode RepeatMode
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
func (RepeatChanged) isEvent()   {}
func (Failed) isEvent()          {}

// eventQueue is a non-blocking sink: a channel plus a drop-oldest policy. A
// push never waits for a consumer, and when the buffer is full the oldest value
// is discarded in favour of the newest. The dropped count is kept so a slow
// consumer is visible instead of silent.
//
// Dropping the oldest rather than the newest is deliberate for a player UI: a
// stale "Playing" is worse than a missing one.
type eventQueue[T any] struct {
	mu     sync.Mutex
	ch     chan T
	drop   int64
	closed bool
}

func newEventQueue[T any](capacity int) *eventQueue[T] {
	if capacity < 1 {
		capacity = 1
	}

	return &eventQueue[T]{ch: make(chan T, capacity)}
}

// channel is the consumer side. It is a plain channel, so a UI can range over
// it or select on it like any other.
func (q *eventQueue[T]) channel() <-chan T { return q.ch }

func (q *eventQueue[T]) push(ev T) {
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

// droppedCount reports how many values a slow consumer cost.
func (q *eventQueue[T]) droppedCount() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.drop
}

// close ends the stream. Pushes after this are inert, so a worker that
// outlives Close cannot panic on a closed channel.
func (q *eventQueue[T]) close() {
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

	// Repeat is the mode the control loop uses at end-of-track, so a UI shows
	// the same value the engine acts on rather than one it last requested.
	Repeat RepeatMode

	// DroppedEvents counts events discarded because the consumer fell behind.
	DroppedEvents int64
}
