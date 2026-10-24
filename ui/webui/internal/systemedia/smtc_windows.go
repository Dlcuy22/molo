//go:build windows

// SMTC transport: molo as a media session the Windows shell can read and drive.
//
// The session is published through a MediaPlayer that plays nothing. Audio still
// goes through oto; the MediaPlayer exists only because
// SystemMediaTransportControls belongs to one. Its command manager is disabled
// so a shell button arrives as an event we handle instead of as a playback
// action inside WinRT.
//
// Hand-rolled on go-ole rather than a generated WinRT projection, so the
// integration adds no dependency of its own. Every IID and vtable slot below is
// copied from the Windows metadata (Windows.Media.winmd), and the order is what
// makes it correct: a wrong slot is a process crash, not an error.

package systemedia

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/dlcuy22/molo/ui/webui/internal/artcache"
	"github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// Interface ids, as the WinRT metadata declares them.
const (
	iidMediaPlayer2            = "3c841218-2123-4fc5-9082-2f883f77bdf5"
	iidMediaPlayer3            = "ee0660da-031b-4feb-bd9b-92e0a0a8d299"
	iidPlaybackCommandManager  = "5acee5a6-5cb6-4a5a-8521-cc86b1c1ed37"
	iidSMTC                    = "99fa3ff4-1742-42a6-902e-087d41f965ec"
	iidSMTC2                   = "ea98d2f6-7f3c-4af2-a586-72889808efb1"
	iidDisplayUpdater          = "8abbc53e-fa55-4ecf-ad8e-c984e5dd1550"
	iidMusicDisplayProperties  = "6bbf0c59-d0a0-4d26-92a0-f978e1d18e7b"
	iidMusicDisplayProperties2 = "00368462-97d3-44b9-b00f-008afcefaf18"
	iidTimelineProperties      = "5125316a-c3a2-475b-8507-93534dc88f15"
	iidStorageFileStatics      = "5984c710-daf2-43c8-8bb4-a4d3eacfd03f"
	iidStreamReferenceStatics  = "857309dc-3fbf-4e7d-986f-ef3b1a07a964"
	iidAsyncInfo               = "00000036-0000-0000-c000-000000000046"
)

// iidAsyncStorageFile is IAsyncOperation<StorageFile>, the type
// StorageFile.GetFileFromPathAsync returns. WinRT derives a parameterized IID
// per closed generic, so it is not a constant in any header.
const iidAsyncStorageFile = "5e52f8ce-aced-5a42-95b4-f674dd84885e"

// iidButtonPressedHandler is TypedEventHandler<SystemMediaTransportControls,
// SystemMediaTransportControlsButtonPressedEventArgs>. WinRT derives it by
// hashing the parameterized signature over the delegate's base GUID
// (9de1c534-6ae1-11e0-84e1-18a905bcc53f), so it is not in any header; this value
// is that hash.
const iidButtonPressedHandler = "0557e996-7b23-5bae-aa81-ea0d671143a4"

// MediaPlayerAudioCategoryMedia, so the shell groups the session with music
// rather than with alerts or communications.
const audioCategoryMedia int32 = 11

const (
	roInitMultithreaded = 1

	sOK          = 0
	sFalse       = 1
	eNoInterface = 0x80004002
	ePointer     = 0x80004003
)

// MediaPlayer2, the first version carrying the transport controls.
type iMediaPlayer2Vtbl struct {
	ole.IInspectableVtbl

	GetSystemMediaTransportControls uintptr
	GetAudioCategory                uintptr
	SetAudioCategory                uintptr
	GetAudioDeviceType              uintptr
	SetAudioDeviceType              uintptr
}

type iMediaPlayer2 struct{ ole.IInspectable }

func (v *iMediaPlayer2) VTable() *iMediaPlayer2Vtbl {
	return (*iMediaPlayer2Vtbl)(unsafe.Pointer(v.RawVTable))
}

// MediaPlayer3 carries the command manager, at slot 11.
type iMediaPlayer3Vtbl struct {
	ole.IInspectableVtbl

	AddIsMutedChanged                   uintptr
	RemoveIsMutedChanged                uintptr
	AddSourceChanged                    uintptr
	RemoveSourceChanged                 uintptr
	GetAudioBalance                     uintptr
	SetAudioBalance                     uintptr
	GetRealTimePlayback                 uintptr
	SetRealTimePlayback                 uintptr
	GetStereoscopicVideoRenderMode      uintptr
	SetStereoscopicVideoRenderMode      uintptr
	GetBreakManager                     uintptr
	GetCommandManager                   uintptr
	GetAudioDevice                      uintptr
	SetAudioDevice                      uintptr
	GetTimelineController               uintptr
	SetTimelineController               uintptr
	GetTimelineControllerPositionOffset uintptr
	SetTimelineControllerPositionOffset uintptr
	GetPlaybackSession                  uintptr
	StepForwardOneFrame                 uintptr
	StepBackwardOneFrame                uintptr
	GetAsCastingSource                  uintptr
}

type iMediaPlayer3 struct{ ole.IInspectable }

func (v *iMediaPlayer3) VTable() *iMediaPlayer3Vtbl {
	return (*iMediaPlayer3Vtbl)(unsafe.Pointer(v.RawVTable))
}

type iPlaybackCommandManagerVtbl struct {
	ole.IInspectableVtbl

	GetIsEnabled uintptr
	SetIsEnabled uintptr
}

type iPlaybackCommandManager struct{ ole.IInspectable }

func (v *iPlaybackCommandManager) VTable() *iPlaybackCommandManagerVtbl {
	return (*iPlaybackCommandManagerVtbl)(unsafe.Pointer(v.RawVTable))
}

// SystemMediaTransportControls, all 30 slots: the button event sits at 26, so
// the ones below it cannot be omitted.
type iSMTCVtbl struct {
	ole.IInspectableVtbl

	GetPlaybackStatus       uintptr
	SetPlaybackStatus       uintptr
	GetDisplayUpdater       uintptr
	GetSoundLevel           uintptr
	GetIsEnabled            uintptr
	SetIsEnabled            uintptr
	GetIsPlayEnabled        uintptr
	SetIsPlayEnabled        uintptr
	GetIsStopEnabled        uintptr
	SetIsStopEnabled        uintptr
	GetIsPauseEnabled       uintptr
	SetIsPauseEnabled       uintptr
	GetIsRecordEnabled      uintptr
	SetIsRecordEnabled      uintptr
	GetIsFastForwardEnabled uintptr
	SetIsFastForwardEnabled uintptr
	GetIsRewindEnabled      uintptr
	SetIsRewindEnabled      uintptr
	GetIsPreviousEnabled    uintptr
	SetIsPreviousEnabled    uintptr
	GetIsNextEnabled        uintptr
	SetIsNextEnabled        uintptr
	GetIsChannelUpEnabled   uintptr
	SetIsChannelUpEnabled   uintptr
	GetIsChannelDownEnabled uintptr
	SetIsChannelDownEnabled uintptr
	AddButtonPressed        uintptr
	RemoveButtonPressed     uintptr
	AddPropertyChanged      uintptr
	RemovePropertyChanged   uintptr
}

type iSMTC struct{ ole.IInspectable }

func (v *iSMTC) VTable() *iSMTCVtbl {
	return (*iSMTCVtbl)(unsafe.Pointer(v.RawVTable))
}

// SystemMediaTransportControls2 carries the timeline, at slot 6.
type iSMTC2Vtbl struct {
	ole.IInspectableVtbl

	GetAutoRepeatMode                     uintptr
	SetAutoRepeatMode                     uintptr
	GetShuffleEnabled                     uintptr
	SetShuffleEnabled                     uintptr
	GetPlaybackRate                       uintptr
	SetPlaybackRate                       uintptr
	UpdateTimelineProperties              uintptr
	AddPlaybackPositionChangeRequested    uintptr
	RemovePlaybackPositionChangeRequested uintptr
	AddPlaybackRateChangeRequested        uintptr
	RemovePlaybackRateChangeRequested     uintptr
	AddShuffleEnabledChangeRequested      uintptr
	RemoveShuffleEnabledChangeRequested   uintptr
	AddAutoRepeatModeChangeRequested      uintptr
	RemoveAutoRepeatModeChangeRequested   uintptr
}

type iSMTC2 struct{ ole.IInspectable }

func (v *iSMTC2) VTable() *iSMTC2Vtbl {
	return (*iSMTC2Vtbl)(unsafe.Pointer(v.RawVTable))
}

type iDisplayUpdaterVtbl struct {
	ole.IInspectableVtbl

	GetType            uintptr
	SetType            uintptr
	GetAppMediaId      uintptr
	SetAppMediaId      uintptr
	GetThumbnail       uintptr
	SetThumbnail       uintptr
	GetMusicProperties uintptr
	GetVideoProperties uintptr
	GetImageProperties uintptr
	CopyFromFileAsync  uintptr
	ClearAll           uintptr
	Update             uintptr
}

type iDisplayUpdater struct{ ole.IInspectable }

func (v *iDisplayUpdater) VTable() *iDisplayUpdaterVtbl {
	return (*iDisplayUpdaterVtbl)(unsafe.Pointer(v.RawVTable))
}

type iMusicDisplayPropertiesVtbl struct {
	ole.IInspectableVtbl

	GetTitle       uintptr
	SetTitle       uintptr
	GetAlbumArtist uintptr
	SetAlbumArtist uintptr
	GetArtist      uintptr
	SetArtist      uintptr
}

type iMusicDisplayProperties struct{ ole.IInspectable }

func (v *iMusicDisplayProperties) VTable() *iMusicDisplayPropertiesVtbl {
	return (*iMusicDisplayPropertiesVtbl)(unsafe.Pointer(v.RawVTable))
}

// The album title moved onto a second interface; the base one has no slot for
// it, so SetAlbumTitle has to be reached through this.
type iMusicDisplayProperties2Vtbl struct {
	ole.IInspectableVtbl

	GetAlbumTitle  uintptr
	SetAlbumTitle  uintptr
	GetTrackNumber uintptr
	SetTrackNumber uintptr
	GetGenres      uintptr
}

type iMusicDisplayProperties2 struct{ ole.IInspectable }

func (v *iMusicDisplayProperties2) VTable() *iMusicDisplayProperties2Vtbl {
	return (*iMusicDisplayProperties2Vtbl)(unsafe.Pointer(v.RawVTable))
}

type iTimelinePropertiesVtbl struct {
	ole.IInspectableVtbl

	GetStartTime   uintptr
	SetStartTime   uintptr
	GetEndTime     uintptr
	SetEndTime     uintptr
	GetMinSeekTime uintptr
	SetMinSeekTime uintptr
	GetMaxSeekTime uintptr
	SetMaxSeekTime uintptr
	GetPosition    uintptr
	SetPosition    uintptr
}

type iTimelineProperties struct{ ole.IInspectable }

func (v *iTimelineProperties) VTable() *iTimelinePropertiesVtbl {
	return (*iTimelinePropertiesVtbl)(unsafe.Pointer(v.RawVTable))
}

type iButtonPressedArgsVtbl struct {
	ole.IInspectableVtbl

	GetButton uintptr
}

type iButtonPressedArgs struct{ ole.IInspectable }

func (v *iButtonPressedArgs) VTable() *iButtonPressedArgsVtbl {
	return (*iButtonPressedArgsVtbl)(unsafe.Pointer(v.RawVTable))
}

// The StorageFile statics, for turning the art file into a StorageFile. A
// static method still receives the activation factory as `this`.
type iStorageFileStaticsVtbl struct {
	ole.IInspectableVtbl

	GetFileFromPathAsync           uintptr
	GetFileFromApplicationUriAsync uintptr
}

type iStorageFileStatics struct{ ole.IInspectable }

func (v *iStorageFileStatics) VTable() *iStorageFileStaticsVtbl {
	return (*iStorageFileStaticsVtbl)(unsafe.Pointer(v.RawVTable))
}

// IAsyncInfo and IAsyncOperation<T> are separate WinRT interfaces with separate
// vtables: a WinRT interface does not flatten the one it derives from, so each
// starts again with the IInspectable slots.
type iAsyncInfoVtbl struct {
	ole.IInspectableVtbl

	GetID        uintptr
	GetStatus    uintptr
	GetErrorCode uintptr
	Cancel       uintptr
	Close        uintptr
}

type iAsyncInfo struct{ ole.IInspectable }

func (v *iAsyncInfo) VTable() *iAsyncInfoVtbl {
	return (*iAsyncInfoVtbl)(unsafe.Pointer(v.RawVTable))
}

type iAsyncOperationVtbl struct {
	ole.IInspectableVtbl

	SetCompleted uintptr
	GetCompleted uintptr
	GetResults   uintptr
}

type iAsyncOperation struct{ ole.IInspectable }

func (v *iAsyncOperation) VTable() *iAsyncOperationVtbl {
	return (*iAsyncOperationVtbl)(unsafe.Pointer(v.RawVTable))
}

// The stream reference statics. CreateFromUri is unusable here: WinRT accepts
// only http, https, ms-appx and ms-appdata there, so a local cover has to go
// through CreateFromFile.
type iStreamReferenceStaticsVtbl struct {
	ole.IInspectableVtbl

	CreateFromFile   uintptr
	CreateFromUri    uintptr
	CreateFromStream uintptr
}

type iStreamReferenceStatics struct{ ole.IInspectable }

func (v *iStreamReferenceStatics) VTable() *iStreamReferenceStaticsVtbl {
	return (*iStreamReferenceStaticsVtbl)(unsafe.Pointer(v.RawVTable))
}

// ---- WinRT plumbing ----

var (
	modcombase = windows.NewLazySystemDLL("combase.dll")

	procWindowsCreateString = modcombase.NewProc("WindowsCreateString")
	procRoUninitialize      = modcombase.NewProc("RoUninitialize")
)

// newHString builds a WinRT string. go-ole's NewHString measures its input in
// runes, which truncates anything outside the BMP, so a title with an emoji
// would arrive cut in half; this one measures UTF-16 units.
func newHString(s string) (ole.HString, error) {
	units := utf16.Encode([]rune(s))
	var buf *uint16
	if len(units) > 0 {
		buf = &units[0]
	}
	var h ole.HString
	hr, _, _ := procWindowsCreateString.Call(
		uintptr(unsafe.Pointer(buf)),
		uintptr(len(units)),
		uintptr(unsafe.Pointer(&h)),
	)
	if hr != 0 {
		return 0, ole.NewError(hr)
	}

	return h, nil
}

// withHString runs fn with a temporary WinRT string and frees it afterwards.
func withHString(s string, fn func(ole.HString) error) error {
	h, err := newHString(s)
	if err != nil {
		return err
	}
	defer func() { _ = ole.DeleteHString(h) }()

	return fn(h)
}

// queryInterface asks an object for one of its interfaces. The result carries a
// reference the caller releases.
func queryInterface(obj *ole.IInspectable, iid string) (*ole.IInspectable, error) {
	disp, err := obj.QueryInterface(ole.NewGUID(iid))
	if err != nil {
		return nil, err
	}

	return (*ole.IInspectable)(unsafe.Pointer(disp)), nil
}

// roInitialize puts the calling thread in the multi-threaded apartment. It is
// called once per session goroutine, and the OS reuses threads, so S_FALSE (that
// thread is already in the apartment) is success, not a failure.
func roInitialize() error {
	err := ole.RoInitialize(roInitMultithreaded)
	if err == nil {
		return nil
	}
	var oleErr *ole.OleError
	if errors.As(err, &oleErr) && oleErr.Code() == sFalse {
		return nil
	}

	return err
}

// hresultErr turns a call result into an error, keeping the COM message.
func hresultErr(what string, hr uintptr) error {
	if hr == 0 {
		return nil
	}

	return fmt.Errorf("smtc: %s: %w", what, ole.NewError(hr))
}

// ---- the button delegate ----

// smtcHandler is the COM object Windows calls when a transport button is
// pressed. Its first field is the vtable pointer, so a pointer to it is a valid
// interface pointer; the callbacks cast `this` back to the handler.
//
// The object lives on the Go heap and the backend keeps the reference for as
// long as the subscription exists: Go's collector does not move heap objects, so
// the pointer Windows holds stays valid, and the field is what keeps it alive.
type smtcHandler struct {
	vtbl *smtcHandlerVtbl
	iid  ole.GUID

	mu   sync.Mutex
	refs int32

	onButton func(button int32)
}

type smtcHandlerVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	Invoke         uintptr
}

var (
	handlerQueryInterface = syscall.NewCallback(smtcQueryInterface)
	handlerAddRef         = syscall.NewCallback(smtcAddRef)
	handlerRelease        = syscall.NewCallback(smtcRelease)
	handlerInvoke         = syscall.NewCallback(smtcInvoke)
)

func newSMTCHandler(iid string, onButton func(int32)) *smtcHandler {
	h := &smtcHandler{
		iid:      *ole.NewGUID(iid),
		refs:     1,
		onButton: onButton,
	}
	h.vtbl = &smtcHandlerVtbl{
		QueryInterface: handlerQueryInterface,
		AddRef:         handlerAddRef,
		Release:        handlerRelease,
		Invoke:         handlerInvoke,
	}

	return h
}

// release drops the reference the backend holds. Windows' own reference is
// dropped by RemoveButtonPressed before this runs.
func (h *smtcHandler) release() {
	h.mu.Lock()
	h.refs--
	h.mu.Unlock()
}

func smtcQueryInterface(this unsafe.Pointer, iid *ole.GUID, out *unsafe.Pointer) uintptr {
	h := (*smtcHandler)(this)
	if out == nil {
		return ePointer
	}
	if !ole.IsEqualGUID(iid, &h.iid) &&
		!ole.IsEqualGUID(iid, ole.IID_IUnknown) &&
		!ole.IsEqualGUID(iid, ole.IID_IInspectable) {
		*out = nil

		return eNoInterface
	}
	*out = this
	smtcAddRef(this)

	return sOK
}

func smtcAddRef(this unsafe.Pointer) uintptr {
	h := (*smtcHandler)(this)
	h.mu.Lock()
	h.refs++
	refs := h.refs
	h.mu.Unlock()

	return uintptr(refs)
}

func smtcRelease(this unsafe.Pointer) uintptr {
	h := (*smtcHandler)(this)
	h.mu.Lock()
	if h.refs > 0 {
		h.refs--
	}
	refs := h.refs
	h.mu.Unlock()

	return uintptr(refs)
}

// smtcInvoke is the event itself. It runs on whatever thread the shell calls
// from, so it reads the button and hands it to the backend, which dispatches the
// command off this thread; anything slow here would stall the shell.
func smtcInvoke(this, sender, args unsafe.Pointer) uintptr {
	h := (*smtcHandler)(this)
	if args == nil || h.onButton == nil {
		return sOK
	}

	var button int32
	hr, _, _ := syscall.SyscallN((*iButtonPressedArgs)(args).VTable().GetButton,
		uintptr(args), uintptr(unsafe.Pointer(&button)))
	if hr != 0 {
		return sOK
	}
	h.onButton(button)

	return sOK
}

// ---- the session ----

// smtcSession owns every WinRT object of one published session. It is created,
// used and released on one goroutine, because RoInitialize is per-thread and a
// Go goroutine can otherwise be scheduled onto another thread between calls.
type smtcSession struct {
	log *slog.Logger
	cmd Commands

	player   *ole.IInspectable
	smtc     *iSMTC
	smtc2    *iSMTC2
	updater  *iDisplayUpdater
	music    *iMusicDisplayProperties
	music2   *iMusicDisplayProperties2
	timeline *iTimelineProperties
	handler  *smtcHandler
	token    uint64

	status   int32
	enabled  bool
	trackID  string
	duration time.Duration
	artKey   string
}

func newSMTCObjects(cmd Commands, log *slog.Logger) (*smtcSession, error) {
	player, err := ole.RoActivateInstance("Windows.Media.Playback.MediaPlayer")
	if err != nil {
		return nil, fmt.Errorf("smtc: activate MediaPlayer: %w", err)
	}

	s := &smtcSession{log: log, cmd: cmd, player: player, status: -1, enabled: true}
	if err := s.init(); err != nil {
		s.close()

		return nil, err
	}

	return s, nil
}

func (s *smtcSession) init() error {
	mp2Obj, err := queryInterface(s.player, iidMediaPlayer2)
	if err != nil {
		return fmt.Errorf("smtc: MediaPlayer2: %w", err)
	}
	defer mp2Obj.Release()
	mp2 := (*iMediaPlayer2)(unsafe.Pointer(mp2Obj))

	hr, _, _ := syscall.SyscallN(mp2.VTable().SetAudioCategory,
		uintptr(unsafe.Pointer(mp2)), uintptr(audioCategoryMedia))
	if err := hresultErr("set audio category", hr); err != nil {
		return err
	}

	// Left enabled, WinRT acts on play and pause itself and the button events
	// never reach us, because the MediaPlayer has no source of its own.
	mp3Obj, err := queryInterface(s.player, iidMediaPlayer3)
	if err != nil {
		return fmt.Errorf("smtc: MediaPlayer3: %w", err)
	}
	mp3 := (*iMediaPlayer3)(unsafe.Pointer(mp3Obj))
	var manager *iPlaybackCommandManager
	hr, _, _ = syscall.SyscallN(mp3.VTable().GetCommandManager,
		uintptr(unsafe.Pointer(mp3)), uintptr(unsafe.Pointer(&manager)))
	mp3Obj.Release()
	if err := hresultErr("get command manager", hr); err != nil {
		return err
	}
	if manager == nil {
		return errors.New("smtc: MediaPlayer has no command manager")
	}
	defer manager.Release()
	hr, _, _ = syscall.SyscallN(manager.VTable().SetIsEnabled,
		uintptr(unsafe.Pointer(manager)), 0)
	if err := hresultErr("disable command manager", hr); err != nil {
		return err
	}

	var smtcObj *ole.IInspectable
	hr, _, _ = syscall.SyscallN(mp2.VTable().GetSystemMediaTransportControls,
		uintptr(unsafe.Pointer(mp2)), uintptr(unsafe.Pointer(&smtcObj)))
	if err := hresultErr("get transport controls", hr); err != nil {
		return err
	}
	if smtcObj == nil {
		return errors.New("smtc: no transport controls")
	}
	smtcItf, err := queryInterface(smtcObj, iidSMTC)
	smtcObj.Release()
	if err != nil {
		return fmt.Errorf("smtc: ISystemMediaTransportControls: %w", err)
	}
	s.smtc = (*iSMTC)(unsafe.Pointer(smtcItf))

	if err := s.setEnabled(true); err != nil {
		return err
	}
	if err := s.setButtons(buttonsFor(s.cmd)); err != nil {
		return err
	}

	// Timeline and album title live on later interface versions. A missing one
	// only costs a progress bar or an album name, so neither is fatal.
	if obj, err := queryInterface(smtcItf, iidSMTC2); err == nil {
		s.smtc2 = (*iSMTC2)(unsafe.Pointer(obj))
	} else {
		s.log.Debug("smtc: timeline updates unavailable", "error", err)
	}

	var updaterObj *ole.IInspectable
	hr, _, _ = syscall.SyscallN(s.smtc.VTable().GetDisplayUpdater,
		uintptr(unsafe.Pointer(s.smtc)), uintptr(unsafe.Pointer(&updaterObj)))
	if err := hresultErr("get display updater", hr); err != nil {
		return err
	}
	updaterItf, err := queryInterface(updaterObj, iidDisplayUpdater)
	updaterObj.Release()
	if err != nil {
		return fmt.Errorf("smtc: IDisplayUpdater: %w", err)
	}
	s.updater = (*iDisplayUpdater)(unsafe.Pointer(updaterItf))

	hr, _, _ = syscall.SyscallN(s.updater.VTable().SetType,
		uintptr(unsafe.Pointer(s.updater)), uintptr(smtcTypeMusic))
	if err := hresultErr("set media type", hr); err != nil {
		return err
	}

	var musicObj *ole.IInspectable
	hr, _, _ = syscall.SyscallN(s.updater.VTable().GetMusicProperties,
		uintptr(unsafe.Pointer(s.updater)), uintptr(unsafe.Pointer(&musicObj)))
	if err := hresultErr("get music properties", hr); err != nil {
		return err
	}
	musicItf, err := queryInterface(musicObj, iidMusicDisplayProperties)
	musicObj.Release()
	if err != nil {
		return fmt.Errorf("smtc: IMusicDisplayProperties: %w", err)
	}
	s.music = (*iMusicDisplayProperties)(unsafe.Pointer(musicItf))

	if obj, err := queryInterface(musicItf, iidMusicDisplayProperties2); err == nil {
		s.music2 = (*iMusicDisplayProperties2)(unsafe.Pointer(obj))
	} else {
		s.log.Debug("smtc: album title unavailable", "error", err)
	}

	timelineObj, err := ole.RoActivateInstance("Windows.Media.SystemMediaTransportControlsTimelineProperties")
	if err != nil {
		return fmt.Errorf("smtc: activate timeline properties: %w", err)
	}
	timelineItf, err := queryInterface(timelineObj, iidTimelineProperties)
	timelineObj.Release()
	if err != nil {
		return fmt.Errorf("smtc: ITimelineProperties: %w", err)
	}
	s.timeline = (*iTimelineProperties)(unsafe.Pointer(timelineItf))

	s.handler = newSMTCHandler(iidButtonPressedHandler, s.onButton)
	var token eventRegistrationToken
	hr, _, _ = syscall.SyscallN(s.smtc.VTable().AddButtonPressed,
		uintptr(unsafe.Pointer(s.smtc)),
		uintptr(unsafe.Pointer(s.handler)),
		uintptr(unsafe.Pointer(&token)))
	if err := hresultErr("subscribe to button presses", hr); err != nil {
		return err
	}
	s.token = uint64(token.Value)

	return nil
}

// eventRegistrationToken is the 8-byte value AddButtonPressed hands back.
type eventRegistrationToken struct{ Value int64 }

func (s *smtcSession) setEnabled(on bool) error {
	hr, _, _ := syscall.SyscallN(s.smtc.VTable().SetIsEnabled,
		uintptr(unsafe.Pointer(s.smtc)), boolArg(on))

	return hresultErr("enable transport controls", hr)
}

func (s *smtcSession) setButtons(b smtcButtons) error {
	settings := []struct {
		slot uintptr
		on   bool
		name string
	}{
		{s.smtc.VTable().SetIsPlayEnabled, b.Play, "play"},
		{s.smtc.VTable().SetIsPauseEnabled, b.Pause, "pause"},
		{s.smtc.VTable().SetIsStopEnabled, b.Stop, "stop"},
		{s.smtc.VTable().SetIsNextEnabled, b.Next, "next"},
		{s.smtc.VTable().SetIsPreviousEnabled, b.Previous, "previous"},
	}
	for _, setting := range settings {
		hr, _, _ := syscall.SyscallN(setting.slot,
			uintptr(unsafe.Pointer(s.smtc)), boolArg(setting.on))
		if err := hresultErr("enable the "+setting.name+" button", hr); err != nil {
			return err
		}
	}

	return nil
}

// close tears the session down in the reverse order of init. Every step is best
// effort, because it also runs on a half-built session after a failed init.
func (s *smtcSession) close() {
	if s.smtc != nil && s.handler != nil {
		hr, _, _ := syscall.SyscallN(s.smtc.VTable().RemoveButtonPressed,
			uintptr(unsafe.Pointer(s.smtc)), uintptr(s.token))
		if hr != 0 {
			s.log.Debug("smtc: unsubscribe from button presses", "error", ole.NewError(hr))
		}
		s.handler.release()
	}
	// A released session should not linger in the shell as a stopped track.
	if s.updater != nil {
		hr, _, _ := syscall.SyscallN(s.updater.VTable().ClearAll, uintptr(unsafe.Pointer(s.updater)))
		if hr != 0 {
			s.log.Debug("smtc: clear the display on close", "error", ole.NewError(hr))
		}
	}

	for _, obj := range []interface{ Release() int32 }{
		s.timeline, s.music2, s.music, s.updater, s.smtc2, s.smtc, s.player,
	} {
		if obj != nil {
			obj.Release()
		}
	}
	s.timeline, s.music2, s.music, s.updater, s.smtc2, s.smtc, s.player = nil, nil, nil, nil, nil, nil, nil
}

// publish writes one track snapshot. Metadata is only rewritten when the track
// changes; the timeline is written on every call because the position moves.
func (s *smtcSession) publish(t TrackInfo) {
	if !s.enabled {
		// Clear disables the controls so the session leaves the flyout; the
		// next track is what brings it back.
		if err := s.setEnabled(true); err != nil {
			s.log.Debug("smtc: re-enable transport controls", "error", err)

			return
		}
		s.enabled = true
	}

	if status := smtcPlaybackStatus(t); status != s.status {
		hr, _, _ := syscall.SyscallN(s.smtc.VTable().SetPlaybackStatus,
			uintptr(unsafe.Pointer(s.smtc)), uintptr(status))
		if err := hresultErr("set playback status", hr); err != nil {
			s.log.Debug("smtc: publish status", "error", err)
		} else {
			s.status = status
		}
	}

	if t.ID != s.trackID {
		if err := s.setMetadata(t); err != nil {
			s.log.Debug("smtc: publish metadata", "error", err)
		} else {
			s.trackID = t.ID
		}
	}

	s.duration = t.Duration
	s.setTimeline(timelineFor(t.Duration, t.Position))
}

// setMetadata writes the tagged fields and the cover, then tells the shell to
// read them. The shell draws the last published values until Update runs, so
// the writes are batched behind one call.
func (s *smtcSession) setMetadata(t TrackInfo) error {
	setText := func(obj unsafe.Pointer, slot uintptr, value string) error {
		if value == "" {
			return nil
		}
		var hr uintptr

		return withHString(value, func(h ole.HString) error {
			hr, _, _ = syscall.SyscallN(slot, uintptr(obj), uintptr(h))

			return hresultErr("set text", hr)
		})
	}

	if err := setText(unsafe.Pointer(s.music), s.music.VTable().SetTitle, t.Title); err != nil {
		return err
	}
	if err := setText(unsafe.Pointer(s.music), s.music.VTable().SetArtist, t.Artist); err != nil {
		return err
	}
	if s.music2 != nil {
		if err := setText(unsafe.Pointer(s.music2), s.music2.VTable().SetAlbumTitle, t.Album); err != nil {
			return err
		}
	}
	if err := s.setThumbnail(t.Artwork, t.MIME); err != nil {
		return err
	}

	hr, _, _ := syscall.SyscallN(s.updater.VTable().Update, uintptr(unsafe.Pointer(s.updater)))

	return hresultErr("update display", hr)
}

// setThumbnail points the shell at the cover file. The bytes are written once
// per cover, keyed by content, and the reference is dropped straight away
// because SetThumbnail keeps its own.
func (s *smtcSession) setThumbnail(data []byte, mime string) error {
	if len(data) == 0 {
		if s.artKey == "" {
			return nil
		}
		// A new track without art must not keep showing the old cover.
		hr, _, _ := syscall.SyscallN(s.updater.VTable().SetThumbnail,
			uintptr(unsafe.Pointer(s.updater)), 0)
		s.artKey = ""

		return hresultErr("clear thumbnail", hr)
	}

	key := artcache.Key(data)
	if key == s.artKey {
		return nil
	}
	file, err := writeArtFile(key, mime, data)
	if err != nil {
		return err
	}
	ref, err := streamReferenceFromFile(file)
	if err != nil {
		return err
	}
	defer ref.Release()

	hr, _, _ := syscall.SyscallN(s.updater.VTable().SetThumbnail,
		uintptr(unsafe.Pointer(s.updater)), uintptr(unsafe.Pointer(ref)))
	if err := hresultErr("set thumbnail", hr); err != nil {
		return err
	}
	s.artKey = key

	return nil
}

// writeArtFile puts the cover where the shell can read it and returns its path.
// Art is decorative, so a failure is reported and the track publishes without a
// cover.
func writeArtFile(key, mime string, data []byte) (string, error) {
	dir, err := artcache.Dir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, artFileName(key, mime))
	if _, err := os.Stat(path); err != nil {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
	}

	return path, nil
}

// streamReferenceFromFile wraps a local file in the reference the display
// updater accepts. CreateFromUri is not an option: WinRT rejects file:// there
// and only takes http, https, ms-appx and ms-appdata, so the file is opened
// through StorageFile, which is an async call.
func streamReferenceFromFile(path string) (*ole.IInspectable, error) {
	file, err := storageFileFromPath(path)
	if err != nil {
		return nil, err
	}
	defer file.Release()

	factoryObj, err := ole.RoGetActivationFactory("Windows.Storage.Streams.RandomAccessStreamReference",
		ole.NewGUID(iidStreamReferenceStatics))
	if err != nil {
		return nil, fmt.Errorf("smtc: stream reference factory: %w", err)
	}
	statics := (*iStreamReferenceStatics)(unsafe.Pointer(factoryObj))
	defer factoryObj.Release()

	var ref *ole.IInspectable
	hr, _, _ := syscall.SyscallN(statics.VTable().CreateFromFile,
		uintptr(unsafe.Pointer(factoryObj)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(&ref)))
	if err := hresultErr("create stream reference", hr); err != nil {
		return nil, err
	}

	return ref, nil
}

// storageFileFromPath opens a local file as a StorageFile.
func storageFileFromPath(path string) (*ole.IInspectable, error) {
	factoryObj, err := ole.RoGetActivationFactory("Windows.Storage.StorageFile",
		ole.NewGUID(iidStorageFileStatics))
	if err != nil {
		return nil, fmt.Errorf("smtc: StorageFile factory: %w", err)
	}
	defer factoryObj.Release()
	statics := (*iStorageFileStatics)(unsafe.Pointer(factoryObj))

	var op *ole.IInspectable
	err = withHString(path, func(h ole.HString) error {
		var hr uintptr
		hr, _, _ = syscall.SyscallN(statics.VTable().GetFileFromPathAsync,
			uintptr(unsafe.Pointer(factoryObj)),
			uintptr(h),
			uintptr(unsafe.Pointer(&op)))

		return hresultErr("open art file", hr)
	})
	if err != nil {
		return nil, err
	}
	if op == nil {
		return nil, errors.New("smtc: StorageFile returned nothing")
	}

	return awaitAsync(op, iidAsyncStorageFile)
}

// awaitAsync waits for an IAsyncOperation and returns its result. The wait is a
// poll: these operations complete on their own, so no callback object is
// needed, and the poll is bounded so a stuck operation cannot hang a track
// change.
func awaitAsync(op *ole.IInspectable, iid string) (*ole.IInspectable, error) {
	infoObj, err := queryInterface(op, iidAsyncInfo)
	if err != nil {
		return nil, err
	}
	defer infoObj.Release()
	info := (*iAsyncInfo)(unsafe.Pointer(infoObj))

	const (
		statusCompleted = 1
		statusCanceled  = 2
		statusError     = 3
	)
	for i := 0; i < 500; i++ {
		var status int32
		hr, _, _ := syscall.SyscallN(info.VTable().GetStatus,
			uintptr(unsafe.Pointer(info)), uintptr(unsafe.Pointer(&status)))
		if err := hresultErr("poll async status", hr); err != nil {
			return nil, err
		}
		switch status {
		case statusCompleted:
			resultObj, err := queryInterface(op, iid)
			if err != nil {
				return nil, err
			}
			async := (*iAsyncOperation)(unsafe.Pointer(resultObj))
			var out *ole.IInspectable
			hr, _, _ := syscall.SyscallN(async.VTable().GetResults,
				uintptr(unsafe.Pointer(async)), uintptr(unsafe.Pointer(&out)))
			resultObj.Release()
			if err := hresultErr("async result", hr); err != nil {
				return nil, err
			}

			return out, nil
		case statusCanceled, statusError:
			var code int32
			syscall.SyscallN(info.VTable().GetErrorCode,
				uintptr(unsafe.Pointer(info)), uintptr(unsafe.Pointer(&code)))

			return nil, fmt.Errorf("smtc: async operation failed with %#x", code)
		}
		time.Sleep(time.Millisecond)
	}

	return nil, errors.New("smtc: async operation timed out")
}

// setTimeline writes the five values and publishes them in one call. Without
// SMTC2 the shell simply has no progress bar.
func (s *smtcSession) setTimeline(tl smtcTimeline) {
	if s.smtc2 == nil {
		return
	}
	values := []struct {
		slot  uintptr
		value int64
	}{
		{s.timeline.VTable().SetStartTime, tl.Start},
		{s.timeline.VTable().SetEndTime, tl.End},
		{s.timeline.VTable().SetMinSeekTime, tl.MinSeek},
		{s.timeline.VTable().SetMaxSeekTime, tl.MaxSeek},
		{s.timeline.VTable().SetPosition, tl.Position},
	}
	for _, v := range values {
		hr, _, _ := syscall.SyscallN(v.slot,
			uintptr(unsafe.Pointer(s.timeline)), uintptr(v.value))
		if err := hresultErr("set timeline", hr); err != nil {
			s.log.Debug("smtc: publish timeline", "error", err)

			return
		}
	}
	hr, _, _ := syscall.SyscallN(s.smtc2.VTable().UpdateTimelineProperties,
		uintptr(unsafe.Pointer(s.smtc2)), uintptr(unsafe.Pointer(s.timeline)))
	if err := hresultErr("update timeline", hr); err != nil {
		s.log.Debug("smtc: publish timeline", "error", err)
	}
}

// seek moves only the progress bar, for a seek the shell did not initiate.
func (s *smtcSession) seek(position time.Duration) {
	s.setTimeline(timelineFor(s.duration, position))
}

// clear drops the published track, which is what an emptied queue means.
// ClearAll only stages the removal; Update is what tells the shell to redraw.
func (s *smtcSession) clear() {
	hr, _, _ := syscall.SyscallN(s.updater.VTable().ClearAll, uintptr(unsafe.Pointer(s.updater)))
	if err := hresultErr("clear display", hr); err != nil {
		s.log.Debug("smtc: clear", "error", err)
	}
	hr, _, _ = syscall.SyscallN(s.updater.VTable().Update, uintptr(unsafe.Pointer(s.updater)))
	if err := hresultErr("update display", hr); err != nil {
		s.log.Debug("smtc: clear", "error", err)
	}
	hr, _, _ = syscall.SyscallN(s.smtc.VTable().SetPlaybackStatus,
		uintptr(unsafe.Pointer(s.smtc)), uintptr(smtcStatusStopped))
	if err := hresultErr("set playback status", hr); err != nil {
		s.log.Debug("smtc: clear", "error", err)
	}
	s.status = smtcStatusStopped
	s.trackID = ""
	s.duration = 0
	s.artKey = ""
	s.setTimeline(smtcTimeline{})

	// A session with nothing to play is disabled rather than left showing an
	// empty card, which is what "the queue is empty" looks like in the shell.
	if err := s.setEnabled(false); err != nil {
		s.log.Debug("smtc: disable transport controls", "error", err)
	} else {
		s.enabled = false
	}
}

// onButton runs a pressed shell button as a player command. It is called on the
// shell's thread, so the command is dispatched off it: the player's own update
// path publishes the result, and blocking here would stall the shell.
func (s *smtcSession) onButton(button int32) {
	action := buttonAction(button)
	var fn func()
	switch action {
	case smtcActionPlay:
		fn = s.cmd.Play
	case smtcActionPause:
		fn = s.cmd.Pause
	case smtcActionStop:
		fn = s.cmd.Stop
	case smtcActionNext:
		fn = s.cmd.Next
	case smtcActionPrevious:
		fn = s.cmd.Previous
	}
	if fn == nil {
		if action == smtcActionNone {
			s.log.Debug("smtc: ignoring button", "button", button)
		}

		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("smtc: command panicked", "panic", r)
			}
		}()
		fn()
	}()
}

func boolArg(v bool) uintptr {
	if v {
		return 1
	}

	return 0
}

// ---- the backend ----

// backend is the SMTC transport. It owns the session goroutine and the latest
// unpublished snapshot; every WinRT object belongs to that goroutine alone.
type backend struct {
	log *slog.Logger
	cmd Commands

	mu      sync.Mutex
	started bool
	pending smtcPending

	stop chan struct{}
	done chan struct{}
	wake chan struct{}
}

// smtcPending is what the owner goroutine has been asked to publish. Update,
// Clear and Seeked all land here, and the last write wins: each is a complete
// snapshot, so a coalesced one is never stale.
type smtcPending struct {
	track  TrackInfo
	has    bool
	clear  bool
	seek   time.Duration
	seeked bool
}

func newBackend(cmd Commands, log *slog.Logger) (Backend, error) {
	if log == nil {
		log = slog.Default()
	}

	return &backend{cmd: cmd, log: log}, nil
}

// Start creates the session on its own thread and waits for it to be live, so a
// machine that cannot publish is reported from Start rather than at the first
// track.
func (b *backend) Start() error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()

		return nil
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	b.stop, b.done, b.wake = stop, done, wake
	b.pending = smtcPending{}
	b.mu.Unlock()

	ready := make(chan error, 1)
	go b.run(ready, stop, done, wake)

	if err := <-ready; err != nil {
		b.mu.Lock()
		b.stop, b.done, b.wake = nil, nil, nil
		b.mu.Unlock()
		close(stop)
		<-done

		return err
	}

	b.mu.Lock()
	b.started = true
	b.mu.Unlock()

	return nil
}

// Stop releases the session and waits for its thread to finish. It is
// idempotent and safe after a failed Start.
func (b *backend) Stop() {
	b.mu.Lock()
	stop, done := b.stop, b.done
	b.stop, b.done, b.wake = nil, nil, nil
	b.started = false
	b.pending = smtcPending{}
	b.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// Update hands the current track to the session goroutine. It does not block:
// a 4 Hz snapshot pump must never wait on the shell.
func (b *backend) Update(t TrackInfo) {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()

		return
	}
	b.pending.track = t
	b.pending.has = true
	b.pending.clear = false
	b.mu.Unlock()
	b.notify()
}

// Clear removes the published track.
func (b *backend) Clear() {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()

		return
	}
	b.pending = smtcPending{clear: true}
	b.mu.Unlock()
	b.notify()
}

// Seeked moves the progress bar without waiting for the next snapshot.
func (b *backend) Seeked(position time.Duration) {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()

		return
	}
	b.pending.seek = position
	b.pending.seeked = true
	b.mu.Unlock()
	b.notify()
}

// notify wakes the owner goroutine. The channel holds one wake-up, which is
// what makes the mailbox coalescing: a burst of updates is one publish.
func (b *backend) notify() {
	b.mu.Lock()
	wake := b.wake
	b.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// take returns the pending work and resets the mailbox.
func (b *backend) take() smtcPending {
	b.mu.Lock()
	pending := b.pending
	b.pending = smtcPending{}
	b.mu.Unlock()

	return pending
}

// run owns the WinRT objects for the life of the session. LockOSThread is what
// makes that safe: RoInitialize is per-thread, and the objects must be created
// and released on the thread that initialized it.
func (b *backend) run(ready chan<- error, stop <-chan struct{}, done chan<- struct{}, wake <-chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(done)

	if err := roInitialize(); err != nil {
		ready <- fmt.Errorf("smtc: RoInitialize: %w", err)

		return
	}
	defer procRoUninitialize.Call()

	session, err := newSMTCObjects(b.cmd, b.log)
	if err != nil {
		ready <- err

		return
	}
	ready <- nil
	defer session.close()
	b.log.Info("smtc: serving")

	for {
		select {
		case <-stop:
			b.log.Info("smtc: stopped")

			return
		case <-wake:
			pending := b.take()
			if pending.clear {
				session.clear()
				continue
			}
			if pending.has {
				session.publish(pending.track)
			}
			if pending.seeked {
				session.seek(pending.seek)
			}
		}
	}
}
