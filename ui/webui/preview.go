package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/dsp"
)

// Preview defaults: a 30 second window from the start of the track, with a
// 300 ms fade at each edge, looping until the selection changes.
const (
	defaultPreviewLengthMs = 30000
	defaultPreviewFadeMs   = 300
	previewTick            = 20 * time.Millisecond
	// previewLoadTimeout bounds how long a Play may take to produce a live
	// track before the preview gives up. The engine builds a track on a worker,
	// so "not playing yet" is normal for a moment; a build that never lands is
	// not.
	previewLoadTimeout = 3 * time.Second
	// previewSeekTimeout bounds how long a window start may take to land. The
	// pure-Go decoder's seek cost is proportional to its target, so a far start
	// is slow; past this the preview sounds from wherever the stream is rather
	// than hanging silently.
	previewSeekTimeout = 2 * time.Second
)

// PreviewConfig is the preview window. StartMs and LengthMs are milliseconds
// from the start of the track; a LengthMs of 0 means "to the end". FadeMs is
// the ramp at each edge, Loop repeats the window until the selection changes,
// and Volume is the preview's own level in [0, 1].
type PreviewConfig struct {
	StartMs  int64   `json:"startMs"`
	LengthMs int64   `json:"lengthMs"`
	FadeMs   int64   `json:"fadeMs"`
	Loop     bool    `json:"loop"`
	Volume   float64 `json:"volume"`
}

// PreviewState is the preview's contribution to the snapshot: which row is
// sounding, how far into the window, and whether it loops. Active is false
// when no preview is running, which is the signal to draw nothing.
type PreviewState struct {
	Active   bool   `json:"active"`
	Path     string `json:"path"`
	Position int64  `json:"position"`
	Duration int64  `json:"duration"`
	Loop     bool   `json:"loop"`
}

// normalizePreviewConfig clamps a config into the ranges the UI and the fade
// effect can honour, so a bad value cannot wedge the session. Volume 0 is a
// legitimate mute, so only a negative or above-unity value is corrected.
func normalizePreviewConfig(c PreviewConfig) PreviewConfig {
	if c.StartMs < 0 {
		c.StartMs = 0
	}
	if c.LengthMs < 0 {
		c.LengthMs = 0
	}
	switch {
	case c.FadeMs < 0:
		c.FadeMs = 0
	case c.FadeMs > 5000:
		c.FadeMs = 5000
	}
	switch {
	case c.Volume < 0:
		c.Volume = 0
	case c.Volume > 1:
		c.Volume = 1
	}

	return c
}

// previewPhase is the previewer's state machine. It exists because the engine's
// commands are asynchronous: Play does not mean "playing", and Seek does not
// mean "at the target" until a worker lands it. Every phase is a thing we are
// waiting for, checked on the tick.
type previewPhase uint8

const (
	// phaseIdle means nothing is previewing.
	phaseIdle previewPhase = iota
	// phaseLoading means a Play has been issued and the track is not live yet.
	phaseLoading
	// phasePositioning means the track is live but muted while it is driven to
	// the window start. Output stays silent through this phase, which is what
	// keeps the head of the track from being heard before the seek lands.
	phasePositioning
	// phaseSounding means the window is audible.
	phaseSounding
	// phaseFading means an audible fade-out is running before the next action.
	phaseFading
)

// previewAction is what to do once a fade-out completes: load a new window, or
// end the preview. Both empty means settle to idle.
type previewAction struct {
	play string
	stop bool
}

// previewer plays a short window of the highlighted palette row on a second
// player, so the main track can stay loaded and paused underneath and resume
// instantly. One goroutine owns the second player and drives the window, the
// loop and the fades on a tick.
//
// The fade is a real dsp effect in the second player's chain, not a volume
// ramp: the master gain applies once per ~100 ms buffer, which would click
// rather than fade. During positioning the same effect is installed with a
// zero-length fade-out, which holds the output at silence while the engine's
// asynchronous seek lands; that is what stops the head of a track from being
// heard before a non-zero start-at takes effect.
type previewer struct {
	svc    *PlayerService
	player molo.Player

	wake    chan struct{}
	closing chan struct{}
	done    chan struct{}

	// mu guards everything a caller thread can touch: the config, the
	// UI-facing state, and the newest unprocessed intent. halt wins over a
	// pending play, so a stop is never lost to a queued selection change.
	mu          sync.Mutex
	cfg         PreviewConfig
	active      bool
	path        string
	pos         int64
	dur         int64
	pendingPlay *string
	pendingHalt bool

	// The fields below are owned by the preview goroutine, so they need no
	// lock. sessionOn is true while a preview session holds the main track
	// paused; resumeMain records whether the main track was found playing and
	// paused by us, so one the user had already paused is left paused.
	sessionOn  bool
	resumeMain bool

	phase        atomic.Uint32
	playIssuedAt time.Time
	seekTarget   int64
	seekDeadline time.Time
	fadeDeadline time.Time
	pending      previewAction
}

// phaseNow reads the current phase. It is an atomic read so a test (or a
// future UI) can observe the state machine without a lock.
func (p *previewer) phaseNow() previewPhase {
	return previewPhase(p.phase.Load())
}

// setPhase publishes the current phase.
func (p *previewer) setPhase(ph previewPhase) {
	p.phase.Store(uint32(ph))
}

func newPreviewer(svc *PlayerService, p molo.Player) *previewer {
	return &previewer{
		svc:     svc,
		player:  p,
		wake:    make(chan struct{}, 1),
		closing: make(chan struct{}),
		done:    make(chan struct{}),
		cfg: PreviewConfig{
			LengthMs: defaultPreviewLengthMs,
			FadeMs:   defaultPreviewFadeMs,
			Loop:     true,
			Volume:   1,
		},
	}
}

func (p *previewer) start() {
	go p.run()
}

// close stops the goroutine and releases the second molo. It is idempotent
// from the caller's side because ServiceShutdown runs once.
func (p *previewer) close() {
	close(p.closing)
	<-p.done
	if p.player != nil {
		_ = p.player.Close()
	}
}

// play starts, or replaces, the preview of path. It is non-blocking.
func (p *previewer) play(path string) {
	p.mu.Lock()
	p.pendingPlay = &path
	p.pendingHalt = false
	p.mu.Unlock()
	p.signal()
}

// halt ends the preview and resumes the main track if this session paused it.
func (p *previewer) halt() {
	p.mu.Lock()
	p.pendingPlay = nil
	p.pendingHalt = true
	p.mu.Unlock()
	p.signal()
}

// configure replaces the preview settings. Volume applies at once (the gain is
// atomic and safe to set from any thread); a Start-at change re-arms the
// current selection, because the window it names has moved.
func (p *previewer) configure(cfg PreviewConfig) {
	cfg = normalizePreviewConfig(cfg)

	p.mu.Lock()
	startChanged := cfg.StartMs != p.cfg.StartMs
	p.cfg = cfg
	path := p.path
	active := p.active
	p.mu.Unlock()

	if p.player != nil {
		p.player.SetVolume(cfg.Volume)
	}

	if startChanged && active && path != "" {
		p.play(path)
	}
}

// config reports the preview settings in force.
func (p *previewer) config() PreviewConfig {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.cfg
}

// state reports the preview's UI-facing state.
func (p *previewer) state() PreviewState {
	p.mu.Lock()
	defer p.mu.Unlock()

	return PreviewState{
		Active:   p.active,
		Path:     p.path,
		Position: p.pos,
		Duration: p.dur,
		Loop:     p.cfg.Loop,
	}
}

// signal wakes the goroutine without blocking. A pending wake is as good as a
// new one because the goroutine drains the whole intent on wake.
func (p *previewer) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *previewer) run() {
	defer close(p.done)

	ticker := time.NewTicker(previewTick)
	defer ticker.Stop()

	for {
		select {
		case <-p.closing:
			p.shutdown()

			return
		case <-p.wake:
			p.drain()
		case <-ticker.C:
			p.tick()
		}
	}
}

// drain applies the newest unprocessed intent. A halt always wins, so a stop
// can never be lost to a burst of selection changes.
func (p *previewer) drain() {
	p.mu.Lock()
	halt := p.pendingHalt
	var play *string
	if !halt {
		play = p.pendingPlay
	}
	p.pendingHalt = false
	p.pendingPlay = nil
	p.mu.Unlock()

	switch {
	case halt:
		p.requestStop()
	case play != nil:
		p.requestPlay(*play)
	}
}

func (p *previewer) tick() {
	// A fade-out in progress completes on its own deadline, then the queued
	// action runs. The ticker, not a sleep, drives it, so the goroutine stays
	// responsive while a long fade plays out.
	if p.phaseNow() == phaseFading {
		if time.Now().Before(p.fadeDeadline) {
			return
		}
		p.finishFade()

		return
	}

	// While a session holds the main track, keep it from sounding. This is a
	// re-check, not a one-shot: the main player may have been mid-build when
	// the session began and only reached Playing afterwards.
	p.ensureMainPaused()

	if p.phaseNow() == phaseIdle || p.player == nil {
		return
	}

	snap := p.player.Snapshot()

	switch p.phaseNow() {
	case phaseLoading:
		switch snap.State {
		case molo.Playing, molo.Paused:
			p.toPositioning()
		case molo.Stopped, molo.Idle:
			if time.Since(p.playIssuedAt) > previewLoadTimeout {
				p.abort()
			}
		}
	case phasePositioning:
		if snap.State != molo.Playing && snap.State != molo.Paused {
			if time.Since(p.playIssuedAt) > previewLoadTimeout {
				p.abort()
			}

			return
		}
		if p.seekTarget > 0 {
			landed := snap.Position >= time.Duration(p.seekTarget)*time.Millisecond
			if !landed && time.Now().Before(p.seekDeadline) {
				return
			}
			p.seekTarget = 0
		}
		p.toSounding()
	case phaseSounding:
		if snap.State == molo.Stopped || snap.State == molo.Idle {
			p.windowExhausted()

			return
		}
		p.publishPosition(snap.Position)
		cfg := p.config()
		if cfg.LengthMs > 0 && snap.Position >= time.Duration(cfg.StartMs+cfg.LengthMs)*time.Millisecond {
			p.windowExhausted()
		}
	}
}

// requestPlay begins the session and loads the window. A sounding or
// already-fading preview fades out first so the switch is smooth; a silent one
// is replaced at once.
func (p *previewer) requestPlay(path string) {
	p.beginSession()
	switch p.phaseNow() {
	case phaseSounding, phaseFading:
		p.startFade(previewAction{play: path})
	default:
		p.load(path)
	}
}

// requestStop ends the preview, fading a sounding window out first.
func (p *previewer) requestStop() {
	switch p.phaseNow() {
	case phaseIdle:
		p.endSession()
	case phaseSounding:
		p.startFade(previewAction{stop: true})
	default:
		// Loading, positioning or already fading: the output is silent, so
		// stop the second player now rather than leave a decoder running.
		if p.player != nil {
			_ = p.player.Stop()
		}
		p.pending = previewAction{}
		p.settle()
		p.endSession()
	}
}

// beginSession pauses the main track and arms the per-tick re-check that keeps
// it paused, because the main player's state can change asynchronously.
func (p *previewer) beginSession() {
	if p.sessionOn {
		return
	}
	p.sessionOn = true
	p.ensureMainPaused()
}

// ensureMainPaused pauses the main player whenever it is found playing during a
// session, and remembers that the session paused it so it can be resumed. A
// main track the user had already paused is never resumed.
func (p *previewer) ensureMainPaused() {
	if !p.sessionOn || p.svc == nil || p.svc.player == nil {
		return
	}
	if p.svc.player.Snapshot().State == molo.Playing {
		_ = p.svc.player.Pause()
		p.resumeMain = true
	}
}

// endSession resumes the main track if the session paused it.
func (p *previewer) endSession() {
	if !p.sessionOn {
		return
	}
	if p.resumeMain && p.svc != nil && p.svc.player != nil {
		_ = p.svc.player.Resume()
	}
	p.sessionOn = false
	p.resumeMain = false
}

// load starts path and drives it to the window start. The output is gated
// silent for the whole load, so the head of the track is never audible.
func (p *previewer) load(path string) {
	if p.player == nil {
		return
	}
	cfg := p.config()

	p.setPhase(phaseLoading)
	p.playIssuedAt = time.Now()
	p.seekTarget = 0
	p.pending = previewAction{}

	// The gate is the fade effect, not the master gain: a zero-length fade-out
	// holds the output at silence and is swapped for a fade-in once the window
	// is positioned. Using the gain would click when it is raised.
	p.setFade(dsp.FadeModeOut, 0)
	p.player.SetVolume(cfg.Volume)
	_ = p.player.Play(path)

	p.mu.Lock()
	p.active = true
	p.path = path
	p.pos = 0
	p.dur = cfg.LengthMs
	p.mu.Unlock()
}

// toPositioning seeks to the window start while the gate holds silence. A
// zero start needs no seek and goes straight to sounding.
func (p *previewer) toPositioning() {
	cfg := p.config()
	p.setPhase(phasePositioning)
	p.seekTarget = cfg.StartMs
	p.seekDeadline = time.Now().Add(previewSeekTimeout)
	if cfg.StartMs > 0 {
		_ = p.player.Seek(time.Duration(cfg.StartMs) * time.Millisecond)
	}
}

// toSounding opens the gate with a fade-in, so the window breathes in at its
// start rather than beginning mid-air.
func (p *previewer) toSounding() {
	cfg := p.config()
	p.setPhase(phaseSounding)
	p.setFade(dsp.FadeModeIn, cfg.FadeMs)
}

// windowExhausted loops the window back to its start, or ends the preview and
// resumes the main track when looping is off.
func (p *previewer) windowExhausted() {
	cfg := p.config()
	if !cfg.Loop {
		p.requestStop()

		return
	}
	if p.player == nil {
		return
	}

	// Gate closed, then rewind and fade in again. The rewind is a seek when the
	// track is still live; a track that ended is replayed from the top, still
	// gated.
	p.setFade(dsp.FadeModeOut, 0)
	p.playIssuedAt = time.Now()
	p.seekTarget = cfg.StartMs
	p.seekDeadline = time.Now().Add(previewSeekTimeout)

	if snap := p.player.Snapshot(); snap.State == molo.Playing || snap.State == molo.Paused {
		p.setPhase(phasePositioning)
		_ = p.player.Seek(time.Duration(cfg.StartMs) * time.Millisecond)
	} else {
		p.mu.Lock()
		path := p.path
		p.mu.Unlock()
		if path == "" {
			p.settle()

			return
		}
		p.setPhase(phaseLoading)
		_ = p.player.Play(path)
	}

	p.mu.Lock()
	p.pos = 0
	p.mu.Unlock()
}

// startFade begins an audible fade-out toward the queued action. A fade already
// in flight keeps its ramp and only swaps the action, so a rapid burst does not
// re-arm the ramp from unity and click.
func (p *previewer) startFade(action previewAction) {
	p.pending = action
	if p.phaseNow() == phaseFading {
		return
	}
	if p.player == nil {
		p.finishFade()

		return
	}
	cfg := p.config()
	if cfg.FadeMs <= 0 {
		p.finishFade()

		return
	}
	p.setFade(dsp.FadeModeOut, cfg.FadeMs)
	p.fadeDeadline = time.Now().Add(time.Duration(cfg.FadeMs) * time.Millisecond)
	p.setPhase(phaseFading)
}

// finishFade stops the second player and runs the queued action.
func (p *previewer) finishFade() {
	if p.player != nil {
		_ = p.player.Stop()
	}
	action := p.pending
	p.pending = previewAction{}

	switch {
	case action.stop:
		p.settle()
		p.endSession()
	case action.play != "":
		p.load(action.play)
	default:
		p.settle()
	}
}

// abort gives up on a load that never produced a live track.
func (p *previewer) abort() {
	if p.player != nil {
		_ = p.player.Stop()
	}
	p.settle()
	p.endSession()
}

// settle returns the state machine to idle without touching the main track.
func (p *previewer) settle() {
	p.setPhase(phaseIdle)
	p.seekTarget = 0

	p.mu.Lock()
	p.active = false
	p.path = ""
	p.pos = 0
	p.mu.Unlock()
}

// shutdown stops everything for process teardown. The main track is resumed if
// the session paused it, so closing the app mid-preview does not leave it
// paused for the next run.
func (p *previewer) shutdown() {
	if p.player != nil {
		_ = p.player.Stop()
	}
	p.settle()
	p.endSession()
}

// setFade installs a single fade stage in the second player's chain with an
// explicit duration. The effect is built fresh, so a fade-in always starts from
// silence and a zero-length fade-out holds the output silent.
func (p *previewer) setFade(mode string, durationMs int64) {
	if p.player == nil {
		return
	}
	_ = p.player.ApplyPipeline(dsp.Pipeline{Post: []dsp.Spec{{
		Kind:   "fade",
		Params: dsp.Values{"mode": mode, "duration": float64(durationMs)},
	}}})
}

// publishPosition mirrors the sounding position into the UI state, relative to
// the window start so the palette can draw a 0..Length progress bar.
func (p *previewer) publishPosition(abs time.Duration) {
	cfg := p.config()
	pos := abs.Milliseconds() - cfg.StartMs
	if pos < 0 {
		pos = 0
	}

	p.mu.Lock()
	p.pos = pos
	p.dur = cfg.LengthMs
	p.mu.Unlock()
}
