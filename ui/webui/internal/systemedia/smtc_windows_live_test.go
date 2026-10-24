//go:build windows

package systemedia

// The live test reads the published session back through the same client API
// the Windows shell uses (Windows.Media.Control), so it proves the outbound
// direction end to end instead of trusting our own objects. It touches the real
// media flyout, so it only runs when MOLO_SMTC_LIVE is set:
//
//	MOLO_SMTC_LIVE=1 go test ./internal/systemedia -run TestLiveSMTC -v

import (
	"bytes"

	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"runtime"

	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
)

// The client side of the shell API, with only the slots the assertions read.
const (
	iidSessionManagerStatics = "2050c4ee-11a0-57de-aed7-c97c70338245"
	iidSessionManager        = "cace8eac-e86e-504a-ab31-5ff8ff1bce49"
	iidSession               = "7148c835-9b14-5ae2-ab85-dc9b1c14e1a8"
	iidSessionMediaProps     = "68856cf6-adb4-54b2-ac16-05837907acb6"
	iidSessionPlaybackInfo   = "94b4b6cf-e8ba-51ad-87a7-c10ade106127"
	iidAsyncSessionManager   = "3eec115e-7346-5c27-8c5f-da78514a277b"
	iidAsyncSessionProps     = "b185e6f3-e0d8-51cb-913f-c98d48c93c46"
)

// The client reads GlobalSystemMediaTransportControlsSessionPlaybackStatus,
// which is a different enum from the MediaPlaybackStatus a publisher writes.
const (
	clientStatusClosed   int32 = 0
	clientStatusOpened   int32 = 1
	clientStatusChanging int32 = 2
	clientStatusStopped  int32 = 3
	clientStatusPlaying  int32 = 4
	clientStatusPaused   int32 = 5
)

type iSessionManagerStaticsVtbl struct {
	ole.IInspectableVtbl

	RequestAsync uintptr
}

type iSessionManagerStatics struct{ ole.IInspectable }

func (v *iSessionManagerStatics) VTable() *iSessionManagerStaticsVtbl {
	return (*iSessionManagerStaticsVtbl)(unsafe.Pointer(v.RawVTable))
}

type iSessionManagerVtbl struct {
	ole.IInspectableVtbl

	GetCurrentSession uintptr
	GetSessions       uintptr
}

type iSessionManager struct{ ole.IInspectable }

func (v *iSessionManager) VTable() *iSessionManagerVtbl {
	return (*iSessionManagerVtbl)(unsafe.Pointer(v.RawVTable))
}

type iSessionVectorVtbl struct {
	ole.IInspectableVtbl

	GetAt   uintptr
	GetSize uintptr
	IndexOf uintptr
	GetMany uintptr
}

type iSessionVector struct{ ole.IInspectable }

func (v *iSessionVector) VTable() *iSessionVectorVtbl {
	return (*iSessionVectorVtbl)(unsafe.Pointer(v.RawVTable))
}

type iClientSessionVtbl struct {
	ole.IInspectableVtbl

	GetSourceAppUserModelID    uintptr
	TryGetMediaPropertiesAsync uintptr
	GetTimelineProperties      uintptr
	GetPlaybackInfo            uintptr
	TryPlayAsync               uintptr
	TryPauseAsync              uintptr
	TryStopAsync               uintptr
	TryRecordAsync             uintptr
	TryFastForwardAsync        uintptr
	TryRewindAsync             uintptr
	TrySkipNextAsync           uintptr
	TrySkipPreviousAsync       uintptr
}

type iClientSession struct{ ole.IInspectable }

func (v *iClientSession) VTable() *iClientSessionVtbl {
	return (*iClientSessionVtbl)(unsafe.Pointer(v.RawVTable))
}

type iClientPlaybackInfoVtbl struct {
	ole.IInspectableVtbl

	GetControls       uintptr
	GetPlaybackStatus uintptr
}

type iClientPlaybackInfo struct{ ole.IInspectable }

func (v *iClientPlaybackInfo) VTable() *iClientPlaybackInfoVtbl {
	return (*iClientPlaybackInfoVtbl)(unsafe.Pointer(v.RawVTable))
}

// The client reads GlobalSystemMediaTransportControlsSessionTimelineProperties,
// a read-only view with its own slot order, not the publisher's timeline object.
type iClientTimelineVtbl struct {
	ole.IInspectableVtbl

	GetStartTime     uintptr
	GetEndTime       uintptr
	GetMinSeekTime   uintptr
	GetMaxSeekTime   uintptr
	GetPosition      uintptr
	GetLastUpdatedAt uintptr
}

type iClientTimeline struct{ ole.IInspectable }

func (v *iClientTimeline) VTable() *iClientTimelineVtbl {
	return (*iClientTimelineVtbl)(unsafe.Pointer(v.RawVTable))
}

type iClientMediaPropsVtbl struct {
	ole.IInspectableVtbl

	GetTitle           uintptr
	GetSubtitle        uintptr
	GetAlbumArtist     uintptr
	GetArtist          uintptr
	GetAlbumTitle      uintptr
	GetTrackNumber     uintptr
	GetGenres          uintptr
	GetAlbumTrackCount uintptr
	GetPlaybackType    uintptr
	GetThumbnail       uintptr
}

type iClientMediaProps struct{ ole.IInspectable }

func (v *iClientMediaProps) VTable() *iClientMediaPropsVtbl {
	return (*iClientMediaPropsVtbl)(unsafe.Pointer(v.RawVTable))
}

// sessionView is what the client read back for one session.
type sessionView struct {
	AppID     string
	Status    int32
	Title     string
	Artist    string
	Album     string
	Thumbnail bool
	Position  int64
	End       int64
}

// readSessions asks the shell API for every session. WinRT is per-thread, so the
// reads run on a pinned goroutine of their own.
func readSessions(t *testing.T) ([]sessionView, error) {
	t.Helper()

	var (
		sessions []sessionView
		err      error
	)
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		if initErr := roInitialize(); initErr != nil {
			err = initErr

			return
		}
		sessions, err = readSessionsLocked()
	}()
	<-done

	return sessions, err
}

func readSessionsLocked() ([]sessionView, error) {
	factoryObj, err := ole.RoGetActivationFactory(
		"Windows.Media.Control.GlobalSystemMediaTransportControlsSessionManager",
		ole.NewGUID(iidSessionManagerStatics))
	if err != nil {
		return nil, err
	}
	defer factoryObj.Release()
	statics := (*iSessionManagerStatics)(unsafe.Pointer(factoryObj))

	var op *ole.IInspectable
	hr, _, _ := syscall.SyscallN(statics.VTable().RequestAsync,
		uintptr(unsafe.Pointer(factoryObj)), uintptr(unsafe.Pointer(&op)))
	if err := hresultErr("client RequestAsync", hr); err != nil {
		return nil, err
	}
	managerObj, err := awaitAsync(op, iidAsyncSessionManager)
	op.Release()
	if err != nil {
		return nil, err
	}
	defer managerObj.Release()

	managerItf, err := queryInterface(managerObj, iidSessionManager)
	if err != nil {
		return nil, err
	}
	defer managerItf.Release()
	manager := (*iSessionManager)(unsafe.Pointer(managerItf))

	var listObj *ole.IInspectable
	hr, _, _ = syscall.SyscallN(manager.VTable().GetSessions,
		uintptr(unsafe.Pointer(manager)), uintptr(unsafe.Pointer(&listObj)))
	if err := hresultErr("client GetSessions", hr); err != nil {
		return nil, err
	}
	defer listObj.Release()
	list := (*iSessionVector)(unsafe.Pointer(listObj))

	var size uint32
	hr, _, _ = syscall.SyscallN(list.VTable().GetSize,
		uintptr(unsafe.Pointer(list)), uintptr(unsafe.Pointer(&size)))
	if err := hresultErr("client GetSize", hr); err != nil {
		return nil, err
	}

	views := make([]sessionView, 0, size)
	for i := uint32(0); i < size; i++ {
		var sessionObj *ole.IInspectable
		hr, _, _ = syscall.SyscallN(list.VTable().GetAt,
			uintptr(unsafe.Pointer(list)), uintptr(i), uintptr(unsafe.Pointer(&sessionObj)))
		if hr != 0 {
			continue
		}
		view, err := readSession(sessionObj)
		sessionObj.Release()
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}

	return views, nil
}

func readSession(sessionObj *ole.IInspectable) (sessionView, error) {
	var view sessionView

	itf, err := queryInterface(sessionObj, iidSession)
	if err != nil {
		return view, err
	}
	defer itf.Release()
	session := (*iClientSession)(unsafe.Pointer(itf))
	view.AppID = clientString(itf, session.VTable().GetSourceAppUserModelID)

	var infoObj *ole.IInspectable
	hr, _, _ := syscall.SyscallN(session.VTable().GetPlaybackInfo,
		uintptr(unsafe.Pointer(session)), uintptr(unsafe.Pointer(&infoObj)))
	if hr == 0 && infoObj != nil {
		info := (*iClientPlaybackInfo)(unsafe.Pointer(infoObj))
		hr, _, _ = syscall.SyscallN(info.VTable().GetPlaybackStatus,
			uintptr(unsafe.Pointer(info)), uintptr(unsafe.Pointer(&view.Status)))
		if hr != 0 {
			view.Status = -1
		}
		infoObj.Release()
	}

	var timelineObj *ole.IInspectable
	hr, _, _ = syscall.SyscallN(session.VTable().GetTimelineProperties,
		uintptr(unsafe.Pointer(session)), uintptr(unsafe.Pointer(&timelineObj)))
	if hr == 0 && timelineObj != nil {
		timeline := (*iClientTimeline)(unsafe.Pointer(timelineObj))
		syscall.SyscallN(timeline.VTable().GetPosition,
			uintptr(unsafe.Pointer(timeline)), uintptr(unsafe.Pointer(&view.Position)))
		syscall.SyscallN(timeline.VTable().GetEndTime,
			uintptr(unsafe.Pointer(timeline)), uintptr(unsafe.Pointer(&view.End)))
		timelineObj.Release()
	}

	var propsOp *ole.IInspectable
	hr, _, _ = syscall.SyscallN(session.VTable().TryGetMediaPropertiesAsync,
		uintptr(unsafe.Pointer(session)), uintptr(unsafe.Pointer(&propsOp)))
	if hr != 0 || propsOp == nil {
		return view, nil
	}
	propsObj, err := awaitAsync(propsOp, iidAsyncSessionProps)
	propsOp.Release()
	if err != nil {
		return view, err
	}
	defer propsObj.Release()

	propsItf, err := queryInterface(propsObj, iidSessionMediaProps)
	if err != nil {
		return view, err
	}
	defer propsItf.Release()
	props := (*iClientMediaProps)(unsafe.Pointer(propsItf))
	view.Title = clientString(propsItf, props.VTable().GetTitle)
	view.Artist = clientString(propsItf, props.VTable().GetArtist)
	view.Album = clientString(propsItf, props.VTable().GetAlbumTitle)

	var thumb *ole.IInspectable
	hr, _, _ = syscall.SyscallN(props.VTable().GetThumbnail,
		uintptr(unsafe.Pointer(props)), uintptr(unsafe.Pointer(&thumb)))
	if hr == 0 && thumb != nil {
		view.Thumbnail = true
		thumb.Release()
	}

	return view, nil
}

// clientString reads one string property and frees the HSTRING.
func clientString(obj *ole.IInspectable, slot uintptr) string {
	var h ole.HString
	hr, _, _ := syscall.SyscallN(slot, uintptr(unsafe.Pointer(obj)), uintptr(unsafe.Pointer(&h)))
	if hr != 0 || h == 0 {
		return ""
	}
	defer func() { _ = ole.DeleteHString(h) }()

	return h.String()
}

// findSession returns the view for one app id.
func findSession(views []sessionView, appID string) (sessionView, bool) {
	for _, view := range views {
		if view.AppID == appID {
			return view, true
		}
	}

	return sessionView{}, false
}

// pressButton asks the shell API to press one transport button on our session,
// which is the same path the flyout and the media keys take.
func pressButton(t *testing.T, appID string, slot uintptr) {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- func() error {
			if err := roInitialize(); err != nil {
				return err
			}
			session, err := findSessionObject(appID)
			if err != nil {
				return err
			}
			defer session.Release()

			var op *ole.IInspectable
			hr, _, _ := syscall.SyscallN(slot,
				uintptr(unsafe.Pointer(session)), uintptr(unsafe.Pointer(&op)))
			if err := hresultErr("press button", hr); err != nil {
				return err
			}
			if op != nil {
				defer op.Release()
			}

			return nil
		}()
	}()
	if err := <-done; err != nil {
		t.Fatalf("pressButton: %v", err)
	}
}

// findSessionObject returns our own session from the shell's list.
func findSessionObject(appID string) (*ole.IInspectable, error) {
	factoryObj, err := ole.RoGetActivationFactory(
		"Windows.Media.Control.GlobalSystemMediaTransportControlsSessionManager",
		ole.NewGUID(iidSessionManagerStatics))
	if err != nil {
		return nil, err
	}
	defer factoryObj.Release()
	statics := (*iSessionManagerStatics)(unsafe.Pointer(factoryObj))

	var op *ole.IInspectable
	hr, _, _ := syscall.SyscallN(statics.VTable().RequestAsync,
		uintptr(unsafe.Pointer(factoryObj)), uintptr(unsafe.Pointer(&op)))
	if err := hresultErr("RequestAsync", hr); err != nil {
		return nil, err
	}
	managerObj, err := awaitAsync(op, iidAsyncSessionManager)
	op.Release()
	if err != nil {
		return nil, err
	}
	defer managerObj.Release()

	managerItf, err := queryInterface(managerObj, iidSessionManager)
	if err != nil {
		return nil, err
	}
	defer managerItf.Release()
	manager := (*iSessionManager)(unsafe.Pointer(managerItf))

	var listObj *ole.IInspectable
	hr, _, _ = syscall.SyscallN(manager.VTable().GetSessions,
		uintptr(unsafe.Pointer(manager)), uintptr(unsafe.Pointer(&listObj)))
	if err := hresultErr("GetSessions", hr); err != nil {
		return nil, err
	}
	defer listObj.Release()
	list := (*iSessionVector)(unsafe.Pointer(listObj))

	var size uint32
	hr, _, _ = syscall.SyscallN(list.VTable().GetSize,
		uintptr(unsafe.Pointer(list)), uintptr(unsafe.Pointer(&size)))
	if err := hresultErr("GetSize", hr); err != nil {
		return nil, err
	}

	for i := uint32(0); i < size; i++ {
		var sessionObj *ole.IInspectable
		hr, _, _ = syscall.SyscallN(list.VTable().GetAt,
			uintptr(unsafe.Pointer(list)), uintptr(i), uintptr(unsafe.Pointer(&sessionObj)))
		if hr != 0 {
			continue
		}
		itf, err := queryInterface(sessionObj, iidSession)
		if err != nil {
			sessionObj.Release()

			continue
		}
		id := clientString(itf, (*iClientSession)(unsafe.Pointer(itf)).VTable().GetSourceAppUserModelID)
		itf.Release()
		if id != appID {
			sessionObj.Release()

			continue
		}

		return sessionObj, nil
	}

	return nil, fmt.Errorf("session %q not found", appID)
}

// TestLiveSMTC publishes through the backend and reads the result back through
// the shell's client API.
func TestLiveSMTC(t *testing.T) {
	pressed := make(chan string, 4)

	if os.Getenv("MOLO_SMTC_LIVE") == "" {
		t.Skip("set MOLO_SMTC_LIVE=1 to publish a real media session")
	}

	// Each command publishes its result, as the player's snapshot pump would.
	// The shell only offers the button that matches the current status, so the
	// state has to move for the next press to be accepted.
	var b *backend
	publish := func(status string) {
		track := TrackInfo{ID: "/music/probe.flac", Title: "Buttons", Duration: 90 * time.Second}
		switch status {
		case "playing":
			track.Playing = true
		case "paused":
			track.Paused = true
		default:
			track.Stopped = true
		}
		b.Update(track)
	}

	transport, err := newBackend(Commands{
		Play: func() {
			pressed <- "play"
			publish("playing")
		},
		Pause: func() {
			pressed <- "pause"
			publish("paused")
		},
		Stop: func() {
			pressed <- "stop"
			publish("stopped")
		},
		Next: func() {
			pressed <- "next"
			publish("playing")
		},
		Previous: func() {
			pressed <- "previous"
			publish("playing")
		},
	}, slog.Default())
	if err != nil {
		t.Fatalf("newBackend: %v", err)
	}
	b = transport.(*backend)
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	appID := os.Args[0]
	if i := bytes.LastIndexByte([]byte(appID), '\\'); i >= 0 {
		appID = appID[i+1:]
	}

	waitFor := func(what string, check func(sessionView) bool) sessionView {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		var last sessionView
		for time.Now().Before(deadline) {
			views, err := readSessions(t)
			if err != nil {
				t.Fatalf("readSessions: %v", err)
			}
			if view, ok := findSession(views, appID); ok {
				last = view
				if check(view) {
					return view
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("session never satisfied %q; last read %+v", what, last)

		return last
	}

	// A title with an emoji: it is one rune but two UTF-16 units, which is what
	// the HSTRING length has to count.
	const title = "Probe \U0001F3B5 Title"
	art := testArtwork(t)

	b.Update(TrackInfo{
		ID:       "/music/probe.flac",
		Title:    title,
		Artist:   "Probe Artist",
		Album:    "Probe Album",
		Artwork:  art,
		MIME:     "image/png",
		Duration: 90 * time.Second,
		Position: 3 * time.Second,
		Playing:  true,
	})

	view := waitFor("the published track", func(v sessionView) bool {
		return v.Title == title && v.Thumbnail && v.Position == 30_000_000 && v.End == 900_000_000
	})
	if view.Status != clientStatusPlaying {
		t.Fatalf("status = %d, want playing", view.Status)
	}
	if view.Artist != "Probe Artist" || view.Album != "Probe Album" {
		t.Fatalf("artist/album = %q/%q", view.Artist, view.Album)
	}
	if view.Position != 30_000_000 || view.End != 900_000_000 {
		t.Fatalf("timeline = %d/%d ticks, want 30s of 90s", view.Position, view.End)
	}

	// A seek moves the progress bar without a new snapshot.
	b.Seeked(10 * time.Second)
	waitFor("the seek", func(v sessionView) bool { return v.Position == 100_000_000 })

	// Pausing is reflected in the status.
	b.Update(TrackInfo{
		ID: "/music/probe.flac", Title: title, Paused: true,
		Duration: 90 * time.Second, Position: 10 * time.Second,
	})
	waitFor("the paused status", func(v sessionView) bool { return v.Status == clientStatusPaused })

	// A button pressed in the shell reaches the command. This is the inbound
	// half: the client call goes through the same path the flyout uses, and the
	// event arrives on Windows' thread, which is what the delegate exists for.
	session, err := findSessionObject(appID)
	if err != nil {
		t.Fatalf("findSessionObject: %v", err)
	}
	sessionItf, err := queryInterface(session, iidSession)
	session.Release()
	if err != nil {
		t.Fatalf("session interface: %v", err)
	}
	clientSession := (*iClientSession)(unsafe.Pointer(sessionItf))
	buttons := []struct {
		name string
		slot uintptr
		want string
	}{
		{"play", clientSession.VTable().TryPlayAsync, "play"},
		{"pause", clientSession.VTable().TryPauseAsync, "pause"},
		{"next", clientSession.VTable().TrySkipNextAsync, "next"},
		{"previous", clientSession.VTable().TrySkipPreviousAsync, "previous"},
		{"stop", clientSession.VTable().TryStopAsync, "stop"},
	}
	clientSession.Release()
	for _, button := range buttons {
		pressButton(t, appID, button.slot)
		select {
		case got := <-pressed:
			if got != button.want {
				t.Fatalf("button %s reached %q, want %q", button.name, got, button.want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("button %s never reached a command", button.name)
		}
	}

	// Clearing drops the track and disables the session, which takes it out of
	// the flyout: an idle player has nothing to show.
	b.Clear()
	deadline := time.Now().Add(5 * time.Second)
	for {
		views, err := readSessions(t)
		if err != nil {
			t.Fatalf("readSessions: %v", err)
		}
		if _, ok := findSession(views, appID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session still listed after Clear")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// And the next track brings it back.
	b.Update(TrackInfo{ID: "/music/back.flac", Title: "Back", Playing: true, Duration: time.Minute})
	waitFor("the session to come back", func(v sessionView) bool {
		return v.Title == "Back" && v.Status == clientStatusPlaying
	})

	// Stopping the transport removes the session altogether.
	b.Stop()
	deadline = time.Now().Add(5 * time.Second)
	for {
		views, err := readSessions(t)
		if err != nil {
			t.Fatalf("readSessions: %v", err)
		}
		if _, ok := findSession(views, appID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session still listed after Stop")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// And it can be created again.
	if err := b.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	b.Update(TrackInfo{ID: "/music/two.flac", Title: "Second", Playing: true, Duration: time.Minute})
	waitFor("the restarted session", func(v sessionView) bool { return v.Title == "Second" })
}

// testArtwork is a real PNG, because the shell decodes the file rather than
// trusting the declared MIME.
func testArtwork(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}
