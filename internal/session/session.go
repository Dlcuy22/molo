package session

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/dsp"
	"github.com/dlcuy22/player/meta"
	"github.com/dlcuy22/player/playback"
	"github.com/dlcuy22/player/stream"
)

// The device format is fixed, which is what lets one device serve every track.
var canonical = core.CanonicalFormat

// defaultEventBuffer is the event channel depth. It is generous enough that a
// UI draining once a frame never loses an event, and small enough that a UI
// that has stopped reading cannot grow memory.
const defaultEventBuffer = 64

// devicePollInterval bounds how long a device error can go unnoticed. The oto
// backend fails on its own thread, so polling is the only way to surface it.
const devicePollInterval = 20 * time.Millisecond

var (
	// ErrEmptyQueue means PlayQueue was handed nothing to play.
	ErrEmptyQueue = errors.New("session: queue is empty")
	// ErrEmptyPath means Play was handed an empty path.
	ErrEmptyPath = errors.New("session: path is empty")
	// ErrNegativeSeek means a seek target was negative.
	ErrNegativeSeek = errors.New("session: seek target is negative")
	// ErrNoProber means the registry has no duration probe for the path. The
	// duration simply stays unknown.
	ErrNoProber = errors.New("session: no duration prober for the path")
	// ErrClosed means the session is shutting down.
	ErrClosed = errors.New("session: closed")
)

// Config tunes a Session. Zero fields take the documented default, so the
// empty Config is the intended setup.
type Config struct {
	// Backend is the playback backend name, default "oto".
	Backend string

	// EventBuffer is the event channel depth, default 64.
	EventBuffer int

	// RingFrames is the streamer ring size in output frames. Zero selects the
	// streamer's own default of about 300 ms.
	RingFrames int

	// Volume is the initial gain in [0, 1]. Zero selects unity, because a
	// muted-by-default player is never what a caller means.
	Volume float64

	// Resolver describes each track. Nil selects meta.Default().
	Resolver meta.Resolver

	// ProbeMode selects how much work the asynchronous duration probe may do.
	// Nil selects the cheap tail probe. A pointer, not a value, because
	// core.DurationUnknown is the zero value and must stay distinguishable
	// from "the caller did not choose".
	ProbeMode *core.DurationMode

	// The seams below let tests drive the controller without a file system or
	// an audio server. Production code leaves them nil.
	openDecoder func(path string) (decode.Decoder, error)
	probeStream func(path string, opts decode.ProbeOptions) (core.StreamInfo, error)
	newDevice   func() (playback.Device, error)
}

// command is one queued public call. Commands are values, so a caller never
// shares state with the controller.
type command struct {
	kind  cmdKind
	path  string
	paths []string
	pos   time.Duration
}

type cmdKind uint8

const (
	cmdPlay cmdKind = iota
	cmdPlayQueue
	cmdNext
	cmdPrev
	cmdPause
	cmdResume
	cmdStop
	cmdSeek
)

// openResult is a finished synchronous pipeline build, delivered to the
// control loop so the build itself never runs there.
type openResult struct {
	seq      uint64
	index    int
	path     string
	streamer *stream.Streamer
	info     core.StreamInfo
	decoder  string
	parser   string
	err      error
}

type probeResult struct {
	seq   uint64
	index int
	info  core.StreamInfo
	err   error
}

type metaResult struct {
	seq   uint64
	index int
	m     *meta.Meta
	err   error
}

// seekResult is a finished reposition, delivered to the control loop so the
// seek itself never runs there. stream and seq identify the track it was
// started for, so the controller can drop a result whose track has moved on.
type seekResult struct {
	seq     uint64
	stream  *stream.Streamer
	target  time.Duration
	from    time.Duration
	resume  bool
	elapsed time.Duration
	err     error
}

// view is the mutable state Snapshot reads. The control goroutine is the only
// writer; readers take the same short-lived lock.
type view struct {
	state State

	path     string
	meta     meta.Meta
	duration time.Duration
	lastPos  time.Duration

	// decoder and parser label the streamer feeding the device. They belong to
	// the track, so startIndex clears them with the rest of the per-track state.
	decoder string
	parser  string

	queueIndex int
	queueLen   int
}

// Session is the controller. It owns the queue, the current streamer, the
// device and the gain, and it is the only place those change.
//
// Concurrency: public commands append to an inbox under a short-lived mutex
// and signal the control loop without blocking. The control loop moves the
// batch and reacts; every slow operation (opening a decoder, probing a file,
// resolving tags) runs on its own goroutine and reports back through a
// channel. That is what makes a command non-blocking by construction rather
// than by the caller being careful.
type Session struct {
	cfg    Config
	gain   *dsp.Gain
	tap    *tap
	events *eventQueue
	v      view

	mu     sync.Mutex
	inbox  []command
	closed bool

	wake    chan struct{}
	closing chan struct{}
	stopped chan struct{}
	feed    chan feedEvent
	openCh  chan openResult
	probeCh chan probeResult
	metaCh  chan metaResult
	seekCh  chan seekResult

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once

	// workerWG tracks the asynchronous build/probe/meta goroutines so
	// shutdown can wait for them before draining their result channels.
	workerWG sync.WaitGroup

	// The fields below are owned by the control goroutine and are never read
	// from another one, so they need no lock.
	provider      *routedProvider
	device        playback.Device
	deviceStarted bool
	live          *stream.Streamer
	queue         []string
	index         int
	seq           uint64
	inFlight      bool

	// Seek is the one slow step that is not a build, so the controller starts
	// it on a worker and keeps going. seekBusy is true while a worker is
	// repositioning and seekPending holds the newest target that has not
	// started yet (latest-wins). Both are owned by the control goroutine; the
	// worker only reports on seekCh.
	seekBusy    bool
	seekPending *seekRequest
}

// seekRequest is one accepted seek, either in flight or waiting to replace an
// in-flight one. from is captured when the device has been parked, so it is
// the frame the reposition actually started from.
type seekRequest struct {
	seq    uint64
	stream *stream.Streamer
	target time.Duration
	from   time.Duration
	resume bool
}

// New builds a session and starts its control goroutine. It does not touch the
// audio device or the file system, so it succeeds on a machine with no audio
// server; a backend failure surfaces as a Failed event on the first Play.
func New(cfg Config) (*Session, error) {
	if cfg.Backend == "" {
		cfg.Backend = "oto"
	}
	if cfg.EventBuffer <= 0 {
		cfg.EventBuffer = defaultEventBuffer
	}
	if cfg.Volume == 0 {
		cfg.Volume = 1
	}
	if cfg.Resolver == nil {
		cfg.Resolver = meta.Default()
	}
	if cfg.openDecoder == nil {
		cfg.openDecoder = decode.Default.Open
	}
	if cfg.probeStream == nil {
		cfg.probeStream = probeWithDefaultRegistry
	}
	if cfg.ProbeMode == nil {
		mode := core.DurationProbe
		cfg.ProbeMode = &mode
	}
	if cfg.newDevice == nil {
		backend := cfg.Backend
		cfg.newDevice = func() (playback.Device, error) { return playback.Open(backend) }
	}

	s := &Session{
		cfg:     cfg,
		gain:    dsp.NewGain(cfg.Volume),
		tap:     newTap(),
		events:  newEventQueue(cfg.EventBuffer),
		v:       view{state: StateIdle, queueIndex: -1},
		wake:    make(chan struct{}, 1),
		closing: make(chan struct{}),
		stopped: make(chan struct{}),
		feed:    make(chan feedEvent, 8),
		openCh:  make(chan openResult, 4),
		probeCh: make(chan probeResult, 4),
		metaCh:  make(chan metaResult, 4),
		seekCh:  make(chan seekResult, 1),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.provider = newRoutedProvider(s.gain, s.tap, s.feed, s.closing)

	go s.run()

	return s, nil
}

// probeWithDefaultRegistry asks the process-wide decoder registry for a
// duration probe.
func probeWithDefaultRegistry(path string, opts decode.ProbeOptions) (core.StreamInfo, error) {
	p, ok := decode.Default.Probe(path)
	if !ok {
		return core.StreamInfo{Format: canonical, TotalFrames: -1}, ErrNoProber
	}

	return p.Probe(path, opts)
}

// Snapshot reports the controller state. It is safe to call from any
// goroutine and is cheap enough to poll every frame.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	v := s.v
	s.mu.Unlock()

	pos := v.lastPos
	var stats stream.Stats
	if live := s.provider.current(); live != nil {
		// Position is converted through the streamer's one helper, so no
		// caller can pair a native-rate count with the output clock.
		pos = stream.FramesToDuration(live.Position())
		stats = live.Stats()
	}

	return Snapshot{
		State:         v.state,
		Path:          v.path,
		Meta:          v.meta,
		Position:      pos,
		Duration:      v.duration,
		Volume:        s.gain.Volume(),
		Format:        canonical,
		QueueIndex:    v.queueIndex,
		QueueLen:      v.queueLen,
		Stats:         stats,
		Decoder:       v.decoder,
		Parser:        v.parser,
		DroppedEvents: s.events.droppedCount(),
	}
}

// Events is the stream of controller events. It never blocks the engine: a
// producer that outruns this channel costs the oldest event, counted in
// Snapshot.DroppedEvents.
func (s *Session) Events() <-chan Event { return s.events.channel() }

// Play replaces the queue with one track and starts it.
func (s *Session) Play(path string) error {
	if path == "" {
		return ErrEmptyPath
	}
	s.enqueue(command{kind: cmdPlay, path: path})

	return nil
}

// PlayQueue replaces the queue and starts at the first track.
func (s *Session) PlayQueue(paths []string) error {
	if len(paths) == 0 {
		return ErrEmptyQueue
	}
	s.enqueue(command{kind: cmdPlayQueue, paths: append([]string(nil), paths...)})

	return nil
}

// Next advances within the queue. Past the last track it stops.
func (s *Session) Next() error {
	s.enqueue(command{kind: cmdNext})

	return nil
}

// Prev goes back one track; on the first track it restarts the current one.
func (s *Session) Prev() error {
	s.enqueue(command{kind: cmdPrev})

	return nil
}

// Queue returns a copy of the current queue.
func (s *Session) Queue() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.queue...)
}

// Pause stops the device. It is ignored unless the session is playing.
func (s *Session) Pause() error {
	s.enqueue(command{kind: cmdPause})

	return nil
}

// Resume restarts a paused device. It is ignored unless the session is paused.
func (s *Session) Resume() error {
	s.enqueue(command{kind: cmdResume})

	return nil
}

// Stop ends playback and keeps the queue. The device stays open, so the next
// Play reuses it.
func (s *Session) Stop() error {
	s.enqueue(command{kind: cmdStop})

	return nil
}

// Seek repositions the current track once the device has been paused, so the
// device never reads a half-flushed ring.
func (s *Session) Seek(d time.Duration) error {
	if d < 0 {
		return ErrNegativeSeek
	}
	s.enqueue(command{kind: cmdSeek, pos: d})

	return nil
}

// SetVolume changes the post-ring gain. It is atomic and takes effect on the
// next buffer, so it is safe to call while audio is running.
func (s *Session) SetVolume(v float64) { s.gain.SetVolume(v) }

// Tap is the real-time audio feed a visualizer reads. The engine writes mono
// downmixed frames after the gain and before the device, so a consumer sees
// exactly what is heard.
type Tap interface {
	// Read copies up to len(dst) mono frames in time order. It never blocks
	// and reports zero when nothing is buffered.
	Read(dst []float32) int
}

// Tap returns the session's visualizer feed. The feed becomes live on the
// first Read, so holding a Tap without consuming it costs the audio path
// nothing. The returned Tap stays valid across tracks and after Stop.
func (s *Session) Tap() Tap {
	return s.tap
}

// Close stops every goroutine and releases the device and streamer. It is
// idempotent and safe to call from any goroutine.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		close(s.closing)
		s.cancel()
		<-s.stopped
	})

	return nil
}

// enqueue appends a command and wakes the control loop. The wake is a
// non-blocking signal: a pending wake is as good as a new one because the loop
// drains the whole inbox.
func (s *Session) enqueue(c command) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return
	}
	s.inbox = append(s.inbox, c)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run is the one control goroutine. It only ever does bounded work: the slow
// steps arrive as results on channels.
func (s *Session) run() {
	defer close(s.stopped)

	ticker := time.NewTicker(devicePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.closing:
			s.shutdown()

			return
		case <-s.wake:
			s.runCommands()
		case ev := <-s.feed:
			s.handleFeed(ev)
		case res := <-s.openCh:
			s.handleOpen(res)
		case res := <-s.probeCh:
			s.handleProbe(res)
		case res := <-s.metaCh:
			s.handleMeta(res)
		case res := <-s.seekCh:
			s.handleSeek(res)
		case <-ticker.C:
			s.pollDevice()
		}
	}
}

// runCommands applies the inbox in order. Commands are cheap; nothing here
// waits on a file or a device, which is what keeps the loop responsive.
func (s *Session) runCommands() {
	s.mu.Lock()
	cmds := s.inbox
	s.inbox = nil
	s.mu.Unlock()

	for _, c := range cmds {
		s.apply(c)
	}
}

func (s *Session) apply(c command) {
	switch c.kind {
	case cmdPlay:
		s.setQueue([]string{c.path})
		s.startIndex(0)
	case cmdPlayQueue:
		s.setQueue(c.paths)
		s.startIndex(0)
	case cmdNext:
		s.next()
	case cmdPrev:
		s.prev()
	case cmdPause:
		if s.state() == StatePlaying && s.device != nil {
			_ = s.device.Pause()
			s.setState(StatePaused)
		}
	case cmdResume:
		if s.state() == StatePaused && s.device != nil {
			_ = s.device.Resume()
			s.setState(StatePlaying)
		}
	case cmdStop:
		if st := s.state(); st == StatePlaying || st == StatePaused {
			s.stopToStopped()
		}
	case cmdSeek:
		s.requestSeek(c.pos)
	}
}

// next advances the queue, or stops at the end. It is the command path; the
// natural end path shares startIndex but not the stop decision, because a
// track that merely ended has no more tracks by definition.
func (s *Session) next() {
	if len(s.queue) == 0 {
		return
	}
	if s.index+1 < len(s.queue) {
		s.startIndex(s.index + 1)

		return
	}
	s.stopToStopped()
}

// prev steps back, restarting the first track instead of leaving the queue.
func (s *Session) prev() {
	if len(s.queue) == 0 {
		return
	}
	if s.index > 0 {
		s.startIndex(s.index - 1)

		return
	}
	// Already on the first track: restart it if one is loaded, otherwise
	// start the queue. A UI Prev at the top of a list means "play this again".
	if s.provider.current() == nil {
		s.startIndex(0)

		return
	}
	s.requestSeek(0)
}

// setQueue replaces the queue under the lock Queue reads under. The control
// goroutine is the only writer, but Queue is public and may be called from any
// goroutine, so the write must be synchronised.
func (s *Session) setQueue(paths []string) {
	s.mu.Lock()
	s.queue = paths
	s.mu.Unlock()
}

// startIndex tears the current track down and opens index asynchronously.
func (s *Session) startIndex(index int) {
	if index < 0 || index >= len(s.queue) {
		return
	}
	path := s.queue[index]
	s.index = index
	s.detach()
	s.seq++

	s.updateView(func(v *view) {
		v.path = path
		v.meta = meta.Meta{Path: path}
		v.duration = 0
		v.lastPos = 0
		v.decoder = ""
		v.parser = ""
		v.queueIndex = index
		v.queueLen = len(s.queue)
	})

	s.openTrack(s.seq, index, path)
}

// detach drops the current stream and parks the device. The device is not
// closed: it is the resource the next track reuses.
func (s *Session) detach() {
	s.retireLive()
	if s.device != nil && s.deviceStarted {
		_ = s.device.Pause()
	}
}

// retireLive stops the provider from reading the current streamer and closes
// it. Setting the provider to nil first means an in-flight read returns
// silence rather than the ErrClosed that a planned close produces.
func (s *Session) retireLive() {
	s.inFlight = false
	// Any target that has not started belongs to the stream being retired, so
	// it is dropped rather than applied to the next track. An in-flight worker
	// is left alone; its stale result is discarded in handleSeek.
	s.seekPending = nil
	if s.live == nil {
		return
	}
	pos := stream.FramesToDuration(s.live.Position())
	s.updateView(func(v *view) { v.lastPos = pos })

	s.provider.setCurrent(nil)
	_ = s.live.Close()
	s.live = nil
}

// openTrack builds the pipeline for one file off the control goroutine. The
// result is buffered, so a worker never blocks on the control loop; the
// closing case covers a send after shutdown.
func (s *Session) openTrack(seq uint64, index int, path string) {
	s.workerWG.Add(1)

	go func() {
		defer s.workerWG.Done()

		res := s.build(seq, index, path)
		select {
		case s.openCh <- res:
		case <-s.closing:
			if res.streamer != nil {
				_ = res.streamer.Close()
			}
		}
	}()
}

// build creates and starts a streamer over the path. The decoder's stream
// info is captured here because only the decoder knows it. An opener that runs
// after Close is refused, so shutdown cannot be extended by a new file open.
func (s *Session) build(seq uint64, index int, path string) openResult {
	select {
	case <-s.closing:
		return openResult{seq: seq, index: index, path: path, err: ErrClosed}
	default:
	}

	var info core.StreamInfo
	var decoderName, parserName string
	open := func(<-chan struct{}) (decode.Decoder, error) {
		select {
		case <-s.closing:
			return nil, ErrClosed
		default:
		}

		d, err := s.cfg.openDecoder(path)
		if err != nil {
			return nil, err
		}
		info = d.Info()
		decoderName, parserName = decode.Describe(d)

		return d, nil
	}

	st, err := stream.New(open, stream.Config{RingFrames: s.cfg.RingFrames})
	if err != nil {
		return openResult{seq: seq, index: index, path: path, err: err}
	}
	if err := st.Start(s.ctx); err != nil {
		_ = st.Close()

		return openResult{seq: seq, index: index, path: path, err: err}
	}

	return openResult{
		seq: seq, index: index, path: path,
		streamer: st, info: info, decoder: decoderName, parser: parserName,
	}
}

// handleOpen installs a finished build, or discards it when the queue has
// already moved on.
func (s *Session) handleOpen(res openResult) {
	if res.seq != s.seq {
		// The queue moved on while this build ran, so the result is stale. A
		// successful build owns a live streamer that must be closed; a failed
		// one has nothing to release.
		if res.streamer != nil {
			_ = res.streamer.Close()
		}

		return
	}
	if res.err != nil {
		s.fail(res.err, false)

		return
	}

	s.activate(res)
}

// activate points the device at the new streamer. The device is opened lazily
// on the first track and reused from then on.
func (s *Session) activate(res openResult) {
	if s.device == nil {
		dev, err := s.cfg.newDevice()
		if err != nil {
			_ = res.streamer.Close()
			s.fail(err, false)

			return
		}
		if err := dev.Open(canonical, s.provider); err != nil {
			_ = dev.Close()
			_ = res.streamer.Close()
			s.fail(err, false)

			return
		}
		s.device = dev
	}

	s.live = res.streamer
	s.provider.setCurrent(res.streamer)
	s.inFlight = true

	s.updateView(func(v *view) {
		v.path = res.path
		v.meta = meta.Meta{Path: res.path, Stream: res.info}
		v.duration = 0
		v.decoder = res.decoder
		v.parser = res.parser
		v.queueIndex = res.index
		v.queueLen = len(s.queue)
	})

	if !s.deviceStarted {
		if err := s.device.Start(); err != nil {
			s.fail(err, true)

			return
		}
		s.deviceStarted = true
	} else if err := s.device.Resume(); err != nil {
		s.fail(err, true)

		return
	}

	s.setState(StatePlaying)
	s.emit(TrackChanged{Index: res.index, Path: res.path})
	s.enrich(res.seq, res.index, res.path)
}

// enrich starts the asynchronous metadata and duration lookups. Play never
// waits for either: the snapshot reports an unknown duration until the probe
// lands. These workers own no engine resource, so Close does not wait for
// them; their channel sends are released by the closing signal.
func (s *Session) enrich(seq uint64, index int, path string) {
	go func() {
		info, err := s.cfg.probeStream(path, decode.ProbeOptions{Duration: *s.cfg.ProbeMode})
		select {
		case s.probeCh <- probeResult{seq: seq, index: index, info: info, err: err}:
		case <-s.closing:
		}
	}()

	go func() {
		m, err := s.cfg.Resolver.Resolve(s.ctx, path)
		select {
		case s.metaCh <- metaResult{seq: seq, index: index, m: m, err: err}:
		case <-s.closing:
		}
	}()
}

// handleProbe records a duration for the track it was started for. A probe for
// a track that is no longer current is discarded.
func (s *Session) handleProbe(res probeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if res.seq != s.seq || res.index != s.v.queueIndex {
		return
	}

	s.v.duration = res.info.Duration()
}

// handleMeta records resolved tags, merging in the stream info the decoder
// reported: only the decoder can describe the stream truthfully.
func (s *Session) handleMeta(res metaResult) {
	if res.err != nil || res.m == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if res.seq != s.seq || res.index != s.v.queueIndex {
		return
	}

	merged := *res.m
	merged.Stream = s.v.meta.Stream
	s.v.meta = merged
}

// handleFeed reacts to the provider's view of the current stream. Events for a
// stream that is no longer live are stale and ignored.
func (s *Session) handleFeed(ev feedEvent) {
	if ev.stream == nil || ev.stream != s.live {
		return
	}

	switch ev.kind {
	case feedExhausted:
		s.emit(TrackEnded{})
		s.retireLive()

		if s.index+1 < len(s.queue) {
			s.startIndex(s.index + 1)

			return
		}
		s.setState(StateStopped)
		if s.device != nil && s.deviceStarted {
			_ = s.device.Pause()
		}
	case feedError:
		s.fail(ev.err, true)
	}
}

// pollDevice surfaces an asynchronous backend failure. It only runs while a
// track is active, so a device that errored after a planned stop cannot
// produce a spurious Failed.
func (s *Session) pollDevice() {
	if !s.inFlight || s.device == nil {
		return
	}
	if err := s.device.Err(); err != nil {
		s.fail(err, true)
	}
}

// fail tears the pipeline down after a decode or device error and reports it.
// closeDevice is false for an open failure, where the existing device is still
// healthy and can be reused by the next Play. Bumping seq discards any build
// still in flight so a stale track cannot activate after the failure.
func (s *Session) fail(err error, closeDevice bool) {
	s.seq++
	s.retireLive()

	if closeDevice && s.device != nil {
		_ = s.device.Pause()
		_ = s.device.Close()
		s.device = nil
		s.deviceStarted = false
	}

	s.emit(Failed{Err: err})
	s.setState(StateStopped)
}

// stopToStopped ends playback on request or at the end of the queue. The
// device stays open, paused, ready for the next track. Bumping seq discards any
// build that is still in flight, so a Stop followed by a stale open cannot
// resurrect a track the user already stopped.
func (s *Session) stopToStopped() {
	s.seq++
	s.retireLive()
	if s.device != nil && s.deviceStarted {
		_ = s.device.Pause()
	}
	s.setState(StateStopped)
}

// requestSeek accepts a seek from the control loop without performing it. The
// device is parked here, so the origin is frozen and no device read races the
// worker's flush, and the intent to resume is decided now. The reposition then
// runs on a worker; the control loop keeps serving commands and events.
func (s *Session) requestSeek(d time.Duration) {
	live := s.live
	if live == nil {
		return
	}

	if s.device != nil {
		_ = s.device.Pause()
	}
	// Capture the origin once the device is parked, so the reported position is
	// the frame the seek actually started from rather than a moving target.
	req := &seekRequest{
		seq:    s.seq,
		stream: live,
		target: d,
		from:   stream.FramesToDuration(live.Position()),
		resume: s.state() == StatePlaying,
	}

	if s.seekBusy {
		// Latest-wins: overwrite the target that has not started yet. The
		// in-flight worker is not disturbed; its completion starts this one.
		s.seekPending = req

		return
	}

	s.startSeek(req)
}

// startSeek hands one accepted request to a worker. At most one worker runs at
// a time, so two repositions never share a streamer. The worker owns the
// SeekFrame call and reports back on seekCh; it must not touch session state.
func (s *Session) startSeek(req *seekRequest) {
	s.seekBusy = true
	s.workerWG.Add(1)

	go func() {
		defer s.workerWG.Done()

		start := time.Now()
		err := req.stream.SeekFrame(framesFor(req.target))
		res := seekResult{
			seq:     req.seq,
			stream:  req.stream,
			target:  req.target,
			from:    req.from,
			resume:  req.resume,
			elapsed: time.Since(start),
			err:     err,
		}
		select {
		case s.seekCh <- res:
		case <-s.closing:
		}
	}()
}

// startPendingSeek launches the newest not-yet-started target, if any, and
// reports whether it did. A pending request for a track that has since moved
// on is stale and dropped.
func (s *Session) startPendingSeek() bool {
	req := s.seekPending
	s.seekPending = nil
	if req == nil {
		return false
	}
	if req.stream != s.live || req.seq != s.seq {
		return false
	}
	s.startSeek(req)

	return true
}

// handleSeek applies a finished reposition. A result whose track is no longer
// live is stale and ignored whole: it must not move the new track or resume a
// device the user asked to leave alone. The device is resumed only when the
// seek intended it and the session is still playing.
func (s *Session) handleSeek(res seekResult) {
	s.seekBusy = false

	if res.stream != s.live || res.seq != s.seq {
		s.startPendingSeek()

		return
	}

	if res.err != nil {
		s.emit(Failed{Err: res.err})
		s.startPendingSeek()

		return
	}

	// A newer target replaced this one, so stay parked until it lands: a resumed
	// device would be handed a half-flushed ring by the next reposition.
	pending := s.startPendingSeek()
	if !pending && res.resume && s.state() == StatePlaying && s.device != nil {
		_ = s.device.Resume()
	}

	s.emit(Seeked{Position: res.target, From: res.from, Elapsed: res.elapsed})
}

// shutdown is the single teardown path. It runs on the control goroutine, so
// no other engine mutation can race it. Builds that were in flight are waited
// out and any streamer they delivered is closed, so nothing survives Close.
func (s *Session) shutdown() {
	s.retireLive()
	if s.device != nil {
		_ = s.device.Pause()
		_ = s.device.Close()
		s.device = nil
		s.deviceStarted = false
	}

	s.workerWG.Wait()
	for {
		select {
		case res := <-s.openCh:
			if res.streamer != nil {
				_ = res.streamer.Close()
			}
		default:
			s.events.close()

			return
		}
	}
}

// setState applies a transition if it is legal and emits the change. An
// illegal transition is a no-op, never a panic: commands arrive from a UI and
// a wrong one must not take the engine down.
func (s *Session) setState(to State) {
	s.mu.Lock()
	from := s.v.state
	if from == to || !canTransition(from, to) {
		s.mu.Unlock()

		return
	}
	s.v.state = to
	s.mu.Unlock()

	s.emit(StateChanged{From: from, To: to})
}

func (s *Session) state() State {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.v.state
}

func (s *Session) updateView(fn func(*view)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.v)
}

func (s *Session) emit(ev Event) { s.events.push(ev) }

// framesFor converts a duration into the streamer's output-frame domain. It is
// the inverse of stream.FramesToDuration and lives here so the rate used for
// seeking is the same constant the position helper uses.
func framesFor(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}

	return int64(d) * int64(canonical.Rate) / int64(time.Second)
}
