//go:build !linux

// The no-op transport for platforms without one yet.
//
// MPRIS is Linux-only and SMTC is Windows-only, so macOS, the mobile targets
// and the plain server build land here. It lets the Manager be wired
// unconditionally: a machine that cannot show system media controls reports the
// transport inactive rather than failing to build.
package systemedia

import (
	"log/slog"
	"time"
)

// backend is the inert transport. Every method is a no-op and Start reports
// that no transport exists for this platform.
type backend struct{}

func newBackend(_ Commands, _ *slog.Logger) (Backend, error) {
	return &backend{}, nil
}

func (b *backend) Start() error { return errUnsupported }

func (b *backend) Stop() {}

func (b *backend) Update(TrackInfo) {}

func (b *backend) Clear() {}

func (b *backend) Seeked(time.Duration) {}
