// Package tui is a Bubble Tea model for the molo engine. It draws the now
// playing panel, the progress bar and the queue, and turns keystrokes into
// facade commands.
//
// The model is pure: Update is a function of the current model and one message,
// it never touches a terminal, and the engine behind it is the molo.Player
// interface. That is what lets the whole UI be driven from a test with a fake
// engine, and it is why the only engine package imported here is the facade.
package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/decode"
)

// tickInterval is the position refresh period. 4 Hz is smooth enough for a
// clock that shows whole seconds and cheap enough that polling never competes
// with the decoder. The level meter is deliberately not on this clock; a 4 Hz
// meter both lags and shimmers, so it runs on meterInterval instead.
const tickInterval = 250 * time.Millisecond

// meterInterval is the level meter refresh period. The tap drops frames it is
// not read fast enough, so a slow reader loses detail rather than seeing it
// late. 20 Hz is fast enough to look continuous and slow enough that the RMS
// pass stays negligible next to the decoder.
const meterInterval = 50 * time.Millisecond

// meterReadBlockFrames bounds one tap read, about 21 ms at 48 kHz, so a read
// usually returns one whole published block.
const meterReadBlockFrames = 1024

// meterMaxBlocks bounds how many blocks one meter tick drains. The tap holds
// about 680 ms, so 64 blocks (about 1.3 s) always empties it; the bound exists
// only so a stalled consumer cannot make a single tick spin.
const meterMaxBlocks = 64

// seekFlushDelay is how long a seek batch waits after the last key before it is
// issued. On the pure-Go Opus decoder a seek costs time proportional to its
// target, so a burst must collapse to one call; 150 ms is long enough to absorb
// a burst and short enough to feel immediate.
const seekFlushDelay = 150 * time.Millisecond

// maxAnchorTicks bounds how long the model holds a commanded seek target while
// waiting for the engine to confirm it. A seek can take hundreds of
// milliseconds, and the poll must not report the old position in that window;
// the bound exists so a dropped Seeked event cannot freeze the clock.
const maxAnchorTicks = 8

const (
	defaultWidth  = 80
	defaultHeight = 24
	barWidth      = 32
	meterWidth    = 24
)

// activeMarker flags the queue row that is playing.
const activeMarker = "▶"

// helpText is the one-line key legend. It names only bindings the model
// actually implements, so the legend is never a promise the UI cannot keep.
const helpText = "space pause/resume  n/p next/prev  h/l seek  +/- volume  c codec  q quit"

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	accentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

// model is the whole UI state. Every field is a value the test can inspect, and
// none of them is an engine handle except the Player itself.
type model struct {
	p molo.Player

	width, height int
	frame         int

	snap   molo.Snapshot
	queue  []string
	titles []string

	events     <-chan molo.Event
	eventsDone bool
	tap        molo.Tap

	meter meter
	err   error

	// now is the clock the meter releases against. It is a field so a test can
	// advance time deterministically; production leaves the default.
	now func() time.Time

	// debug is the bounded technical log shown under the main panel. The two
	// guards make the per-track identity lines fire once, not once per tick.
	debug              debugLog
	debugDecoderLogged bool
	debugMetaLogged    bool

	// The seek batch. seekSeq stamps the flush timer so a timer superseded by a
	// later press is dropped; seekPending is true while a trailing flush is due;
	// seekTarget is the accumulated target.
	seekSeq     uint64
	seekPending bool
	seekArmed   bool
	seekTarget  time.Duration

	// seekAnchor holds the last commanded target from a flush until the engine
	// agrees with it. A seek blocks the engine for a time proportional to its
	// target, during which the position poll still reports the old position;
	// without the anchor the next batch would accumulate from that stale read
	// and lose a step. anchorTicks bounds the wait so a dropped confirmation
	// cannot hold the clock at a target the engine never reached.
	seekAnchor   time.Duration
	seekAnchored bool
	anchorTicks  int

	// waveFn is nil unless the caller asked for a waveform, which keeps a
	// plain model free of the analysis dependency.
	waveFn  WaveFunc
	waveSeq uint64
	wave    *Wave
	waveCtx context.Context
	// cancelWave stops the in-flight pass when the track changes.
	cancelWave context.CancelFunc

	// The codec picker. codecs is the registry list read once at construction,
	// because Codecs() is stable for the life of the process; codecSetting is
	// the effective decoder preference mirrored from Settings, with "" meaning
	// automatic. pickerOpen, pickerCursor and pickerErr are pure view state.
	codecs       []decode.Codec
	codecSetting string
	pickerOpen   bool
	pickerCursor int
	pickerErr    error
}

// New builds the UI model over a Player. It is the entry point the command
// uses; the program is run by the caller.
func New(p molo.Player) tea.Model { return newModel(p) }

// NewWithWaveform is New plus a waveform. The caller supplies the producer, so
// the model stays decoupled from the analysis package and a test can stub the
// decode.
func NewWithWaveform(p molo.Player, fn WaveFunc) tea.Model {
	m := newModel(p)
	m.waveFn = fn

	return m
}

func newModel(p molo.Player) model {
	m := model{
		p:       p,
		width:   defaultWidth,
		height:  defaultHeight,
		waveCtx: context.Background(),
		now:     time.Now,
		// The registry is process-wide and stable, so the chooser list is read
		// once here rather than per frame.
		codecs: decode.Default.Codecs(),
	}
	if p != nil {
		m.events = p.Events()
		m.tap = p.Tap()
		// Snapshot is a cheap, non-blocking read by contract, so taking one
		// here means the first painted frame already shows the current track
		// instead of an empty screen until the first tick.
		m.snap = p.Snapshot()
		m.codecSetting = p.Settings().Decoder
	}

	return m
}

// Init starts the independent streams of the UI: engine events, the position
// poll, the queue read and the visualizer tap. None of them blocks the event
// loop, because each is a Cmd that runs off the Update goroutine.
func (m model) Init() tea.Cmd {
	return tea.Batch(m.nextEvent(), m.tick(), readQueue(m.p), meterTickCmd())
}

// nextEvent re-arms the event bridge unless the engine has closed its channel.
func (m model) nextEvent() tea.Cmd {
	if m.eventsDone || m.events == nil {
		return nil
	}

	return waitForEvent(m.events)
}

func (m model) tick() tea.Cmd {
	return tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg(time.Now()) })
}

// meterTickCmd arms the next meter read. The meter owns this interval rather
// than riding the position tick, so the level can look live while the engine
// snapshot and queue poll stay at 4 Hz.
func meterTickCmd() tea.Cmd {
	return tea.Tick(meterInterval, func(time.Time) tea.Msg { return meterTick{} })
}

// addSeek folds one seek key into the pending batch. The window is
// trailing-edge: each key restarts it, so a continuous burst becomes one seek
// once the keys stop rather than one per window. The target accumulates from
// the model's snapshot, not from the engine, so a burst never re-reads a
// position that is still moving.
func (m model) addSeek(delta time.Duration) (tea.Model, tea.Cmd) {
	if m.p == nil {
		return m, nil
	}

	if m.seekPending {
		m.seekTarget = clampSeekTarget(m.seekTarget, delta, m.snap.Duration)
	} else {
		m.seekPending = true
		m.seekTarget = clampSeekTarget(m.snap.Position, delta, m.snap.Duration)
	}

	// The sequence marks the batch's newest key. Only one timer is ever in
	// flight; it re-arms itself while the sequence keeps moving.
	m.seekSeq++
	if m.seekArmed {
		return m, nil
	}
	m.seekArmed = true

	return m, m.flushSeekCmd()
}

// flushSeekCmd fires once after the batch window. It carries the sequence of
// the key that armed it, so the flush can tell whether the window was extended.
func (m model) flushSeekCmd() tea.Cmd {
	seq := m.seekSeq

	return tea.Tick(seekFlushDelay, func(time.Time) tea.Msg { return seekFlushMsg{seq: seq} })
}

// flushSeek closes the batch. A key that arrived during the window bumps the
// sequence, so this flush re-arms instead of seeking; after a quiet window it
// applies the whole batch in exactly one engine call.
func (m model) flushSeek(seq uint64) (tea.Model, tea.Cmd) {
	if !m.seekPending || m.p == nil {
		return m, nil
	}
	if seq != m.seekSeq {
		return m, m.flushSeekCmd()
	}

	m.seekPending = false
	m.seekArmed = false
	// Move the displayed position with the target now and anchor it. The engine
	// confirms with a Seeked event, but a 4 Hz poll can read the pre-seek
	// position first; the anchor keeps that stale read from making the next
	// batch lose a step. The tick count bounds the anchor so a dropped
	// confirmation cannot freeze the clock.
	m.snap.Position = m.seekTarget
	m.seekAnchor = m.seekTarget
	m.seekAnchored = true
	m.anchorTicks = 0
	_ = m.p.Seek(m.seekTarget)

	return m, nil
}

// readQueue asks the engine for the current queue. The call is cheap, but it is
// still a Cmd: Update must never assume an engine call is instant.
func readQueue(p molo.Player) tea.Cmd {
	if p == nil {
		return nil
	}

	return func() tea.Msg { return queueMsg{paths: p.Queue()} }
}

// readTap drains the visualizer feed and returns the level of everything it
// held, as a command. It is a Cmd because even a bounded copy belongs off the
// Update goroutine; the engine's Tap never blocks and drops frames rather than
// stalling the audio thread, so a slow UI loses meter detail, never sound.
//
// It drains the whole feed rather than one block. At 20 Hz a tick covers about
// 50 ms while the device publishes in ~100 ms chunks, so a single read would
// often return nothing and the meter would strobe; draining also keeps the
// reader from skipping the frames a partial read would leave behind.
func readTap(t molo.Tap) tea.Cmd {
	if t == nil {
		return nil
	}

	return func() tea.Msg {
		level, frames := drainTap(t, meterReadBlockFrames)

		return meterMsg{level: level, frames: frames}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		// The picker is modal: while it is open every key goes to it, so the
		// main view's controls cannot fire behind the overlay.
		if m.pickerOpen {
			return m.handlePickerKey(msg)
		}
		if msg.String() == "c" {
			return m.openPicker(), nil
		}

		res := handleKey(m.p, msg)
		if res.quit {
			m.stopWave()

			return m, tea.Quit
		}
		if res.seeking {
			// A repeat is the same held key sending itself again. On the
			// pure-Go decoder a seek costs time proportional to its target, so
			// a held arrow key must not queue a full-cost jump per repeat.
			// Repeats of the other bindings are left alone: their actions are
			// cheap and holding them is a legitimate way to ramp.
			if msg.IsRepeat {
				return m, nil
			}

			return m.addSeek(res.delta)
		}

		return m, nil

	case seekFlushMsg:
		return m.flushSeek(msg.seq)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

		return m, nil

	case tickMsg:
		m.frame++
		if m.p != nil {
			fresh := m.p.Snapshot()
			// A seek may still be running, in which case the poll reports the
			// pre-seek position. Keep the commanded target until the engine
			// agrees with it, or until the anchor expires, so a later batch
			// never accumulates from a stale read.
			if m.seekAnchored {
				m.anchorTicks++
				if fresh.Position == m.seekAnchor || m.anchorTicks > maxAnchorTicks {
					m.seekAnchored = false
				} else {
					fresh.Position = m.snap.Position
				}
			}
			m.snap = fresh
			m.recordTitle()
			m.recordIdentity()
		}

		// The meter is not read here: it has its own stream at meterInterval.
		// Reading the tap from both would make two streams race to drain the
		// same ring, each seeing an arbitrary subset of the audio.
		return m, tea.Batch(m.tick(), readQueue(m.p))

	case stateMsg:
		m.snap.State = msg.to
		m.debug.push(formatStateLine(msg.to))

		return m, m.nextEvent()

	case trackMsg:
		m.resetForTrack(msg.index, msg.path)
		m.debug.push(formatTrackLine(msg.path))

		return m, tea.Batch(m.nextEvent(), m.startWave(msg.path))

	case seekedMsg:
		m.debug.push(formatSeekLine(msg.from, msg.position, msg.elapsed))
		// Confirmations arrive in command order, so this may be the older of
		// two seeks in flight. Only the one matching the anchor clears it and
		// moves the clock; an older one must not drag the position backwards.
		if m.seekAnchored && msg.position != m.seekAnchor {
			return m, m.nextEvent()
		}
		m.snap.Position = msg.position
		m.seekAnchored = false

		return m, m.nextEvent()

	case endedMsg:
		m.meter.reset()
		m.debug.push(formatEOSLine(m.snap.Path))

		return m, m.nextEvent()

	case failedMsg:
		m.err = msg.err
		m.debug.push(formatErrorLine(msg.err))

		return m, m.nextEvent()

	case eventsClosedMsg:
		m.eventsDone = true

		return m, nil

	case queueMsg:
		if !equalStrings(m.queue, msg.paths) {
			m.queue = msg.paths
			m.titles = make([]string, len(msg.paths))
		}

		return m, nil

	case meterMsg:
		// No frames means nothing was published since the last read; holding the
		// level is right, decaying it would read as a dropout that never
		// happened.
		if msg.frames > 0 {
			m.meter.push(msg.level, m.now())
		}

		return m, nil

	case meterTick:
		return m, tea.Batch(meterTickCmd(), readTap(m.tap))

	case waveMsg:
		// A pass that finished after its track was replaced must not paint
		// over the new track's waveform.
		if msg.seq != m.waveSeq {
			return m, nil
		}
		if msg.err == nil {
			m.wave = msg.wave
		}

		return m, nil

	case codecResultMsg:
		return m.applyCodecResult(msg), nil
	}

	return m, nil
}

// startWave cancels any in-flight pass and starts one for the new track. A
// model without a wave producer returns nil, which is the common case.
func (m *model) startWave(path string) tea.Cmd {
	m.stopWave()
	m.wave = nil
	if m.waveFn == nil || path == "" {
		return nil
	}

	m.waveSeq++
	ctx, cancel := context.WithCancel(m.waveCtx)
	m.cancelWave = cancel

	return loadWave(ctx, m.waveFn, m.waveSeq, path, waveBuckets)
}

// stopWave cancels the in-flight pass, if any. It is called on a track change
// and on quit, so a decode never outlives the reason it started.
func (m *model) stopWave() {
	if m.cancelWave != nil {
		m.cancelWave()
		m.cancelWave = nil
	}
}

// resetForTrack clears everything that described the previous track. A stale
// title, a stale position or a held meter level would all be lies about the new
// one until the engine reports in. It takes a pointer because it must mutate
// the model Update is holding.
func (m *model) resetForTrack(index int, path string) {
	m.snap.Path = path
	m.snap.Position = 0
	m.snap.Duration = 0
	m.snap.QueueIndex = index
	m.snap.Meta.Tags.Title = ""
	m.snap.Meta.Tags.Artist = ""
	m.snap.Meta.Tags.Album = ""
	m.snap.Decoder = ""
	m.snap.Parser = ""
	m.snap.Meta.Source = ""
	m.debugDecoderLogged = false
	m.debugMetaLogged = false
	m.err = nil
	m.meter.reset()

	// A batch aimed at the old track is meaningless on the new one. Bump the
	// sequence so its in-flight timer is dropped when it lands, and drop any
	// anchor from a seek that was still running.
	m.seekPending = false
	m.seekArmed = false
	m.seekAnchored = false
	m.seekSeq++

	if index >= 0 && index < len(m.titles) {
		m.titles[index] = ""
	}
}

// recordTitle remembers the resolved title of the current track so the queue
// can show it. Only the current track's tags are ever in a Snapshot, which is
// why this is learned as tracks come and go rather than fetched for the queue.
func (m *model) recordTitle() {
	idx := m.snap.QueueIndex
	if idx < 0 || idx >= len(m.titles) {
		return
	}
	if title := m.snap.Meta.Tags.Title; title != "" {
		m.titles[idx] = title
	}
}

// recordIdentity logs the decoder and the meta resolver for the current track,
// each at most once. Both arrive asynchronously after the track event, so they
// are learned from the poll; the guards stop a 4 Hz tick from repeating the same
// two lines for the life of the track.
func (m *model) recordIdentity() {
	if !m.debugDecoderLogged && m.snap.Decoder != "" {
		m.debug.push(formatDecoderLine(m.snap.Decoder, m.snap.Parser))
		m.debugDecoderLogged = true
	}
	if !m.debugMetaLogged && m.snap.Meta.Source != "" {
		m.debug.push(formatMetaLine(m.snap.Meta.Source))
		m.debugMetaLogged = true
	}
}

func (m model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "player"

	return v
}

func (m model) render() string {
	body := m.renderBody()

	var b strings.Builder
	b.WriteString(body)

	if panel := m.renderDebugPanel(m.width, m.debugLinesBudget(strings.Count(body, "\n")+1)); panel != "" {
		b.WriteString("\n\n")
		b.WriteString(panel)
	}

	return b.String()
}

// renderBody draws everything above the debug panel: the now playing line, the
// progress, the optional waveform and meter, the queue, the error and the key
// legend.
func (m model) renderBody() string {
	width := m.width
	if width <= 0 {
		width = defaultWidth
	}

	// The picker replaces the playing panel rather than overlaying it: a
	// terminal has no z-order, and a list appended under a full queue would be
	// easy to miss. The player keeps running behind it either way.
	if m.pickerOpen {
		return m.renderPicker(width)
	}

	var b strings.Builder

	title := displayName(m.snap.Meta.Tags.Title, m.snap.Path)
	if title == "" {
		title = "(nothing playing)"
	}
	b.WriteString(titleStyle.Render(clip(title, width)))
	b.WriteString("\n")

	if m.snap.State != molo.Idle {
		if line := trackSubtitle(m.snap.Meta.Tags.Artist, m.snap.Meta.Tags.Album); line != "" {
			b.WriteString(dimStyle.Render(clip(line, width)))
			b.WriteString("\n")
		}

		b.WriteString(formatProgress(m.snap.State, m.snap.Position, m.snap.Duration))
		b.WriteString("\n")
		b.WriteString(accentStyle.Render(renderBar(m.snap.Position, m.snap.Duration, barWidth, m.frame)))
		b.WriteString(" ")
		b.WriteString(dimStyle.Render("vol " + strconv.Itoa(volumePercent(m.snap.Volume)) + "%"))
		b.WriteString("\n")
	}

	if wave := renderWave(m.wave, waveWidth(m.width)); wave != "" {
		b.WriteString(accentStyle.Render(wave))
		b.WriteString("\n")
	}

	if meter := m.renderMeter(); meter != "" {
		b.WriteString(dimStyle.Render("level "))
		b.WriteString(accentStyle.Render(meter))
		b.WriteString("\n")
	}

	if q := m.renderQueueView(width); q != "" {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("queue"))
		b.WriteString("\n")
		b.WriteString(q)
		b.WriteString("\n")
	}

	if m.err != nil {
		b.WriteString("\n")
		b.WriteString(errStyle.Render(clip("error: "+m.err.Error(), width)))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(dimStyle.Render(helpText))

	return b.String()
}

func (m model) renderMeter() string {
	return meterBars(m.meter.level, meterWidth)
}

// waveWidth keeps the waveform one terminal wide, capped so a very wide window
// does not force a needlessly large bucket count.
func waveWidth(width int) int {
	if width <= 0 || width > waveBuckets {
		return waveBuckets
	}

	return width
}

// renderQueueView draws the queue with its resolved titles.
func (m model) renderQueueView(width int) string {
	return renderQueueWith(m.queue, m.titles, m.snap.QueueIndex, width)
}

// trackSubtitle joins the artist and album, skipping either when absent so a
// half-tagged file does not render a dangling separator.
func trackSubtitle(artist, album string) string {
	switch {
	case artist != "" && album != "":
		return artist + "  " + album
	case artist != "":
		return artist
	default:
		return album
	}
}

// renderQueueWith renders one row per track with the active index marked. The
// marker is the only cue that a row is current, so exactly one row may carry
// it, and an index outside the queue marks none.
func renderQueueWith(paths, titles []string, active, width int) string {
	if len(paths) == 0 {
		return ""
	}

	var b strings.Builder
	for i, path := range paths {
		name := displayName("", path)
		if i < len(titles) && titles[i] != "" {
			name = titles[i]
		}
		name = clip(name, width-4)

		if i == active {
			b.WriteString(accentStyle.Render(activeMarker + " " + name))
		} else {
			b.WriteString("  " + name)
		}
		if i < len(paths)-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// renderQueue is renderQueueWith without learned titles, for callers that only
// have paths.
func renderQueue(paths []string, active, width int) string {
	return renderQueueWith(paths, nil, active, width)
}

func equalStrings(a, b []string) bool {
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
