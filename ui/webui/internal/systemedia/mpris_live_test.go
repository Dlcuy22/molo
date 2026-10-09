//go:build linux

package systemedia

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// The tests in this file talk to a real session bus, so they are skipped
// wherever one is not running (CI, a container). They are the only proof that
// the exported object, the properties and the inbound method dispatch actually
// work against a live bus rather than against the mapping helpers alone.

// liveBus connects a client to the session bus, skipping the test when there is
// none. It returns the client connection, which is deliberately separate from
// the backend's own so the calls travel the bus like any other client's.
func liveBus(t *testing.T) *dbus.Conn {
	t.Helper()

	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no session bus")
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("cannot connect to the session bus: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// startLiveBackend builds and starts a backend on the live bus, with a
// recording command surface, and stops it at cleanup.
func startLiveBackend(t *testing.T) (*backend, *recorder) {
	t.Helper()

	// A real molo may already be running on the developer's bus and holding the
	// name. That is an environment condition, not a failure of this code, so the
	// test skips rather than reporting a collision.
	if owner, err := nameOwner(t); err == nil && owner != "" {
		t.Skipf("bus name %s already owned by %s; run under dbus-run-session", busName, owner)
	}

	rec := &recorder{calls: make(chan string, 16)}
	cmd := Commands{
		Play:      func() { rec.record("play") },
		Pause:     func() { rec.record("pause") },
		PlayPause: func() { rec.record("playpause") },
		Next:      func() { rec.record("next") },
		Previous:  func() { rec.record("previous") },
		Stop:      func() { rec.record("stop") },
		SeekTo:    func(d time.Duration) { rec.record("seekto:" + d.String()) },
		SetVolume: func(v float64) { rec.record("volume") },
	}
	b, err := newBackend(cmd, slog.Default())
	if err != nil {
		t.Fatalf("newBackend: %v", err)
	}
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(b.Stop)

	return b.(*backend), rec
}

type recorder struct {
	calls chan string
}

// nameOwner reports the unique bus name that currently owns name, or "" when
// the name is free. It connects on its own short-lived connection so it does
// not depend on the backend under test.
func nameOwner(t *testing.T) (string, error) {
	t.Helper()
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	var owner string
	if err := conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, busName).Store(&owner); err != nil {
		return "", err
	}

	return owner, nil
}

func (r *recorder) record(s string) {
	select {
	case r.calls <- s:
	default:
	}
}

func (r *recorder) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-r.calls:
		if got != want {
			t.Fatalf("command = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no command received, want %q", want)
	}
}

// TestLiveMPRISPropertiesRoundTrip pins the read path: a client on the bus sees
// the playback status, metadata and position the backend published, in the
// shapes the spec defines.
func TestLiveMPRISPropertiesRoundTrip(t *testing.T) {
	client := liveBus(t)
	b, _ := startLiveBackend(t)

	b.Update(TrackInfo{
		ID:         "/music/a.flac",
		Title:      "A Song",
		Artist:     "An Artist",
		Album:      "An Album",
		Duration:   90 * time.Second,
		Position:   3 * time.Second,
		Playing:    true,
		CanControl: true,
		CanSeek:    true,
		CanGoNext:  true,
	})

	obj := client.Object(busName, rootPath)

	status, err := obj.GetProperty(ifacePlayer + ".PlaybackStatus")
	if err != nil {
		t.Fatalf("PlaybackStatus: %v", err)
	}
	if status.Value() != "Playing" {
		t.Fatalf("PlaybackStatus = %v, want Playing", status.Value())
	}

	metaVar, err := obj.GetProperty(ifacePlayer + ".Metadata")
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	meta, ok := metaVar.Value().(map[string]dbus.Variant)
	if !ok {
		t.Fatalf("Metadata = %T, want a{sv}", metaVar.Value())
	}
	if got := meta["xesam:title"].Value(); got != "A Song" {
		t.Fatalf("xesam:title = %v", got)
	}
	if got := meta["mpris:length"].Value(); got != int64(90_000_000) {
		t.Fatalf("mpris:length = %v, want microseconds", got)
	}
	if got := meta["mpris:trackid"].Value(); got != trackPath("/music/a.flac") {
		t.Fatalf("mpris:trackid = %v", got)
	}

	posVar, err := obj.GetProperty(ifacePlayer + ".Position")
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	if got := posVar.Value(); got != int64(3_000_000) {
		t.Fatalf("Position = %v, want 3000000", got)
	}

	identity, err := obj.GetProperty(ifaceRoot + ".Identity")
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if identity.Value() != appIdentity {
		t.Fatalf("Identity = %v, want %q", identity.Value(), appIdentity)
	}
}

// TestLiveMPRISMethodsReachTheCommands pins the inbound path: a method call from
// the bus lands on the matching command.
func TestLiveMPRISMethodsReachTheCommands(t *testing.T) {
	client := liveBus(t)
	_, rec := startLiveBackend(t)

	obj := client.Object(busName, rootPath)

	for _, m := range []struct {
		method string
		want   string
	}{
		{"Next", "next"},
		{"Previous", "previous"},
		{"PlayPause", "playpause"},
		{"Stop", "stop"},
	} {
		if call := obj.Call(ifacePlayer+"."+m.method, 0); call.Err != nil {
			t.Fatalf("%s: %v", m.method, call.Err)
		}
		rec.expect(t, m.want)
	}
}

// TestLiveMPRISSetPositionHonoursTheTrackId pins the spec's stale-seek guard
// against a live call: a position for the current track is applied, and one for
// an unknown track is ignored.
func TestLiveMPRISSetPositionHonoursTheTrackId(t *testing.T) {
	client := liveBus(t)
	b, rec := startLiveBackend(t)

	b.Update(TrackInfo{ID: "/music/a.flac", Title: "A Song", Playing: true})
	obj := client.Object(busName, rootPath)

	// A stale id is ignored.
	if call := obj.Call(ifacePlayer+".SetPosition", 0, trackPath("other"), int64(5_000_000)); call.Err != nil {
		t.Fatalf("SetPosition(stale): %v", call.Err)
	}
	select {
	case got := <-rec.calls:
		t.Fatalf("a stale SetPosition reached the engine: %q", got)
	case <-time.After(100 * time.Millisecond):
	}

	// The current id is applied.
	if call := obj.Call(ifacePlayer+".SetPosition", 0, trackPath("/music/a.flac"), int64(5_000_000)); call.Err != nil {
		t.Fatalf("SetPosition(current): %v", call.Err)
	}
	rec.expect(t, "seekto:5s")
}

// TestLiveMPRISEmitsPropertiesChanged pins the outbound signal: a client that
// subscribed to PropertiesChanged is told when the track changes, which is what
// keeps a desktop widget in step without polling.
func TestLiveMPRISEmitsPropertiesChanged(t *testing.T) {
	client := liveBus(t)
	b, _ := startLiveBackend(t)

	if err := client.AddMatchSignal(
		dbus.WithMatchObjectPath(rootPath),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		t.Fatalf("AddMatchSignal: %v", err)
	}
	signals := make(chan *dbus.Signal, 8)
	client.Signal(signals)
	// The match is installed asynchronously by the bus; give it a moment so the
	// first update is not lost to the race.
	time.Sleep(100 * time.Millisecond)

	b.Update(TrackInfo{ID: "/music/a.flac", Title: "First", Playing: true})
	b.Update(TrackInfo{ID: "/music/a.flac", Title: "Second", Playing: true})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case sig := <-signals:
			if len(sig.Body) < 2 {
				continue
			}
			changed, ok := sig.Body[1].(map[string]dbus.Variant)
			if !ok {
				continue
			}
			if metaVar, ok := changed["Metadata"]; ok {
				meta, _ := metaVar.Value().(map[string]dbus.Variant)
				if got := meta["xesam:title"].Value(); got == "Second" {
					return // the signal carried the new title
				}
			}
		case <-deadline:
			t.Fatal("no PropertiesChanged signal carrying the new title")
		}
	}
}

// TestLiveMPRISPropertyWriteReachesTheEngine pins the Set path end to end: a
// client writing Volume is forwarded to the command, and the value is stored.
func TestLiveMPRISPropertyWriteReachesTheEngine(t *testing.T) {
	client := liveBus(t)
	_, rec := startLiveBackend(t)

	obj := client.Object(busName, rootPath)
	if err := obj.SetProperty(ifacePlayer+".Volume", dbus.MakeVariant(0.4)); err != nil {
		t.Fatalf("Set Volume: %v", err)
	}
	rec.expect(t, "volume")

	got, err := obj.GetProperty(ifacePlayer + ".Volume")
	if err != nil {
		t.Fatalf("Get Volume: %v", err)
	}
	if got.Value() != 0.4 {
		t.Fatalf("Volume = %v, want 0.4", got.Value())
	}
}

// TestLiveMPRISLoopStatusRejectsAnInvalidValue pins the write guard: the engine
// has no repeat mode, so any LoopStatus write is refused rather than stored.
func TestLiveMPRISLoopStatusRejectsAnInvalidValue(t *testing.T) {
	client := liveBus(t)
	startLiveBackend(t)

	obj := client.Object(busName, rootPath)
	call := obj.Call("org.freedesktop.DBus.Properties.Set", 0,
		ifacePlayer, "LoopStatus", dbus.MakeVariant("Track"))
	if call.Err == nil {
		t.Fatal("Set LoopStatus succeeded, want an error while repeat is unsupported")
	}

	got, err := obj.GetProperty(ifacePlayer + ".LoopStatus")
	if err != nil {
		t.Fatalf("Get LoopStatus: %v", err)
	}
	if got.Value() != "None" {
		t.Fatalf("LoopStatus = %v, want the stored None", got.Value())
	}
}

// TestLiveMPRISClearDropsToStopped pins the empty-queue case: the status reads
// Stopped and the track id becomes the spec's NoTrack sentinel.
func TestLiveMPRISClearDropsToStopped(t *testing.T) {
	client := liveBus(t)
	b, _ := startLiveBackend(t)

	b.Update(TrackInfo{ID: "/music/a.flac", Title: "A Song", Playing: true})
	b.Clear()

	obj := client.Object(busName, rootPath)
	status, err := obj.GetProperty(ifacePlayer + ".PlaybackStatus")
	if err != nil {
		t.Fatalf("PlaybackStatus: %v", err)
	}
	if status.Value() != "Stopped" {
		t.Fatalf("PlaybackStatus = %v, want Stopped", status.Value())
	}

	metaVar, err := obj.GetProperty(ifacePlayer + ".Metadata")
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	meta := metaVar.Value().(map[string]dbus.Variant)
	if got := meta["mpris:trackid"].Value(); got != noTrack {
		t.Fatalf("mpris:trackid = %v, want NoTrack", got)
	}
}

// TestLiveMPRISIntrospectionListsBothInterfaces pins the introspection a client
// uses to discover what is offered: both required interfaces and the transport
// methods must be present.
func TestLiveMPRISIntrospectionListsBothInterfaces(t *testing.T) {
	client := liveBus(t)
	startLiveBackend(t)

	var xml string
	if err := client.Object(busName, rootPath).
		Call("org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&xml); err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	for _, want := range []string{ifaceRoot, ifacePlayer, "PlayPause", "SetPosition", "Seeked", "PropertiesChanged"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("introspection is missing %q:\n%s", want, xml)
		}
	}
}

// TestLiveMPRISStopReleasesTheName pins the teardown: after Stop the bus name is
// free again, so a restart can re-claim it.
func TestLiveMPRISStopReleasesTheName(t *testing.T) {
	client := liveBus(t)
	b, _ := startLiveBackend(t)

	b.Stop()

	// The name should no longer resolve to an owner.
	var owner string
	err := client.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, busName).Store(&owner)
	if err == nil {
		t.Fatalf("name still owned by %q after Stop", owner)
	}
}
