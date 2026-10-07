// Package main is the Wails v3 desktop front end for the molo engine. The Go
// side is a thin adapter: it owns one molo.Player, turns its sealed event
// stream into Wails events, and exposes the facade's cheap commands as bound
// methods. No audio logic lives here; only the UI this package decides.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/meta"
	"github.com/dlcuy22/molo/provider"
	"github.com/dlcuy22/molo/ui/webui/internal/cover"
	"github.com/dlcuy22/molo/ui/webui/internal/spectrum"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// Event names the frontend subscribes to. They are constants because a typo in
// a string on either side of the bridge is a silent no-op that the generated
// binding cannot catch.
const (
	// eventSnapshot carries PlayerService.Snapshot on every accepted change and
	// on the display tick. It is the whole visible state in one payload,
	// including the last error, so the UI has one subscription to reason about
	// instead of one per field.
	eventSnapshot = "molo:snapshot"
	// eventFrame carries one spectrum frame from the visualizer goroutine.
	eventFrame = "molo:spectrum"
	// eventEffectMeters carries the lean effect meters on their own faster tick,
	// so a gain-reduction needle moves without the full chain snapshot.
	eventEffectMeters = "molo:effect-meters"
)

// spectrumInterval is the visualizer analysis period: ~60 Hz, matching
// EasyEffects' default spectrumFpsCap. The runner slides its 8192-sample
// window by the wall-clock time since the previous frame, so each analysis
// advances ~800 new samples with heavy window overlap: consecutive spectra
// correlate and the motion reads as continuous with no follower lagging it,
// regardless of how coarsely the tap below delivers the audio.
const spectrumInterval = 16 * time.Millisecond

// positionInterval is the snapshot refresh period. The engine reports discrete
// changes as events; the position is continuous, so a poll is what keeps the
// progress bar moving. 4 Hz matches the TUI: smooth for a whole-second clock
// and negligible next to the decoder.
const positionInterval = 250 * time.Millisecond

// effectMeterInterval is the fast effect-meter tick. The snapshot's 4 Hz is
// fine for a static curve but visibly stepped for a moving gain-reduction
// needle or a scrolling dynamics graph, so the meters ride their own tick
// between snapshots. 60 Hz matches the display so a graph's newest sample lands
// once per frame; the payload is a few floats per metered stage and no schema
// or values, so it stays cheaper than the spectrum's own 60 Hz transform.
const effectMeterInterval = 16 * time.Millisecond

// PlayerService owns the engine and the bridge to the frontend.
//
// It is a Wails service, hence a singleton shared by every window, so all
// mutable state is guarded. The engine itself is safe for concurrent use; what
// needs protecting here is the UI's assembled view of it.
type PlayerService struct {
	player molo.Player

	mu        sync.Mutex
	lastError string
	// indexedQueue is the queue the tag index was last reconciled against, so
	// the 4 Hz snapshot does not re-walk an unchanged queue.
	indexedQueue []string

	events  <-chan molo.Event
	tap     molo.Tap
	closing chan struct{}
	wg      sync.WaitGroup

	// index resolves and caches the tags of every queued track, so the palette
	// can search a queue the engine has not described. ctx is cancelled on
	// shutdown to stop any reader still opening a file.
	index  *tagIndex
	ctx    context.Context
	cancel context.CancelFunc

	// ytm is the YouTube Music side: the catalogue metadata of every reference
	// the session has seen, and the provider that resolves a reference into
	// audio. The engine never sees either; the UI owns the catalogue.
	ytm     *ytmIndex
	ytmProv *ytmProvider

	// search caches the last search per query, so arrowing back through a
	// half-typed query does not spend a request. It is bounded and cleared on
	// shutdown with the rest.
	searchMu    sync.Mutex
	searchCache map[string][]YTMResult

	// preview is the palette's short-window audition, played on a second
	// player so the main track can stay loaded and paused underneath. It is
	// built on startup, once the engine exists.
	preview *previewer

	// coverMu guards the decoded-artwork cache. Art arrives with the resolved
	// tags, so it is decoded on the first request and kept under the id the
	// snapshot advertises; a track change reuses the entry if the art repeats.
	coverMu    sync.Mutex
	coverCache map[string]string
	coverOrder []string

	// spectrumMu guards the runner, which the tick goroutine reads and
	// ConfigureSpectrum replaces. It also guards the last emitted frame, so
	// the pump can suppress repeats without a second lock.
	spectrumMu  sync.Mutex
	spectrumCfg spectrum.Config
	runner      *spectrum.Runner
	// spectrumEnabled gates the pump. Off, the tick still fires but the
	// transform is skipped and nothing is emitted, so a hidden visualizer
	// costs no CPU. The runner is kept, not rebuilt, so switching back on
	// resumes from the live tap on the next tick.
	spectrumEnabled bool

	// spectrumLast is the last emitted frame and spectrumLastSilent whether it
	// read as silence, so the pump can skip a frame that carries nothing new.
	spectrumLast       []float64
	spectrumLastSilent bool

	// presence is the Discord Rich Presence integration. It is inert until the
	// user turns it on, so the snapshot pump always calls into it and the cost
	// when off is one comparison.
	presence presenceState

	log *slog.Logger
}

func newPlayerService() *PlayerService {
	ctx, cancel := context.WithCancel(context.Background())

	ytmIndex := newYTMIndex()

	return &PlayerService{
		closing:         make(chan struct{}),
		spectrumCfg:     spectrum.DefaultConfig(),
		spectrumEnabled: true,
		coverCache:      make(map[string]string),
		index:           newTagIndex(),
		ytm:             ytmIndex,
		ytmProv:         newYTMProvider(ytmIndex, ytmSeamlessSwap, nil),
		searchCache:     make(map[string][]YTMResult),
		ctx:             ctx,
		cancel:          cancel,
		log:             slog.Default(),
	}
}

// ServiceStartup builds the engine and starts the two background pumps. It is
// the v3 lifecycle hook; there is no OnStartup option in this framework.
func (s *PlayerService) ServiceStartup(_ context.Context, _ application.ServiceOptions) error {
	// Register the Lua effects before the player is built, so the chooser and
	// the effect window see them from the first snapshot. A script failure is
	// logged and skipped, never fatal: a bad user script must not stop playback.
	loadScripts()

	p, err := molo.New(
		molo.WithProviders(s.ytmProv, provider.LocalAudio{}),
		// The same switch the provider was built with: the engine consumes the
		// Upgrade only when this is set, and the provider offers it only when
		// it is, so the two cannot drift.
		molo.WithExperimental(molo.Experimental{SourceUpgrade: ytmSeamlessSwap}),
	)
	if err != nil {
		return fmt.Errorf("build player: %w", err)
	}
	s.player = p
	s.tap = p.Tap()
	s.events = p.Events()
	if err := s.rebuildRunner(); err != nil {
		return err
	}

	// The preview player is a second instance over the same process-wide oto
	// context, which oto mixes. It is built here so a failure to construct it
	// surfaces at startup rather than on the first Ctrl+Alt+P.
	pp, err := molo.New()
	if err != nil {
		return fmt.Errorf("build preview player: %w", err)
	}
	s.preview = newPreviewer(s, pp)
	s.preview.start()

	// The engine's event channel is drained on its own goroutine and fanned out
	// to Wails events. Draining continuously matters: a UI that only listened
	// when it felt like it would let the engine's buffer fill and drop.
	s.wg.Add(3)
	go s.pumpEvents()
	go s.pumpSpectrum()
	go s.pumpEffectMeters()

	// Restore Discord presence if the user had it on, now that the engine and
	// the catalogue are up and a track can be described.
	s.initDiscord()

	return nil
}

// ServiceShutdown stops the pumps and closes the engine before the framework
// tears the window down.
func (s *PlayerService) ServiceShutdown() error {
	select {
	case <-s.closing:
	default:
		close(s.closing)
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.stopDiscord()
	s.wg.Wait()
	if s.preview != nil {
		s.preview.close()
	}

	if s.player != nil {
		return s.player.Close()
	}

	return nil
}

// pumpEvents drains the engine's event stream and also ticks a position
// refresh. The engine emits an event only when something discrete changes; the
// position moves continuously, so without the tick the progress bar would sit
// still until the next track. This mirrors the TUI's poll: the same 4 Hz that
// is smooth for a whole-second clock and cheap next to the decoder.
func (s *PlayerService) pumpEvents() {
	defer s.wg.Done()

	ticker := time.NewTicker(positionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.closing:
			return
		case ev, ok := <-s.events:
			if !ok {
				return
			}
			if failed, isFail := ev.(molo.Failed); isFail {
				// The snapshot carries the error, so the failure needs no event
				// of its own; recording it here keeps it until a command clears
				// it.
				s.setError(failed.Err.Error())
			}
			snap := s.Snapshot()
			s.emit(eventSnapshot, snap)
			s.syncPresence(snap)
		case <-ticker.C:
			// A tick with no track is a wasted payload, but it is what lets the
			// UI show a first "Ready" and recover if a push was ever missed.
			snap := s.Snapshot()
			s.emit(eventSnapshot, snap)
			s.syncPresence(snap)
		}
	}
}

// pumpEffectMeters publishes the lean effect meters on their own faster tick.
// It is separate from pumpEvents because the 4 Hz snapshot would make a moving
// needle step, and separate from pumpSpectrum because the payload is different:
// a few floats per metered stage, not 150 bands.
func (s *PlayerService) pumpEffectMeters() {
	defer s.wg.Done()

	ticker := time.NewTicker(effectMeterInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.closing:
			return
		case <-ticker.C:
			s.publishEffectMeters()
		}
	}
}

// publishEffectMeters emits one lean meter payload. A chain with no metered
// stage, or an engine that is not built yet, emits nothing rather than an empty
// event, so an idle player does not push a payload at 60 Hz forever.
func (s *PlayerService) publishEffectMeters() {
	fx, err := s.effects()
	if err != nil {
		return
	}
	meters := fx.EffectMeters()
	if len(meters) == 0 {
		return
	}
	out := make([]EffectMetersInfo, 0, len(meters))
	for _, m := range meters {
		out = append(out, EffectMetersInfo{ID: m.ID, Meters: m.Meters})
	}
	s.emit(eventEffectMeters, out)
}

// pumpSpectrum reads the tap on its own clock and publishes a frame. Running
// the transform here rather than in the frontend keeps a slow or backgrounded
// webview from holding anything valuable.
func (s *PlayerService) pumpSpectrum() {
	defer s.wg.Done()

	ticker := time.NewTicker(spectrumInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.closing:
			return
		case <-ticker.C:
			s.publishSpectrum()
		}
	}
}

// publishSpectrum computes one frame and emits it as a plain slice of levels.
// A disabled visualizer skips the transform entirely: the tick still fires, but
// with no listener the display would only burn CPU on a frame nobody draws.
// A frame that carries nothing new is skipped: an identical repeat (the
// runner holds its display while the tap has no new samples, e.g. paused) or
// continued near-silence after the drain already reached the floor. Without
// this the bridge would push 150 floats at 60 Hz forever, even for a frozen
// or empty display the frontend would just redraw identically.
func (s *PlayerService) publishSpectrum() {
	s.spectrumMu.Lock()
	runner := s.runner
	enabled := s.spectrumEnabled
	s.spectrumMu.Unlock()
	if runner == nil || !enabled {
		return
	}

	bands := runner.Frame()
	if s.suppressSpectrum(bands) {
		return
	}
	// Copy: the runner reuses its slice, and the event outlives this call.
	out := make([]float64, len(bands))
	copy(out, bands)
	s.emit(eventFrame, out)
}

// suppressSpectrum reports whether bands is not worth emitting, recording it
// as the last emitted frame when it is. Silence is measured with a small
// epsilon rather than exact zero because the release decay approaches the
// floor asymptotically and would otherwise trickle forever.
func (s *PlayerService) suppressSpectrum(bands []float64) bool {
	const silentLevel = 0.002

	peak := 0.0
	for _, v := range bands {
		if a := abs(v); a > peak {
			peak = a
		}
	}
	silent := peak < silentLevel

	s.spectrumMu.Lock()
	defer s.spectrumMu.Unlock()

	if silent && s.spectrumLastSilent {
		return true
	}
	if !silent && len(bands) == len(s.spectrumLast) && equalFloats(bands, s.spectrumLast) {
		return true
	}
	s.spectrumLast = append(s.spectrumLast[:0], bands...)
	s.spectrumLastSilent = silent

	return false
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}

	return v
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}

	return true
}

// rebuildRunner installs a runner for the current spectrum config. The caller
// holds no lock; the mutex is taken here.
func (s *PlayerService) rebuildRunner() error {
	runner, err := spectrum.New(s.tap, s.spectrumCfg)
	if err != nil {
		return fmt.Errorf("build visualizer: %w", err)
	}
	s.spectrumMu.Lock()
	s.runner = runner
	// A new shape makes the last emitted frame incomparable, so drop it: the
	// next frame must reach the display even if it reads as silence.
	s.spectrumLast = s.spectrumLast[:0]
	s.spectrumLastSilent = false
	s.spectrumMu.Unlock()

	return nil
}

// Snapshot is the bound read the UI calls once on mount and then treats as
// push-updated. It is cheap by the facade's contract, so a stray poll is fine.
func (s *PlayerService) Snapshot() Snapshot {
	snap := s.player.Snapshot()
	queue := s.player.Queue()
	s.syncIndex(queue)

	s.mu.Lock()
	lastErr := s.lastError
	s.mu.Unlock()

	var preview PreviewState
	if s.preview != nil {
		preview = s.preview.state()
	}

	return Snapshot{
		State:       stateName(snap.State),
		Path:        snap.Path,
		Title:       snap.Meta.Tags.Title,
		Artist:      snap.Meta.Tags.Artist,
		Album:       snap.Meta.Tags.Album,
		ArtistID:    s.currentArtistID(snap.Path),
		CoverID:     cover.ID(snap.Meta.Tags.Cover),
		CoverMime:   snap.Meta.Tags.CoverMIME,
		Codec:       snap.Meta.Codec,
		Container:   snap.Meta.Container,
		Position:    snap.Position.Milliseconds(),
		Duration:    snap.Duration.Milliseconds(),
		Volume:      snap.Volume,
		Queue:       s.queueRows(queue, snap.QueueIndex),
		QueueIdx:    snap.QueueIndex,
		Shuffled:    s.player.Shuffled(),
		Decoder:     snap.Decoder,
		DecoderPref: s.player.Settings().Decoder,
		Backend:     snap.Backend,
		SampleRate:  snap.Format.Rate,
		Channels:    snap.Format.Ch,
		Error:       lastErr,
		Preview:     preview,
	}
}

// currentArtistID returns the first credited artist's browse id for a remote
// track, or "" for a local file. It is carried in the snapshot so the settings
// preview can fetch the avatar without a second search; the presence path reads
// the same id from the catalogue index.
func (s *PlayerService) currentArtistID(path string) string {
	if !isYTMRef(path) {
		return ""
	}
	t, ok := s.ytm.lookup(path)
	if !ok {
		return ""
	}

	return t.ArtistID
}

// syncIndex reconciles the tag index with the queue, but only when the queue
// actually changed. Snapshot is polled at 4 Hz, and re-walking an unchanged
// queue on every tick would be pure waste.
func (s *PlayerService) syncIndex(queue []string) {
	s.mu.Lock()
	same := equalPaths(s.indexedQueue, queue)
	if !same {
		s.indexedQueue = append([]string(nil), queue...)
	}
	s.mu.Unlock()
	if same {
		return
	}

	s.indexQueue(queue)
	s.syncYTM(queue)
}

// syncYTM reconciles the YouTube Music catalogue index with the queue: it
// protects the references the queue holds and bounds the rest. It runs whenever
// the queue changes, alongside the tag index.
func (s *PlayerService) syncYTM(queue []string) {
	current := make(map[string]struct{}, len(queue))
	for _, ref := range queue {
		if isYTMRef(ref) {
			current[ref] = struct{}{}
		}
	}
	s.ytm.prune(current)
}

// equalPaths reports whether two queues hold the same paths in the same order.
// It is the queue's identity for the index: a reorder is a new queue because
// the rows the palette jumps to are positional.
func equalPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// Cover returns the current track's artwork as an inline data URL, or an empty
// string when the track has none. The snapshot carries only the id, so the
// payload a UI redraws at 4 Hz stays small; the bytes cross the bridge once,
// when the id changes. The cache is keyed by that id, so revisiting a track
// does not re-decode its art.
func (s *PlayerService) Cover(id string) string {
	if id == "" {
		return ""
	}

	s.coverMu.Lock()
	defer s.coverMu.Unlock()

	if url, ok := s.coverCache[id]; ok {
		return url
	}

	data := s.player.Snapshot().Meta.Tags.Cover
	if cover.ID(data) != id {
		// The track moved on between the request and this read; answer with
		// nothing rather than the wrong art.
		return ""
	}

	url, err := cover.DataURL(data)
	if err != nil {
		s.log.Warn("cover: skipping undecodable artwork", "error", err)

		return ""
	}
	s.storeCover(id, url)

	return url
}

// coverCacheMax bounds the decoded-artwork cache. A long listening session can
// touch hundreds of tracks, and each entry is a few tens of kilobytes of base64
// held for the life of the process, so the oldest entries are evicted once the
// cap is reached.
const coverCacheMax = 64

// searchCacheMax bounds the remembered searches. The palette fires one request
// per settled query, so a session accumulates them; past the cap the cache is
// dropped whole, which is cheap and cannot grow unbounded.
const searchCacheMax = 32

// QueueCover returns the artwork of one queued track as an inline data URL, or
// an empty string when it has none. The palette asks only for the rows it is
// about to draw, so a large queue never pays to decode art the user will not
// see. The cache is keyed by the content id the row carries, so a repeat ask
// and a track that reappears are both served without re-reading the file.
func (s *PlayerService) QueueCover(path string) string {
	// A remote reference has no file to read; its art comes from the
	// catalogue, already fetched when the row was added or the track opened.
	if isYTMRef(path) {
		return s.ytmCover(path)
	}

	tags := s.index.lookup(path)
	if tags.CoverID == "" {
		return ""
	}

	s.coverMu.Lock()
	if url, ok := s.coverCache[tags.CoverID]; ok {
		s.coverMu.Unlock()

		return url
	}
	s.coverMu.Unlock()

	// Read and encode off the lock: opening the file is the slow part, and
	// holding coverMu across it would stall the current track's Cover call.
	m, err := meta.Resolve(s.ctx, path)
	if err != nil || m == nil {
		return ""
	}
	data := m.Tags.Cover
	if cover.ID(data) != tags.CoverID {
		// The file changed under us; the row's id is stale, so answer nothing
		// rather than art that no longer matches.
		return ""
	}

	url, err := cover.DataURL(data)
	if err != nil {
		s.log.Warn("cover: skipping undecodable artwork", "path", path, "error", err)

		return ""
	}

	s.coverMu.Lock()
	s.storeCover(tags.CoverID, url)
	s.coverMu.Unlock()

	return url
}

// storeCover records one decoded data URL, evicting the oldest entry when the
// cache is full. The caller holds coverMu.
func (s *PlayerService) storeCover(id, url string) {
	if _, ok := s.coverCache[id]; !ok {
		if len(s.coverOrder) >= coverCacheMax {
			oldest := s.coverOrder[0]
			s.coverOrder = s.coverOrder[1:]
			delete(s.coverCache, oldest)
		}
		s.coverOrder = append(s.coverOrder, id)
	}
	s.coverCache[id] = url
}

// ytmCover returns a remote track's artwork as an inline data URL, or "" when
// it has none. It is a plain read for the frontend: a failure means "no art",
// which is not worth the error line.
func (s *PlayerService) ytmCover(ref string) string {
	t, ok := s.ytm.lookup(ref)
	if !ok {
		return ""
	}
	id := t.coverID()
	if id == "" {
		return ""
	}

	s.coverMu.Lock()
	if url, ok := s.coverCache[id]; ok {
		s.coverMu.Unlock()

		return url
	}
	s.coverMu.Unlock()

	// The bytes may not be here yet: a row can be drawn from a search result
	// before its art is fetched. Fetch off the lock, because it is a network
	// round trip, then fall back to the URL hash as the cache key so a repeat
	// ask still hits.
	data := s.ytmProv.fetchThumb(ref)
	if len(data) == 0 {
		return ""
	}
	url, err := cover.DataURL(data)
	if err != nil {
		s.log.Warn("cover: skipping undecodable artwork", "ref", ref, "error", err)

		return ""
	}

	id = cover.ID(data)
	s.coverMu.Lock()
	s.storeCover(id, url)
	s.coverMu.Unlock()

	return url
}

// ArtistAvatar returns a YouTube Music artist's avatar as an inline data URL,
// or "" when there is none. It is the settings preview's source: the presence
// itself only needs the URL, but the webview may not load the remote image, so
// the preview asks for the bytes the backend already fetched. The lookup is
// cached in memory and on disk, so repeated asks cost nothing.
func (s *PlayerService) ArtistAvatar(artistID string) string {
	artistID = strings.TrimSpace(artistID)
	if artistID == "" {
		return ""
	}

	return s.ytmProv.artistAvatarDataURL(s.ctx, artistID)
}

// SearchYTMSongs searches YouTube Music and returns the results a listener can
// act on: songs first, then the other kinds, each labelled. The engine is not
// involved; this is catalogue work, and it is the one call here that reaches
// the network.
func (s *PlayerService) SearchYTMSongs(query string) ([]YTMResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	s.searchMu.Lock()
	if hit, ok := s.searchCache[query]; ok {
		s.searchMu.Unlock()

		return hit, nil
	}
	s.searchMu.Unlock()

	res, err := s.ytmProv.search(s.ctx, query, "", false)
	if err != nil {
		return nil, err
	}
	results := ytmResults(res)

	// Cache the rows and their catalogue metadata, so a selected result has a
	// title and art to show before its stream is resolved.
	for _, r := range results {
		if !r.Playable {
			continue
		}
		s.ytm.put(ytmTrack{
			ID:           r.VideoID,
			Title:        r.Title,
			Artist:       r.Artist,
			Album:        r.Album,
			DurationMs:   r.DurationMs,
			ThumbnailURL: r.Thumbnail,
			ArtistID:     r.ArtistID,
		})
	}

	s.searchMu.Lock()
	if len(s.searchCache) >= searchCacheMax {
		s.searchCache = make(map[string][]YTMResult)
	}
	s.searchCache[query] = results
	s.searchMu.Unlock()

	return results, nil
}

// InsertNextYTM queues a YouTube Music track to play after the current one,
// which is what Enter on a search result means. With a track already sounding
// the queue is spliced after it, so that track keeps playing; with nothing
// sounding there is no "next", so the insert also starts the track.
func (s *PlayerService) InsertNextYTM(videoID string) error {
	ref := ytmRef(videoID)
	if ref == ytmRefPrefix {
		return fmt.Errorf("ytm: no track id")
	}

	return s.command(func() error {
		snap := s.player.Snapshot()
		if snap.QueueIndex >= 0 && (snap.State == molo.Playing || snap.State == molo.Paused) {
			return s.player.InsertQueue(snap.QueueIndex+1, []string{ref})
		}

		return s.player.InsertQueueAndPlay(0, []string{ref})
	})
}

// AppendYTM adds a YouTube Music track to the end of the queue without starting
// it, which is what Shift+Enter on a search result means. A live track is left
// alone; an empty queue simply gains its first entry.
func (s *PlayerService) AppendYTM(videoID string) error {
	ref := ytmRef(videoID)
	if ref == ytmRefPrefix {
		return fmt.Errorf("ytm: no track id")
	}

	return s.command(func() error {
		return s.player.InsertQueue(len(s.player.Queue()), []string{ref})
	})
}

// Options is the static chooser data the UI reads once, because neither list
// changes at runtime.
func (s *PlayerService) Options() Options {
	return Options{
		Codecs:   codecOptions(),
		Backends: backendOptions(),
		Effects:  s.player.EffectKinds(),
	}
}

// SpectrumSchema describes the visualizer controls; SpectrumConfig reports the
// values in force. The transform is display-only, so these never touch the
// engine.
func (s *PlayerService) SpectrumSchema() []spectrum.Param { return spectrum.Schema() }

func (s *PlayerService) SpectrumConfig() SpectrumConfig {
	s.spectrumMu.Lock()
	cfg := s.spectrumCfg
	enabled := s.spectrumEnabled
	s.spectrumMu.Unlock()

	return SpectrumConfig{Bars: cfg.Bars, MinHz: int(cfg.MinHz), MaxHz: int(cfg.MaxHz), FFT: cfg.FFT, Enabled: enabled}
}

// ConfigureSpectrum replaces the visualizer shape. The frame is rebuilt, so the
// range, the points and the transform size all take effect on the next tick.
// Enabled is not part of the shape: it only gates the pump, so it can be
// toggled without rebuilding the runner and losing the live window.
func (s *PlayerService) ConfigureSpectrum(cfg SpectrumConfig) error {
	next := spectrum.DefaultConfig()
	if cfg.Bars > 0 {
		next.Bars = cfg.Bars
	}
	if cfg.MinHz > 0 {
		next.MinHz = float64(cfg.MinHz)
	}
	if cfg.MaxHz > 0 {
		next.MaxHz = float64(cfg.MaxHz)
	}
	if cfg.FFT > 0 {
		next.FFT = cfg.FFT
	}
	if next.MinHz >= next.MaxHz {
		return fmt.Errorf("visualizer: low cut must be below high cut")
	}

	s.spectrumMu.Lock()
	wasEnabled := s.spectrumEnabled
	shapeChanged := s.spectrumCfg != next
	haveRunner := s.runner != nil
	s.spectrumCfg = next
	s.spectrumEnabled = cfg.Enabled
	// Turning the pump back on must reach the display even if the first frame
	// reads as silence, so drop the suppression state the way a rebuild does.
	if cfg.Enabled && !wasEnabled {
		s.spectrumLast = s.spectrumLast[:0]
		s.spectrumLastSilent = false
	}
	s.spectrumMu.Unlock()

	// Only a shape change rebuilds the transform. A pure enable toggle keeps
	// the runner and its live window, so switching back on resumes with no
	// cold start; a rebuild here would discard the backlog and lag the display.
	if shapeChanged || !haveRunner {
		if err := s.rebuildRunner(); err != nil {
			return err
		}
	}
	s.emit(eventSnapshot, s.Snapshot())

	return nil
}

// PlayIndex starts queue row index.
func (s *PlayerService) PlayIndex(index int) error {
	return s.command(func() error { return s.player.PlayIndex(index) })
}

// PreviewStart begins previewing the queued track at path, or replaces the
// current preview. It pauses the main track once for the session and resumes it
// when the session ends, so arrow navigation never flaps the main track.
//
// A remote reference is refused: the preview player is built without providers,
// so it could not open one, and the failure would leave the main track paused
// for a session that never produced a sound.
func (s *PlayerService) PreviewStart(path string) error {
	if s.preview == nil {
		return fmt.Errorf("preview is not available")
	}
	if path == "" {
		return fmt.Errorf("preview needs a path")
	}
	if isYTMRef(path) {
		return fmt.Errorf("preview is for local tracks")
	}
	s.preview.play(path)

	return nil
}

// PreviewStop ends the preview and resumes the main track if the session had
// paused it. It is safe to call when no preview is running.
func (s *PlayerService) PreviewStop() error {
	if s.preview == nil {
		return nil
	}
	s.preview.halt()

	return nil
}

// PreviewConfig reads the preview window settings in force.
func (s *PlayerService) PreviewConfig() PreviewConfig {
	if s.preview == nil {
		return normalizePreviewConfig(PreviewConfig{})
	}

	return s.preview.config()
}

// SetPreviewConfig replaces the preview window settings. The values are
// normalized, so a UI can send what it has without clamping first.
func (s *PlayerService) SetPreviewConfig(cfg PreviewConfig) error {
	if s.preview == nil {
		return fmt.Errorf("preview is not available")
	}
	s.preview.configure(cfg)

	return nil
}

// LoadPaths adds the chosen files and folders to the queue. When nothing is
// queued yet it replaces the (empty) queue and starts the first track; when a
// queue already exists it appends, so adding a folder does not cut off or reset
// the track that is playing. A folder is expanded to its playable files,
// recursively and sorted, the same way the TUI reads a folder argument.
func (s *PlayerService) LoadPaths(paths []string) error {
	return s.command(func() error {
		queue, err := s.expand(paths)
		if err != nil {
			return err
		}
		if len(queue) == 0 {
			return fmt.Errorf("no playable files in the selection")
		}

		existing := s.player.Queue()
		if len(existing) == 0 {
			return s.player.PlayQueue(queue)
		}

		// A queue already exists, so this is an "add": append without
		// disturbing the current track. Starting the new refs here would jump
		// away from the track the user is on, which is the bug this guards.
		return s.player.InsertQueue(len(existing), queue)
	})
}

// OpenFiles shows the native picker and loads what it returns. The dialog is
// owned by Go so the frontend does not need a file-system permission of its
// own.
func (s *PlayerService) OpenFiles() error {
	app := application.Get()
	if app == nil {
		return fmt.Errorf("no application")
	}

	files, err := app.Dialog.OpenFile().
		SetTitle("Add audio files").
		AddFilter("Audio", "*.flac;*.opus;*.ogg;*.mp3;*.m4a;*.aac;*.wav").
		PromptForMultipleSelection()
	if err != nil {
		return s.command(func() error { return err })
	}
	if len(files) == 0 {
		return nil // cancelled
	}

	return s.LoadPaths(files)
}

// OpenFolder shows the native folder picker and loads what is playable inside.
func (s *PlayerService) OpenFolder() error {
	app := application.Get()
	if app == nil {
		return fmt.Errorf("no application")
	}

	dir, err := app.Dialog.OpenFile().
		SetTitle("Add a folder").
		CanChooseDirectories(true).
		CanChooseFiles(false).
		PromptForSingleSelection()
	if err != nil {
		return s.command(func() error { return err })
	}
	if dir == "" {
		return nil // cancelled
	}

	return s.LoadPaths([]string{dir})
}

// SetVolume sets the master gain in [0, 1].
func (s *PlayerService) SetVolume(v float64) error {
	return s.command(func() error {
		s.player.SetVolume(clamp01(v))

		return nil
	})
}

// TogglePause flips between playing and paused. With nothing playing it starts
// the current queue row, which is what a single play button means.
func (s *PlayerService) TogglePause() error {
	return s.command(func() error {
		snap := s.player.Snapshot()
		switch snap.State {
		case molo.Playing:
			return s.player.Pause()
		case molo.Paused:
			return s.player.Resume()
		default:
			if snap.QueueIndex >= 0 {
				return s.player.PlayIndex(snap.QueueIndex)
			}

			return nil
		}
	})
}

func (s *PlayerService) Next() error { return s.command(func() error { return s.player.Next() }) }

func (s *PlayerService) Prev() error { return s.command(func() error { return s.player.Prev() }) }

// SetShuffle turns baked shuffle on or off. On, the queue is reordered once and
// advances follow that order; off restores the order the shuffle was taken from.
func (s *PlayerService) SetShuffle(on bool) error {
	return s.command(func() error { return s.player.SetShuffle(on) })
}

// Stop ends playback but keeps the queue, so a later play reuses it.
func (s *PlayerService) Stop() error { return s.command(func() error { return s.player.Stop() }) }

// SeekTo repositions the current track. Milliseconds keep the bound signature
// plain; the facade takes a time.Duration. It is named SeekTo rather than Seek
// because Seek(int64) error is the io.Seeker shape by signature but not in
// meaning, and a method that almost satisfies an interface should not look like
// it does.
func (s *PlayerService) SeekTo(ms int64) error {
	if ms < 0 {
		ms = 0
	}

	return s.command(func() error { return s.player.Seek(time.Duration(ms) * time.Millisecond) })
}

// SetCodec changes the decoder preference. An empty name means automatic. Per
// the facade, this applies to the next track: SwapDecoder on a live track
// reopens it in place, which the UI confirms with the Swapped event.
func (s *PlayerService) SetCodec(name string) error {
	return s.command(func() error {
		if err := molo.ValidateDecoder(name); err != nil {
			return err
		}

		return s.player.SwapDecoder(name)
	})
}

// SetBackend changes the playback backend, reopening the device.
func (s *PlayerService) SetBackend(name string) error {
	return s.command(func() error {
		if err := molo.ValidateBackend(name); err != nil {
			return err
		}

		return s.player.SwapBackend(name)
	})
}

// QueueRow is one entry in the bound queue view. Title, Artist and Album are
// the tags the tag index resolved for the track; they are empty until the
// resolver lands, and the palette falls back to Name when they are. They are
// on the row rather than behind a second call so the search reads one payload.
// CoverID identifies the track's embedded art; the palette fetches the bytes
// through QueueCover only for the rows on screen.
type QueueRow struct {
	Index   int    `json:"index"`
	Path    string `json:"path"`
	Name    string `json:"name"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Album   string `json:"album"`
	CoverID string `json:"coverId"`
	Active  bool   `json:"active"`
}

// Snapshot is the whole visible player state in one value, so the frontend has
// a single subscription and a single source of truth.
type Snapshot struct {
	State  string `json:"state"`
	Path   string `json:"path"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	// ArtistID is the current remote track's first artist browse id, or "" for
	// a local file. The settings preview uses it to fetch the avatar that the
	// Discord presence shows as its small image.
	ArtistID string `json:"artistId"`
	// CoverID identifies the current artwork; the UI fetches the bytes through
	// Cover(id). It is empty when the track has no art, which is the signal to
	// fall back to the placeholder.
	CoverID string `json:"coverId"`
	// CoverMime is the artwork type the source declared. The shipped UI reads
	// the type from the data URL instead; this is here for a UI that wants the
	// source format, or that renders the bytes itself.
	CoverMime string     `json:"coverMime"`
	Codec     string     `json:"codec"`
	Container string     `json:"container"`
	Position  int64      `json:"position"` // milliseconds
	Duration  int64      `json:"duration"` // milliseconds
	Volume    float64    `json:"volume"`
	Queue     []QueueRow `json:"queue"`
	QueueIdx  int        `json:"queueIdx"`
	// Shuffled reports whether baked shuffle is in force, so the toggle can draw
	// its pressed state from the engine rather than from a local guess.
	Shuffled bool   `json:"shuffled"`
	Decoder  string `json:"decoder"`
	// DecoderPref is the saved decoder preference, which is what the chooser
	// selects. Decoder is the effective decoder for the current track, which is
	// more specific and would not match any option value.
	DecoderPref string `json:"decoderPref"`
	Backend     string `json:"backend"`
	SampleRate  int    `json:"sampleRate"`
	Channels    int    `json:"channels"`
	Error       string `json:"error"`
	// Preview is the palette's audition state, so the palette can draw the
	// previewing row and its progress from the same payload as everything else.
	Preview PreviewState `json:"preview"`
}

// Options is the static chooser data.
type Options struct {
	Codecs   []CodecOption `json:"codecs"`
	Backends []string      `json:"backends"`
	Effects  []string      `json:"effects"`
}

// CodecOption is one decoder choice. Auto is offered as its own row rather than
// hidden, because an empty preference is a real choice the engine understands.
type CodecOption struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// SpectrumConfig is the visualizer shape the UI edits. It is a full read and
// write payload: the frontend always echoes every field back, so a zero value
// is a real value (Enabled false disables the pump) rather than "unset".
type SpectrumConfig struct {
	Bars  int `json:"bars"`
	MinHz int `json:"minHz"`
	MaxHz int `json:"maxHz"`
	// FFT is the transform length. Zero means the shipped default, because no
	// transform can be zero wide.
	FFT int `json:"fft"`
	// Enabled gates the analysis pump. Off, the tick still fires but the
	// transform is skipped, so a hidden visualizer costs no CPU. The service
	// starts enabled.
	Enabled bool `json:"enabled"`
}

// command runs one engine call with the shared error policy: a failure is
// published as an event and returned, so the UI can show it whether the call
// came from a click or a keybinding. A success also publishes a snapshot, so
// a change the engine does not announce with its own event (volume is the
// case: SetVolume touches only the gain) still reaches the UI on this call
// rather than on the next position tick.
func (s *PlayerService) command(fn func() error) error {
	if err := fn(); err != nil {
		s.setError(err.Error())
		snap := s.Snapshot()
		s.emit(eventSnapshot, snap)
		s.syncPresence(snap)

		return err
	}
	s.clearError()
	snap := s.Snapshot()
	s.emit(eventSnapshot, snap)
	s.syncPresence(snap)

	return nil
}

// commandID is command for an editor call that returns a new stage ID. It
// reports the error the same way, then returns the ID on success.
func (s *PlayerService) commandID(fn func() (string, error)) (string, error) {
	id, err := fn()
	if err != nil {
		s.setError(err.Error())
		snap := s.Snapshot()
		s.emit(eventSnapshot, snap)
		s.syncPresence(snap)

		return "", err
	}
	s.clearError()
	snap := s.Snapshot()
	s.emit(eventSnapshot, snap)
	s.syncPresence(snap)

	return id, nil
}

// expand resolves files and folders to a queue of playable paths, deduplicated
// with folders expanded recursively. It mirrors the TUI's argument handling so
// both front ends accept the same input.
func (s *PlayerService) expand(paths []string) ([]string, error) {
	supported := make(map[string]bool)
	for _, ext := range molo.SupportedExtensions() {
		supported[strings.ToLower(ext)] = true
	}

	var out []string
	seen := make(map[string]bool)
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			add(p)

			continue
		}

		found, err := collectDir(p, supported)
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			add(f)
		}
	}

	return out, nil
}

func (s *PlayerService) setError(msg string) {
	s.mu.Lock()
	s.lastError = msg
	s.mu.Unlock()
}

func (s *PlayerService) clearError() {
	s.mu.Lock()
	s.lastError = ""
	s.mu.Unlock()
}

// emit publishes one event. It is safe to call from any goroutine, and inert
// before the application exists, which makes it safe from a startup path.
func (s *PlayerService) emit(name string, data any) {
	if app := application.Get(); app != nil {
		app.Event.Emit(name, data)
	}
}

// stateName maps the sealed state to the lowercase string the frontend switches
// on, so the UI never depends on the engine's numeric ordering.
func stateName(st molo.State) string {
	switch st {
	case molo.Playing:
		return "playing"
	case molo.Paused:
		return "paused"
	case molo.Stopped:
		return "stopped"
	default:
		return "idle"
	}
}

// queueRows builds the bound queue view, marking the current row and attaching
// the tags the index has resolved so far. A remote reference has no file, so
// its row is described from the catalogue metadata the YouTube Music index
// holds; without that, the row would show the raw "ytm:..." reference.
func (s *PlayerService) queueRows(paths []string, active int) []QueueRow {
	rows := make([]QueueRow, len(paths))
	for i, p := range paths {
		if isYTMRef(p) {
			rows[i] = ytmQueueRow(i, p, active, s.ytm)
			continue
		}
		tags := s.index.lookup(p)
		rows[i] = QueueRow{
			Index:   i,
			Path:    p,
			Name:    filepath.Base(p),
			Title:   tags.Title,
			Artist:  tags.Artist,
			Album:   tags.Album,
			CoverID: tags.CoverID,
			Active:  i == active,
		}
	}

	return rows
}

// ytmQueueRow describes one remote row from the catalogue index. Name stays
// empty until the catalogue answers, so a row the index has not seen reads as
// unnamed rather than showing its "ytm:..." identifier as a title.
func ytmQueueRow(i int, ref string, active int, index *ytmIndex) QueueRow {
	t, _ := index.lookup(ref)

	return QueueRow{
		Index:   i,
		Path:    ref,
		Name:    t.Title,
		Title:   t.Title,
		Artist:  t.Artist,
		Album:   t.Album,
		CoverID: t.coverID(),
		Active:  i == active,
	}
}

// codecOptions lists the decoder choices, Auto first. The label folds the
// registry family into the friendly name the way the TUI does, so "opus-pion"
// reads "Opus Portable" rather than a bare registry key.
func codecOptions() []CodecOption {
	codecs := molo.Codecs()
	out := make([]CodecOption, 0, len(codecs)+1)
	out = append(out, CodecOption{Name: "", Label: "Auto"})
	for _, c := range codecs {
		out = append(out, CodecOption{Name: c.Name, Label: codecLabel(c)})
	}

	return out
}

// backendOptions lists the playback backends with Auto first. It uses the
// user-facing list, so a dev-only sink such as "fake" never appears in the
// Output dropdown even though the engine can still open it by name.
func backendOptions() []string {
	return append([]string{""}, molo.UserBackends()...)
}

// collectDir walks root for playable files, sorted so the queue is
// deterministic. An unreadable subtree is skipped rather than fatal: a
// permission-denied folder should not lose the rest of the album.
func collectDir(root string, supported map[string]bool) ([]string, error) {
	var found []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}
		if !d.IsDir() && supported[strings.ToLower(filepath.Ext(path))] {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(found)

	return found, nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}

	return v
}
