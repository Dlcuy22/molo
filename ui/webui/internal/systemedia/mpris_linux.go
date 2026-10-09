//go:build linux

// MPRIS2 transport: molo as a media player the Linux desktop can read and
// drive.
//
// The bus name org.mpris.MediaPlayer2.<n> and the object path
// /org/mpris/MediaPlayer2 are claimed, the two required interfaces are
// exported, and org.freedesktop.DBus.Properties is served from a
// prop.Properties so a state change reaches every shell and applet as a
// PropertiesChanged signal. Media keys, headset buttons and lock-screen
// controls all speak this.
//
// Hand-rolled on godbus (BSD) rather than a GPL server package, which is why
// the package exists at all.
package systemedia

import (
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlcuy22/molo/ui/webui/internal/artcache"
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// The three names the spec fixes. The bus name must start with
// org.mpris.MediaPlayer2; the path is the single entry point clients probe.
const (
	busName     = "org.mpris.MediaPlayer2.molo"
	rootPath    = dbus.ObjectPath("/org/mpris/MediaPlayer2")
	ifaceRoot   = "org.mpris.MediaPlayer2"
	ifacePlayer = "org.mpris.MediaPlayer2.Player"
	// noTrack is the spec's "nothing is playing" track id. It is reserved for
	// this purpose and must never be used for a real track.
	noTrack = dbus.ObjectPath("/org/mpris/MediaPlayer2/TrackList/NoTrack")
	// trackPathPrefix is where real track ids live. The spec forbids claiming
	// paths under /org/mpris beyond the ones it defines, so the ids sit under
	// the application's own root instead.
	trackPathPrefix = "/molo/track/"
)

// appDesktopEntry and appIdentity name the application in the desktop's media
// control. DesktopEntry is what the shell matches against a .desktop file, so
// it is the file's stem, not the display name.
const (
	appIdentity     = "molo"
	appDesktopEntry = "molo"
)

// backend is the whole MPRIS transport. It owns the bus connection, the
// exported property set, and the last published track.
type backend struct {
	log  *slog.Logger
	cmd  Commands
	conn *dbus.Conn

	mu    sync.Mutex
	props *prop.Properties
	// started reports whether Start completed, so a command that arrives after
	// Stop is dropped rather than run against a torn-down application. It is a
	// flag of its own rather than a check on conn, so the dispatch guard is
	// testable without a bus.
	started bool
	// trackID is the id of the published track, kept so SetPosition can reject
	// a call aimed at a track that already ended.
	trackID dbus.ObjectPath
	// artKey and artFile cache the artwork file written for the current cover,
	// so a 4 Hz tick does not stat or rewrite it. Keyed by the content hash.
	artKey  string
	artFile string
}

// newBackend builds the MPRIS transport. Start does the work; this only wires
// the command surface into the exported views.
func newBackend(cmd Commands, log *slog.Logger) (Backend, error) {
	if log == nil {
		log = slog.Default()
	}

	return &backend{cmd: cmd, log: log}, nil
}

// rootView exposes only the org.mpris.MediaPlayer2 methods. The two views are
// separate types on purpose: godbus registers every exported method of a value
// under the interface it is exported for, so one combined type would make the
// player's methods callable through the root interface as well.
type rootView struct{ b *backend }

// playerView exposes only the org.mpris.MediaPlayer2.Player methods.
type playerView struct{ b *backend }

// Start connects to the session bus, exports the two interfaces plus
// Properties, and claims the bus name. The name is claimed last so a client can
// never see the object before it is fully wired.
func (b *backend) Start() error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("mpris: connect session bus: %w", err)
	}
	b.mu.Lock()
	b.conn = conn
	b.mu.Unlock()

	root := rootView{b}
	player := playerView{b}
	if err := conn.Export(root, rootPath, ifaceRoot); err != nil {
		b.closeConn()

		return fmt.Errorf("mpris: export root: %w", err)
	}
	if err := conn.ExportWithMap(player, map[string]string{"SeekRelative": "Seek"}, rootPath, ifacePlayer); err != nil {
		b.closeConn()

		return fmt.Errorf("mpris: export player: %w", err)
	}

	props, err := prop.Export(conn, rootPath, b.propertyMap())
	if err != nil {
		b.closeConn()

		return fmt.Errorf("mpris: export properties: %w", err)
	}
	b.mu.Lock()
	b.props = props
	b.started = true
	b.trackID = noTrack
	b.mu.Unlock()

	node := introspect.Node{
		Name: string(rootPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			rootIntrospect(props),
			playerIntrospect(props),
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(&node), rootPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		b.closeConn()

		return fmt.Errorf("mpris: export introspection: %w", err)
	}

	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	if err != nil {
		b.closeConn()

		return fmt.Errorf("mpris: request name: %w", err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		b.closeConn()

		return fmt.Errorf("mpris: bus name %s already owned", busName)
	}
	b.log.Info("mpris: serving", "name", busName)

	return nil
}

// Stop releases the bus name and drops the connection. It is idempotent and
// safe after a failed Start.
func (b *backend) Stop() {
	b.mu.Lock()
	conn := b.conn
	b.props = nil
	b.started = false
	// Clear the cached art URL with the connection: a restart re-derives it,
	// and a stale entry would skip the cache-key check on the next track.
	b.artKey = ""
	b.artFile = ""
	b.mu.Unlock()
	if conn == nil {
		return
	}
	// Claim the name no longer. The reply is ignored: on a forced bus shutdown
	// there is nothing useful to do with a failure, and the connection close
	// below releases the name anyway.
	_, _ = conn.ReleaseName(busName)
	_ = conn.Close()
	b.mu.Lock()
	b.conn = nil
	b.mu.Unlock()
	b.log.Info("mpris: stopped")
}

// closeConn tears down a half-built transport when Start fails partway.
func (b *backend) closeConn() {
	b.Stop()
}

// Update publishes the current track. Only properties whose value actually
// changed are written, because prop.Properties emits PropertiesChanged on every
// Set and a 4 Hz tick would otherwise flood the bus. Position is the exception
// in reverse: written on every call (EmitFalse, so no signal) because it moves
// continuously and clients read it on demand.
func (b *backend) Update(t TrackInfo) {
	artURL := b.artFor(t.Artwork, t.MIME)

	b.set(ifacePlayer, "PlaybackStatus", playbackStatus(t))
	b.set(ifacePlayer, "Metadata", metadataMap(t, artURL))
	b.set(ifacePlayer, "Position", t.Position.Microseconds())
	b.set(ifacePlayer, "Volume", t.Volume)
	b.set(ifacePlayer, "Shuffle", t.Shuffle)
	b.set(ifacePlayer, "LoopStatus", loopName(t.Loop))
	b.set(ifacePlayer, "CanGoNext", t.CanGoNext)
	b.set(ifacePlayer, "CanGoPrevious", t.CanGoPrevious)
	b.set(ifacePlayer, "CanPlay", t.CanControl)
	b.set(ifacePlayer, "CanPause", t.CanControl)
	b.set(ifacePlayer, "CanSeek", t.CanSeek)

	b.mu.Lock()
	b.trackID = trackPath(t.ID)
	b.mu.Unlock()
}

// Clear publishes "nothing is playing": the status drops to Stopped and the
// track id becomes the spec's NoTrack, so the desktop stops drawing a track.
func (b *backend) Clear() {
	b.set(ifacePlayer, "PlaybackStatus", "Stopped")
	b.set(ifacePlayer, "Metadata", map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(noTrack),
	})
	b.set(ifacePlayer, "Position", int64(0))
	b.mu.Lock()
	b.trackID = noTrack
	b.mu.Unlock()
}

// Seeked announces a position change the desktop did not cause, so a progress
// bar jumps instead of waiting for the next poll. Position itself is not
// signalled, so the current value is written first and Seeked carries the new
// one.
func (b *backend) Seeked(position time.Duration) {
	us := position.Microseconds()
	b.set(ifacePlayer, "Position", us)

	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return
	}
	if err := conn.Emit(rootPath, ifacePlayer+".Seeked", us); err != nil {
		b.log.Debug("mpris: emit Seeked failed", "error", err)
	}
}

// set writes one property, skipping the write when the value already matches.
// The read is unsynchronized against a concurrent client Set, which is
// harmless: the worst case is one redundant PropertiesChanged.
func (b *backend) set(iface, name string, v any) {
	b.mu.Lock()
	props := b.props
	b.mu.Unlock()
	if props == nil {
		return
	}
	if reflect.DeepEqual(props.GetMust(iface, name), v) {
		return
	}
	props.SetMust(iface, name, v)
}

// dispatch runs one inbound command off the bus goroutine. It reports whether
// the command was accepted: a nil command or a stopped transport is refused, so
// a setter can tell the client the capability is not there instead of storing a
// value nothing honours.
//
// A property setter reaches this while prop.Properties holds its write lock, so
// the command must not run inline: the player's own update path takes that same
// lock, and the synchronous route would deadlock. A panic is logged rather than
// left to kill the process from a library goroutine.
func (b *backend) dispatch(fn func()) bool {
	if fn == nil {
		return false
	}
	b.mu.Lock()
	started := b.started
	b.mu.Unlock()
	if !started {
		return false
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("mpris: command panicked", "panic", r)
			}
		}()
		fn()
	}()

	return true
}

// artFor returns a file:// URL for the cover art, writing the bytes to the art
// cache on first sight so the shell can read them. Keyed by content, so the same
// cover across many tracks is written once. It returns "" when there is no art
// or the cache is unusable, which MPRIS reads as "no artwork".
func (b *backend) artFor(data []byte, mime string) string {
	if len(data) == 0 {
		return ""
	}

	key := artcache.Key(data)
	b.mu.Lock()
	if b.artKey == key {
		file := b.artFile
		b.mu.Unlock()

		return file
	}
	b.mu.Unlock()

	dir, err := artcache.Dir()
	if err != nil {
		return ""
	}
	ext := ".jpg"
	if strings.Contains(mime, "png") {
		ext = ".png"
	}
	path := filepath.Join(dir, "mpris-"+key+ext)
	if _, err := os.Stat(path); err != nil {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			b.log.Debug("mpris: could not write artwork", "error", err)

			return ""
		}
	}

	file := (&url.URL{Scheme: "file", Path: path}).String()
	b.mu.Lock()
	b.artKey = key
	b.artFile = file
	b.mu.Unlock()

	return file
}

// propertyMap is the initial value and behaviour of every exported property.
// EmitConst marks the ones the spec says never change; EmitFalse is Position,
// which the spec explicitly forbids signalling.
func (b *backend) propertyMap() prop.Map {
	root := map[string]*prop.Prop{
		"CanQuit":             {Value: true, Emit: prop.EmitConst},
		"CanRaise":            {Value: true, Emit: prop.EmitConst},
		"HasTrackList":        {Value: false, Emit: prop.EmitConst},
		"Identity":            {Value: appIdentity, Emit: prop.EmitConst},
		"DesktopEntry":        {Value: appDesktopEntry, Emit: prop.EmitConst},
		"SupportedUriSchemes": {Value: []string{}, Emit: prop.EmitConst},
		"SupportedMimeTypes":  {Value: supportedMimeTypes(), Emit: prop.EmitConst},
	}

	player := map[string]*prop.Prop{
		"PlaybackStatus": {Value: "Stopped", Emit: prop.EmitTrue},
		"Metadata":       {Value: map[string]dbus.Variant{}, Emit: prop.EmitTrue},
		"Position":       {Value: int64(0), Emit: prop.EmitFalse},
		"Volume":         {Value: 1.0, Writable: true, Emit: prop.EmitTrue, Callback: b.onVolume},
		"Rate":           {Value: 1.0, Writable: true, Emit: prop.EmitTrue, Callback: b.onRate},
		"Shuffle":        {Value: false, Writable: true, Emit: prop.EmitTrue, Callback: b.onShuffle},
		"LoopStatus":     {Value: "None", Writable: true, Emit: prop.EmitTrue, Callback: b.onLoop},
		"MinimumRate":    {Value: 1.0, Emit: prop.EmitConst},
		"MaximumRate":    {Value: 1.0, Emit: prop.EmitConst},
		"CanControl":     {Value: true, Emit: prop.EmitConst},
		"CanPlay":        {Value: true, Emit: prop.EmitTrue},
		"CanPause":       {Value: true, Emit: prop.EmitTrue},
		"CanSeek":        {Value: false, Emit: prop.EmitTrue},
		"CanGoNext":      {Value: false, Emit: prop.EmitTrue},
		"CanGoPrevious":  {Value: false, Emit: prop.EmitTrue},
	}

	return prop.Map{ifaceRoot: root, ifacePlayer: player}
}

// onVolume forwards a client volume write to the engine. prop.Properties has
// already validated the type and value signature before the callback runs.
func (b *backend) onVolume(c *prop.Change) *dbus.Error {
	v, ok := c.Value.(float64)
	if !ok {
		return prop.ErrInvalidArg
	}
	if !b.dispatch(func() {
		if b.cmd.SetVolume != nil {
			b.cmd.SetVolume(v)
		}
	}) {
		return dbus.MakeFailedError(errors.New("mpris: volume is not controllable"))
	}

	return nil
}

// onRate accepts the only rate the engine can play, so a client that tries to
// change it gets an honest error instead of a silent lie.
func (b *backend) onRate(c *prop.Change) *dbus.Error {
	if v, ok := c.Value.(float64); ok && v == 1.0 {
		return nil
	}

	return dbus.MakeFailedError(errors.New("mpris: playback rate is fixed at 1.0"))
}

// onShuffle forwards a client shuffle write to the engine, refusing it when the
// engine has no shuffle to set rather than storing a value nothing honours.
func (b *backend) onShuffle(c *prop.Change) *dbus.Error {
	v, ok := c.Value.(bool)
	if !ok {
		return prop.ErrInvalidArg
	}
	if !b.dispatch(func() {
		if b.cmd.SetShuffle != nil {
			b.cmd.SetShuffle(v)
		}
	}) {
		return dbus.MakeFailedError(errors.New("mpris: shuffle is not controllable"))
	}

	return nil
}

// onLoop refuses a repeat-mode write: the engine has no repeat mode, so a
// stored value would be a lie. The property is writable only because the spec
// marks it so, and a client that tries gets an honest error.
func (b *backend) onLoop(c *prop.Change) *dbus.Error {
	return dbus.MakeFailedError(errors.New("mpris: repeat mode is not supported"))
}

// ---- org.mpris.MediaPlayer2 ----

// Raise asks the UI to surface itself. The window is owned by the app, not this
// package, so the command is whatever the caller wired.
func (v rootView) Raise() *dbus.Error {
	v.b.dispatch(v.b.cmd.Raise)

	return nil
}

// Quit asks the application to exit. It is dispatched, never run inline, so a
// quitting app can tear the bus connection down without deadlocking the call
// that asked for it.
func (v rootView) Quit() *dbus.Error {
	v.b.dispatch(v.b.cmd.Quit)

	return nil
}

// ---- org.mpris.MediaPlayer2.Player ----

func (v playerView) Play() *dbus.Error      { v.b.dispatch(v.b.cmd.Play); return nil }
func (v playerView) Pause() *dbus.Error     { v.b.dispatch(v.b.cmd.Pause); return nil }
func (v playerView) PlayPause() *dbus.Error { v.b.dispatch(v.b.cmd.PlayPause); return nil }
func (v playerView) Stop() *dbus.Error      { v.b.dispatch(v.b.cmd.Stop); return nil }
func (v playerView) Next() *dbus.Error      { v.b.dispatch(v.b.cmd.Next); return nil }
func (v playerView) Previous() *dbus.Error  { v.b.dispatch(v.b.cmd.Previous); return nil }

// SeekRelative moves by a signed offset in microseconds, which is the spec's
// relative seek. The engine only takes absolute positions, so the caller's
// command is handed the offset and resolves it against the live position.
//
// The exported name is Seek; the method is named for its direction here
// because Go's io.Seeker owns the plain Seek name and vet flags any other
// shape for it.
func (v playerView) SeekRelative(offset int64) *dbus.Error {
	v.b.dispatch(func() {
		if v.b.cmd.Seek != nil {
			v.b.cmd.Seek(time.Duration(offset) * time.Microsecond)
		}
	})

	return nil
}

// SetPosition sets an absolute position, but only for the track the client
// thinks is current. The guard is the spec's answer to the race between a seek
// and a track change: a stale id is ignored, not applied to the wrong track.
func (v playerView) SetPosition(trackID dbus.ObjectPath, position int64) *dbus.Error {
	v.b.mu.Lock()
	current := v.b.trackID
	v.b.mu.Unlock()
	if trackID != current {
		return nil
	}
	v.b.dispatch(func() {
		if v.b.cmd.SeekTo != nil {
			v.b.cmd.SeekTo(time.Duration(position) * time.Microsecond)
		}
	})

	return nil
}

// ---- mapping helpers ----

// playbackStatus is the MPRIS enum for the engine's state. Idle and Stopped
// both read as "Stopped", which is the spec's "no track is playing".
func playbackStatus(t TrackInfo) string {
	switch {
	case t.Playing:
		return "Playing"
	case t.Paused:
		return "Paused"
	default:
		return "Stopped"
	}
}

// loopName normalizes a loop mode to the three spec values. Anything unknown
// becomes "None" so a stale value can never put an invalid string on the bus.
func loopName(s string) string {
	switch s {
	case "Track", "Playlist":
		return s
	default:
		return "None"
	}
}

// trackPath derives the mpris:trackid from the track reference. It must be a
// valid D-Bus object path, stable for one track and distinct across tracks, so
// the reference is hashed rather than embedded.
func trackPath(ref string) dbus.ObjectPath {
	if ref == "" {
		return noTrack
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(ref))

	return dbus.ObjectPath(trackPathPrefix + strconv.FormatUint(h.Sum64(), 16))
}

// metadataMap is the xesam metadata the desktop draws the track from. Only
// present fields are included: the spec treats a missing key as "unknown",
// while an empty string is a real value a client would render as blank.
func metadataMap(t TrackInfo, artURL string) map[string]dbus.Variant {
	meta := map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(trackPath(t.ID)),
	}
	if t.Title != "" {
		meta["xesam:title"] = dbus.MakeVariant(t.Title)
	}
	if t.Artist != "" {
		meta["xesam:artist"] = dbus.MakeVariant([]string{t.Artist})
	}
	if t.Album != "" {
		meta["xesam:album"] = dbus.MakeVariant(t.Album)
	}
	if t.Duration > 0 {
		meta["mpris:length"] = dbus.MakeVariant(t.Duration.Microseconds())
	}
	if artURL != "" {
		meta["mpris:artUrl"] = dbus.MakeVariant(artURL)
	}

	return meta
}

// supportedMimeTypes is what OpenUri would accept. OpenUri is not implemented,
// so this is informational and paired with an empty URI scheme list, which the
// spec allows.
func supportedMimeTypes() []string {
	return []string{
		"audio/flac", "audio/ogg", "audio/opus", "audio/mpeg",
		"audio/mp4", "audio/aac", "audio/wav", "audio/x-wav",
	}
}

// rootIntrospect describes org.mpris.MediaPlayer2. The property list comes from
// the exported Properties, so the introspection and the served values cannot
// drift.
func rootIntrospect(props *prop.Properties) introspect.Interface {
	return introspect.Interface{
		Name:       ifaceRoot,
		Methods:    []introspect.Method{{Name: "Raise"}, {Name: "Quit"}},
		Properties: props.Introspection(ifaceRoot),
	}
}

// playerIntrospect describes org.mpris.MediaPlayer2.Player.
func playerIntrospect(props *prop.Properties) introspect.Interface {
	return introspect.Interface{
		Name: ifacePlayer,
		Methods: []introspect.Method{
			{Name: "Next"},
			{Name: "Previous"},
			{Name: "Pause"},
			{Name: "PlayPause"},
			{Name: "Stop"},
			{Name: "Play"},
			{Name: "Seek", Args: []introspect.Arg{{Name: "Offset", Type: "x", Direction: "in"}}},
			{Name: "SetPosition", Args: []introspect.Arg{
				{Name: "TrackId", Type: "o", Direction: "in"},
				{Name: "Position", Type: "x", Direction: "in"},
			}},
		},
		Signals: []introspect.Signal{
			{Name: "Seeked", Args: []introspect.Arg{{Name: "Position", Type: "x", Direction: "out"}}},
		},
		Properties: props.Introspection(ifacePlayer),
	}
}
