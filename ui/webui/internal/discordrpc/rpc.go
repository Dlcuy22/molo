package discordrpc

import (
	"log/slog"
	"sync"
	"time"
)

// Timing for the background loop. The sync tick re-sends the activity so the
// progress bar tracks a seek and the connection is noticed as dead; the retry
// tick reconnects when Discord was not running at startup or restarted under
// us. Both are coarse on purpose: presence is cosmetic and must not cost a
// wakeup a second.
const (
	syncInterval  = 5 * time.Second
	retryInterval = 30 * time.Second
)

// Config is the static Discord side of the integration: the application id the
// activity is posted under and the registered asset keys used when a track has
// no artwork to show. It is set once and never changes while running.
type Config struct {
	// AppID is the Discord application the activity belongs to. It must be a
	// real application, because Discord validates the asset keys and image URLs
	// against it.
	AppID string
	// LargeAssetKey and SmallAssetKey are registered asset names shown when the
	// track has no cover or artist image. They fall back to a plain text
	// presence when empty.
	LargeAssetKey  string
	LargeAssetText string
	SmallAssetKey  string
	SmallAssetText string
}

// TrackInfo is one snapshot of playback, pushed from the UI. Image fields are
// already resolved to something Discord accepts: an https URL it can fetch, a
// registered asset key, or empty for "nothing".
type TrackInfo struct {
	Title  string
	Artist string

	// LargeImage is the album cover: an https URL or an asset key. LargeText is
	// the hover tooltip, usually the album.
	LargeImage string
	LargeText  string
	// SmallImage is the artist image, drawn as the small badge over the cover.
	SmallImage string
	SmallText  string

	// DurationMs is the whole track; zero means unknown, which drops the end
	// timestamp so Discord shows an open-ended elapsed clock. It is only a
	// fallback: the manager prefers the live duration callback, because the
	// engine learns a track's length asynchronously and this snapshot may have
	// been taken before the probe answered.
	DurationMs int64
	// Paused freezes the clock: no timestamps are sent, so Discord draws no
	// moving progress bar.
	Paused bool

	// ActivityType is Discord's verb: 2 Listening, 0 Playing, 3 Watching.
	ActivityType int

	// Buttons are the clickable links under the activity, at most two.
	Buttons []Button
}

// Button is one clickable link on the activity.
type Button struct {
	Label string
	URL   string
}

// Manager owns the Discord IPC connection and the periodic re-sync. It is safe
// for concurrent use: Update can be called from the snapshot pump while the
// loop reads the current track.
type Manager struct {
	cfg Config
	log *slog.Logger

	// GetPosition, GetDuration and IsPaused are polled on every send, so the
	// progress bar follows a seek and the end time appears once the engine's
	// probe answers, even between track changes. All may be nil.
	GetPosition func() int64
	GetDuration func() int64
	IsPaused    func() bool

	mu        sync.Mutex
	enabled   bool
	connected bool
	current   TrackInfo
	discord   *ipcClient

	// stopCh, doneCh and wakeCh are owned by Start and captured by the loop, so
	// the loop never reads them from the struct and Stop never reassigns a
	// channel a running loop still holds. wakeCh carries a coalesced "the track
	// changed, send now" signal, so Update does not touch the IPC socket itself.
	stopCh chan struct{}
	doneCh chan struct{}
	wakeCh chan struct{}
}

// New builds a manager for one application. It does not connect; Start does.
func New(cfg Config, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}

	return &Manager{
		cfg:     cfg,
		log:     log,
		discord: newIPCClient(cfg.AppID),
	}
}

// Start enables presence and runs the connect/sync loop until Stop. It is
// idempotent: a second Start while running is ignored.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.enabled {
		m.mu.Unlock()

		return
	}
	m.enabled = true
	// Fresh channels per run, created under the lock and handed to the loop, so
	// a restart after Stop cannot reuse a closed channel.
	stop := make(chan struct{})
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	m.stopCh, m.doneCh, m.wakeCh = stop, done, wake
	m.mu.Unlock()

	go m.loop(stop, done, wake)
}

// Stop disables presence, closes the IPC connection and waits for the loop to
// exit. After it returns the manager can be started again.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.enabled {
		m.mu.Unlock()

		return
	}
	m.enabled = false
	stop, done := m.stopCh, m.doneCh
	m.mu.Unlock()

	close(stop)
	// Bounded wait: a Discord that died mid-write can leave the IPC call
	// blocked, and shutdown must not hang on a cosmetic integration. The loop
	// owns the socket and closes it on the way out, so Stop never touches the
	// client itself; a timed-out loop therefore cannot race a Logout here.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		m.log.Warn("discordrpc: loop did not stop in time; leaving it to close the socket")
	}
}

// Connected reports whether the IPC socket is open, so the settings UI can show
// a status line without reaching into Discord itself.
func (m *Manager) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.connected
}

// Update records the current track and asks the loop to send it. It never
// touches the IPC socket itself: the loop is the sole sender, which is what
// keeps the unsynchronized client from being called from two goroutines. The
// wake is coalesced, so a burst of updates costs one send.
func (m *Manager) Update(track TrackInfo) {
	m.mu.Lock()
	m.current = track
	wake := m.wakeCh
	m.mu.Unlock()

	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// Clear removes the presence without stopping the manager, which is what an
// empty queue means. Like Update it only asks the loop to act.
func (m *Manager) Clear() {
	m.mu.Lock()
	m.current = TrackInfo{}
	wake := m.wakeCh
	m.mu.Unlock()

	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// loop connects, then re-sends the activity on a timer until Stop. It is the
// only goroutine that touches the Discord client, so the client needs no lock
// of its own. It owns the socket for the whole run and closes it on the way
// out, before signalling done.
func (m *Manager) loop(stop, done, wake chan struct{}) {
	defer close(done)
	defer m.disconnect()

	if err := m.connect(); err != nil {
		m.log.Debug("discordrpc: initial connect failed, will retry", "error", err)
	} else {
		// Describe whatever was set before the socket opened, so enabling
		// presence mid-track does not wait for the next sync tick.
		m.setActivity()
	}

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()
	retryTicker := time.NewTicker(retryInterval)
	defer retryTicker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-wake:
			// A track changed. Send only if the socket is up; otherwise the
			// retry tick will describe it once connected.
			if m.isConnected() {
				m.setActivity()
			}
		case <-syncTicker.C:
			if !m.isConnected() {
				continue
			}
			m.setActivity()
		case <-retryTicker.C:
			if !m.isConnected() {
				if err := m.connect(); err == nil {
					m.setActivity()
				}
			}
		}
	}
}

func (m *Manager) isConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.connected
}

func (m *Manager) connect() error {
	if err := m.discord.login(); err != nil {
		return err
	}
	m.mu.Lock()
	m.connected = true
	m.mu.Unlock()
	m.log.Info("discordrpc: connected to Discord")

	return nil
}

func (m *Manager) disconnect() {
	m.discord.logout()
	m.mu.Lock()
	m.connected = false
	m.mu.Unlock()
}

func (m *Manager) markDisconnected(err error) {
	m.log.Debug("discordrpc: SetActivity failed, dropping connection", "error", err)
	m.mu.Lock()
	m.connected = false
	m.mu.Unlock()
}

// setActivity renders the stored track and sends it. It polls the live position
// and pause state so a seek or a pause is reflected on the next tick without a
// new Update.
func (m *Manager) setActivity() {
	m.mu.Lock()
	track := m.current
	m.mu.Unlock()

	if track.Title == "" {
		if err := m.discord.setActivity(nil); err != nil {
			m.markDisconnected(err)
		}

		return
	}

	paused := track.Paused
	if m.IsPaused != nil {
		paused = m.IsPaused()
	}
	elapsedMs := int64(0)
	if m.GetPosition != nil {
		elapsedMs = m.GetPosition()
	}
	durationMs := track.DurationMs
	if m.GetDuration != nil {
		if live := m.GetDuration(); live > 0 {
			durationMs = live
		}
	}

	card := buildActivity(track, m.cfg, paused, elapsedMs, durationMs)

	if err := m.discord.setActivity(card); err != nil {
		m.markDisconnected(err)

		return
	}
	m.log.Debug("discordrpc: activity set", "title", track.Title, "large", card.LargeImage, "small", card.SmallImage)
}

// buildActivity renders one card from the track, the static config and the live
// pause/position/duration. It is a free function so the mapping is testable
// without a Discord socket: the manager only adds the connection around it.
//
// The card header reads "Listening to <Name>", so Name carries the artist to
// match the reference client; the title and artist then repeat as the two body
// lines.
func buildActivity(track TrackInfo, cfg Config, paused bool, elapsedMs, durationMs int64) *activity {
	largeImage := track.LargeImage
	largeText := track.LargeText
	if largeImage == "" {
		largeImage = cfg.LargeAssetKey
		if largeText == "" {
			largeText = cfg.LargeAssetText
		}
	}

	smallImage := track.SmallImage
	smallText := track.SmallText
	if smallImage == "" {
		smallImage = cfg.SmallAssetKey
		if smallText == "" {
			smallText = cfg.SmallAssetText
		}
	}

	artist := track.Artist
	if artist == "" {
		artist = "Unknown artist"
	}

	var stamps *timestamps
	if !paused {
		now := time.Now()
		start := now.Add(-time.Duration(elapsedMs) * time.Millisecond)
		stamps = &timestamps{Start: &start}
		if durationMs > 0 {
			end := start.Add(time.Duration(durationMs) * time.Millisecond)
			stamps.End = &end
		}
	}

	return &activity{
		Type:       track.ActivityType,
		Name:       artist,
		Details:    track.Title,
		State:      artist,
		LargeImage: largeImage,
		LargeText:  largeText,
		SmallImage: smallImage,
		SmallText:  smallText,
		Timestamps: stamps,
		Buttons:    completeButtons(track.Buttons),
	}
}

// completeButtons drops any button with an empty label or URL, because Discord
// rejects a half-built button.
func completeButtons(buttons []Button) []Button {
	out := make([]Button, 0, len(buttons))
	for _, b := range buttons {
		if b.Label == "" || b.URL == "" {
			continue
		}
		out = append(out, b)
	}

	return out
}
