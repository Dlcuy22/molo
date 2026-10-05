package session

import (
	"fmt"
	"sync/atomic"
	"time"
)

// defaultDebugBuffer is the debug stream depth. It is larger than the control
// event buffer because debug records are frequent and individually disposable:
// a UI that drains once a frame should see them, and one that stops reading
// loses only the oldest diagnostics.
const defaultDebugBuffer = 256

// DebugKind classifies one debug record. It is a small closed set so a UI can
// filter without parsing the message.
type DebugKind uint8

const (
	// DebugTrack is emitted when a track is activated, naming the resolved
	// decoder, parser and backend.
	DebugTrack DebugKind = iota
	// DebugUnderrun is emitted when the streamer's underrun counter advances.
	DebugUnderrun
	// DebugSeek is emitted after a reposition lands.
	DebugSeek
	// DebugSwap is emitted after a live decoder or backend change lands.
	DebugSwap
	// DebugPipeline is emitted when a new post-ring chain is installed.
	DebugPipeline
	// DebugProbe is emitted when the asynchronous duration probe answers.
	DebugProbe
	// DebugState is emitted for an accepted state transition.
	DebugState
	// DebugError is emitted for a decode, device or engine failure.
	DebugError
)

// String is the stable label a UI shows. It is not an iota ordinal: the wire
// meaning of a kind must survive an enum insertion.
func (k DebugKind) String() string {
	switch k {
	case DebugTrack:
		return "track"
	case DebugUnderrun:
		return "underrun"
	case DebugSeek:
		return "seek"
	case DebugSwap:
		return "swap"
	case DebugPipeline:
		return "pipeline"
	case DebugProbe:
		return "probe"
	case DebugState:
		return "state"
	case DebugError:
		return "error"
	default:
		return "unknown"
	}
}

// DebugEvent is one diagnostic record. It is deliberately separate from the
// sealed control Event set: debug traffic is frequent and disposable, and it
// must never displace a control event such as Failed or StateChanged from the
// event queue.
//
// Fields are structured where the data is, so a UI can filter and reformat;
// Message is a short pre-rendered sentence for a plain log view. Which numeric
// fields are meaningful depends on Kind and is documented per field.
type DebugEvent struct {
	// Time is stamped when the record is accepted by the sink.
	Time time.Time
	Kind DebugKind

	// Path is the track the record belongs to, empty when not track-scoped.
	Path string

	// Message is a short human sentence. A UI may show it as-is or ignore it.
	Message string

	// Delta is the per-interval count: the underrun increase since the last
	// sample. Zero for kinds that are not deltas.
	Delta int64

	// Count is the absolute figure: cumulative underruns, total frames, or the
	// stage count, depending on Kind.
	Count int64

	// Buffered is the streamer ring occupancy at the sample, zero otherwise.
	Buffered int64

	// Elapsed is how long the reported operation took, zero when it has no
	// duration.
	Elapsed time.Duration

	// Decoder, Parser and Backend name the resolved components when the record
	// is about a track or a swap.
	Decoder string
	Parser  string
	Backend string
}

// debugSink is a consumer-gated debug stream. Like the visualizer tap, it does
// nothing until a consumer attaches, so a session that never asks for debug
// records pays one atomic load per emission and the audio path is untouched.
// Once active it is a bounded drop-oldest queue, so a slow debug consumer can
// never stall the control loop.
type debugSink struct {
	active atomic.Bool
	q      *eventQueue[DebugEvent]
}

func newDebugSink(capacity int) *debugSink {
	return &debugSink{q: newEventQueue[DebugEvent](capacity)}
}

// attach marks the sink as having a consumer. It is what makes emit do work.
func (d *debugSink) attach() { d.active.Store(true) }

// emit records one debug event when a consumer is attached, stamping its time.
// It never blocks and never allocates when inactive.
func (d *debugSink) emit(ev DebugEvent) {
	if !d.active.Load() {
		return
	}
	ev.Time = time.Now()
	d.q.push(ev)
}

// channel is the consumer side of the debug stream.
func (d *debugSink) channel() <-chan DebugEvent { return d.q.channel() }

// droppedCount reports how many records a slow debug consumer cost.
func (d *debugSink) droppedCount() int64 { return d.q.droppedCount() }

// close ends the debug stream.
func (d *debugSink) close() { d.q.close() }

// DebugEvents is the stream of diagnostic records. It is separate from Events
// and never blocks the engine: it is a no-op until this method is called, and
// after that it drops the oldest record when a consumer falls behind, counted
// in DebugDropped. A UI that does not read it pays nothing.
func (s *Session) DebugEvents() <-chan DebugEvent {
	s.debug.attach()

	return s.debug.channel()
}

// DebugDropped reports how many debug records a slow consumer cost. It is the
// debug stream's counterpart to Snapshot.DroppedEvents.
func (s *Session) DebugDropped() int64 { return s.debug.droppedCount() }

// swapLabel names a decoder swap's target for the debug log, spelling out the
// empty preference as "auto" so the line never shows a puzzling blank.
func swapLabel(name string) string {
	if name == "" {
		return "auto"
	}

	return name
}

// stateLabel names a state for the debug log. The exported State is a uint8
// whose ordinal is not a contract, so the label is resolved here rather than
// printed as a number.
func stateLabel(s State) string {
	switch s {
	case StateIdle:
		return "idle"
	case StatePlaying:
		return "playing"
	case StatePaused:
		return "paused"
	case StateStopped:
		return "stopped"
	default:
		return fmt.Sprintf("state(%d)", uint8(s))
	}
}

// underrunDelta returns the increase in a cumulative underrun counter since
// prev, plus the new baseline to store. A counter that went backwards is a
// fresh streamer, so it reports no delta and resets the baseline rather than
// underflowing.
func underrunDelta(prev, cur int64) (delta, base int64) {
	if cur < prev {
		return 0, cur
	}

	return cur - prev, cur
}
