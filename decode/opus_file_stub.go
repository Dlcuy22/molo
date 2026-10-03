//go:build !darwin && !linux && !netbsd

// libopusfile cannot be reached on this platform: purego exposes no dynamic
// loader on Windows, so the native path is unavailable there. The factory is
// still registered so the registry shape is identical everywhere, and it
// reports a clear error instead of disappearing.
package decode

import (
	"errors"
	"runtime"

	"github.com/dlcuy22/player/core"
)

var errLibopusfileUnsupported = errors.New("decode: libopusfile is not available on " + runtime.GOOS)

func loadLibopusfile() error { return errLibopusfileUnsupported }

type LibopusfileFactory struct{}

// NewLibopusfileFactory returns the stub used where the native library cannot
// be reached, so the registry shape is identical on every platform.
func NewLibopusfileFactory() *LibopusfileFactory { return &LibopusfileFactory{} }

func (f *LibopusfileFactory) Name() string { return "opus-libopusfile" }

// FriendlyName and Weight match the native build so the registry shape and the
// UI list are identical on every platform.
func (f *LibopusfileFactory) FriendlyName() string { return "Fastest" }

func (f *LibopusfileFactory) Weight() int { return 80 }

func (f *LibopusfileFactory) Exts() []string { return []string{".opus", ".ogg"} }

func (f *LibopusfileFactory) Match(magic []byte) bool {
	return len(magic) >= 4 && string(magic[:4]) == oggCapture
}

func (f *LibopusfileFactory) Open(path string) (Decoder, error) {
	return nil, errLibopusfileUnsupported
}

func (f *LibopusfileFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	// Mirror the native path's error shape so callers see the same StreamInfo
	// (known output format, unknown total) on every platform.
	return core.StreamInfo{
		Format:      core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		TotalFrames: -1,
	}, errLibopusfileUnsupported
}

func init() {
	Register(NewLibopusfileFactory())
}
