package session

import (
	"github.com/dlcuy22/molo/stream"
)

// prefetchMarginMs is how far before the crossfade window the next track's
// build starts. Building opens a decoder, starts its producer and fills its
// ring, which can be slow for a network source, so the margin is generous. If
// the track is not ready in time the handoff falls back to the ordinary
// end-of-track path, so a slow source costs the blend, never the audio.
const prefetchMarginMs = 4000

// crossfadePrebufferMs is the incoming track's ring level required before the
// blend starts. Starting a fade-in into a cold stream would clip the head of
// the incoming track; the producer fills the ring as soon as the track is
// built, so this is normally satisfied long before the ramp. The value is well
// under the default ring so a custom, smaller ring still blends.
const crossfadePrebufferMs = 100

// pollCrossfade advances the crossfade state machine. It runs on the control
// tick while a track is live. The engine's position and the probed duration
// decide when to build the next track early and when to start the blend; every
// other transition (a manual Next, a seek, a stop) cancels the whole thing.
func (s *Session) pollCrossfade() {
	if s.xfading {
		return
	}

	// A prefetch always targets index+1, which is the wrong track under repeat
	// one: the end-of-track wants the current track again. So repeat one never
	// builds or blends into a next track. The consequence is that repeat one
	// plays as a hard restart, with no blend, which is the mode's documented
	// behaviour rather than a missed optimisation.
	if s.runtime.repeatSettings() == RepeatOne {
		s.cancelPrefetch()

		return
	}

	cfg := s.runtime.crossfadeSettings()
	if !cfg.Enabled {
		// A disabled crossfade must not leave a prefetch in flight: a track
		// built for a blend that will never happen would be opened for nothing.
		s.cancelPrefetch()

		return
	}

	live := s.live
	if live == nil || s.device == nil || !s.deviceStarted {
		return
	}
	// A seek, decoder swap or backend swap is repositioning or rebuilding the
	// live streamer on a worker, and the device is parked while it does. The
	// position is meaningless during that window, so no blend is started and no
	// prefetch is built against it.
	if s.seekBusy || s.seekPending != nil {
		return
	}
	// Nothing to blend into, or the queue is at its end.
	if s.index+1 >= len(s.queue) {
		s.cancelPrefetch()

		return
	}

	remaining, ok := s.crossfadeRemaining(live)
	if !ok {
		// The duration is not known yet, so there is no moment to aim at.
		return
	}

	rampFrames := s.crossfadeRampFrames(cfg)
	prefetchFrames := rampFrames + int64(prefetchMarginMs)*int64(canonical.Rate)/1000

	if s.prefetch == nil && remaining <= prefetchFrames && s.prefetchFail != s.index+1 {
		s.startPrefetch()
	}

	// The prefetch must still be the track that follows the current one. A
	// queue edit or a shuffle can change what "next" means without moving the
	// index, so the identity is re-checked here and a stale prefetch dropped.
	if pf := s.prefetch; pf != nil && pf.index != s.index+1 {
		s.cancelPrefetch()
	}

	// Only a playing track blends. A paused one keeps its prefetch warm so the
	// blend is ready the moment playback resumes.
	if s.state() != StatePlaying {
		return
	}
	if s.prefetch == nil || !s.prefetch.ready {
		return
	}
	if remaining > rampFrames {
		return
	}
	// The incoming track must already have audio buffered before the blend
	// starts, or the ramp fades in silence for its first buffers.
	if s.prefetch.result.streamer.Stats().Buffered < int64(crossfadePrebufferMs)*int64(canonical.Rate)/1000 {
		return
	}

	// A same-album handoff is left unblended: the album was sequenced to run
	// together. The prefetch is kept, and the ordinary end-of-track path
	// promotes it with no gap.
	if cfg.SkipSameAlbum && s.sameAlbumHandoff() {
		return
	}

	s.beginCrossfade(s.prefetch, rampFrames)
}

// crossfadeRemaining reports how much of the current track is left, in output
// frames: the heard position subtracted from the probed duration, so it matches
// what the listener still has rather than how far the decoder has read ahead.
// It reports false when the duration is not known.
func (s *Session) crossfadeRemaining(live *stream.Streamer) (int64, bool) {
	s.mu.Lock()
	duration := s.v.duration
	s.mu.Unlock()
	if duration <= 0 {
		return 0, false
	}

	heard := stream.FramesToDuration(live.Position())
	if latency := s.chain.Latency(); latency > 0 {
		if heard > latency {
			heard -= latency
		} else {
			heard = 0
		}
	}

	remaining := duration - heard
	if remaining < 0 {
		remaining = 0
	}

	return int64(remaining) * int64(canonical.Rate) / int64(1e9), true
}

// crossfadeRampFrames converts the configured duration to output frames.
func (s *Session) crossfadeRampFrames(cfg CrossfadeSettings) int64 {
	return int64(cfg.DurationMs) * int64(canonical.Rate) / 1000
}

// sameAlbumHandoff reports whether the next queued track is from the same album
// as the current one. An empty album on either side is treated as different: an
// unknown album is not evidence that two tracks belong together.
func (s *Session) sameAlbumHandoff() bool {
	if s.prefetch == nil || s.prefetch.album == "" {
		return false
	}

	s.mu.Lock()
	current := s.v.meta.Tags.Album
	s.mu.Unlock()

	return current != "" && current == s.prefetch.album
}

// startPrefetch builds the next queued track early, off the control loop. The
// built streamer is not installed: the current track keeps sounding and the
// incoming one fills its own ring, ready for the ramp. Its album is resolved
// too, so the same-album decision does not touch the control loop.
func (s *Session) startPrefetch() {
	idx := s.index + 1
	if idx < 0 || idx >= len(s.queue) {
		return
	}
	path := s.queue[idx]

	s.prefetchGen++
	pf := &prefetchState{gen: s.prefetchGen, seq: s.seq, index: idx, path: path}
	s.prefetch = pf

	s.workerWG.Add(1)
	go func() {
		defer s.workerWG.Done()

		pf.result = s.build(s.seq, idx, path)
		if pf.result.err == nil {
			pf.album = s.prefetchAlbum(pf.result, path)
		}

		select {
		case s.prefetchCh <- pf:
		case <-s.closing:
			if pf.result.streamer != nil {
				_ = pf.result.streamer.Close()
			}
		}
	}()
}

// prefetchAlbum resolves the album of the prefetched track, so the same-album
// skip can be decided before the ramp. A provider track carries its album from
// the build; a local file is read through the resolver, which is why this runs
// on the prefetch worker and not the control loop.
func (s *Session) prefetchAlbum(res openResult, path string) string {
	if res.providerMeta {
		return res.meta.Tags.Album
	}
	m, err := s.cfg.Resolver.Resolve(s.ctx, path)
	if err != nil || m == nil {
		return ""
	}

	return m.Tags.Album
}

// handlePrefetch installs a finished prefetch build. A result that no longer
// belongs to the current prefetch (the queue moved on, or the prefetch was
// cancelled) is closed rather than installed. A ready build is kept until the
// ramp moment or the end of the current track.
func (s *Session) handlePrefetch(pf *prefetchState) {
	if s.prefetch != pf || pf.gen != s.prefetchGen {
		if pf.result.streamer != nil {
			_ = pf.result.streamer.Close()
		}

		return
	}
	if pf.result.err != nil {
		// A track that will not open is not a crossfade failure: the ordinary
		// end-of-track path will try it again and report the error then. The
		// index is remembered so the tick does not rebuild it every poll.
		s.prefetch = nil
		s.prefetchFail = pf.index

		return
	}

	pf.ready = true
	// The build may land after the ramp moment has already passed (a very short
	// track, or a slow source). Re-run the decision so the blend starts now if
	// there is still room, or waits for the end if there is not.
	s.pollCrossfade()
}

// beginCrossfade publishes the ramp from the current track into the prefetched
// one. The provider mixes the two until the ramp completes, at which point it
// notifies the control loop, which promotes the prefetched track.
func (s *Session) beginCrossfade(pf *prefetchState, rampFrames int64) {
	if pf == nil || !pf.ready || s.live == nil || rampFrames <= 0 {
		return
	}

	if !s.provider.beginCrossfade(s.live, pf.result.streamer, rampFrames) {
		return
	}
	s.xfading = true
	s.xfadeStarts.Add(1)
}

// promoteReadyPrefetch installs the prefetched track as the live one, if one is
// ready. It is the single promotion point for both the completed ramp and the
// unblended same-album handoff. It reports whether a promotion happened, so the
// caller can skip the ordinary end-of-track path when it did.
func (s *Session) promoteReadyPrefetch() bool {
	pf := s.prefetch
	if pf == nil || !pf.ready || pf.result.streamer == nil {
		return false
	}

	from := s.live
	s.xfading = false
	s.prefetch = nil
	s.prefetchFail = 0
	s.index = pf.index

	// The outgoing track's source-upgrade download, if any, belongs to the
	// track being retired; cancel it before activate adopts the incoming
	// track's own upgrade slot, so the outgoing download does not run on.
	if s.upgradeCancel != nil {
		s.upgradeCancel()
		s.upgradeCancel = nil
	}

	// activate points the device at the new streamer (which also clears the
	// provider's ramp), publishes the chain the prefetch built, updates the
	// view, emits TrackChanged and starts the metadata/duration lookups.
	s.activate(pf.result)

	// The outgoing streamer is no longer read by the device, so it is closed
	// here rather than through retireLive: the view already describes the new
	// track, and retireLive would overwrite that with the outgoing position.
	if from != nil && from != s.live {
		_ = from.Close()
	}

	return true
}

// cancelPrefetch drops a prefetch in flight or waiting. A ready streamer is
// closed; an in-flight build is invalidated so its result is closed when it
// lands. It is called whenever the queue moves for a reason other than the
// crossfade itself: a manual Next, a seek, a stop.
//
// It refuses to close the streamer while a ramp is in flight, because the
// provider is reading it: a caller that means "abort the blend too" must use
// cancelCrossfade, which tears the ramp down first.
func (s *Session) cancelPrefetch() {
	pf := s.prefetch
	if pf == nil {
		return
	}
	if !s.xfading && pf.ready && pf.result.streamer != nil {
		_ = pf.result.streamer.Close()
	}
	s.prefetch = nil
	// Invalidate any result still being built, so handlePrefetch closes it
	// instead of installing a track the queue has moved past.
	s.prefetchGen++
}

// cancelCrossfade aborts a ramp in flight and drops any prefetch. It is the one
// call a manual transition makes, so a blend the user interrupted cannot keep
// mixing behind the new track.
func (s *Session) cancelCrossfade() {
	s.xfading = false
	to := s.provider.abortCrossfade()
	pf := s.prefetch
	s.cancelPrefetch()
	// The incoming streamer of the ramp is usually the prefetch's, which
	// cancelPrefetch just closed. It is closed here only in the unexpected case
	// where the ramp and the prefetch diverged, so it cannot leak.
	if to != nil && to != s.live && (pf == nil || pf.result.streamer != to) {
		_ = to.Close()
	}
}
