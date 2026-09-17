//go:build freebsd || android || ios

// oto cannot be built on this platform with cgo disabled, so the backend is
// replaced by one that fails loudly. It is still registered under the same
// name so the registry shape and the caller's code are identical everywhere;
// only the driver differs, exactly like decode's libopusfile stub.

package playback

import (
	"errors"
	"runtime"
	"time"

	"github.com/dlcuy22/player/core"
)

const otoBackendName = "oto"

// ErrAudioInit mirrors the real backend's sentinel so callers match on one
// error regardless of platform.
var ErrAudioInit = errors.New("playback/oto: audio init failed")

// otoUnsupportedError names the platform, because "no audio" on a build the
// developer did not expect is otherwise very hard to diagnose.
var otoUnsupportedError = errors.New("playback/oto: oto is not available on " + runtime.GOOS)

type otoDevice struct{}

var _ Device = (*otoDevice)(nil)

func newOtoDevice() Device { return &otoDevice{} }

func init() {
	Register(otoBackendName, newOtoDevice)
}

func (d *otoDevice) Open(core.FrameFormat, Provider) error {
	return errors.Join(ErrAudioInit, otoUnsupportedError)
}

func (d *otoDevice) Start() error           { return errors.Join(ErrAudioInit, otoUnsupportedError) }
func (d *otoDevice) Pause() error           { return errors.Join(ErrAudioInit, otoUnsupportedError) }
func (d *otoDevice) Resume() error          { return errors.Join(ErrAudioInit, otoUnsupportedError) }
func (d *otoDevice) Flush() error           { return errors.Join(ErrAudioInit, otoUnsupportedError) }
func (d *otoDevice) Close() error           { return nil }
func (d *otoDevice) Latency() time.Duration { return 0 }
func (d *otoDevice) Err() error             { return errors.Join(ErrAudioInit, otoUnsupportedError) }
