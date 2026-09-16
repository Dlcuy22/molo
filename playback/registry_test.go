package playback

import (
	"errors"
	"io"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
)

// stubDevice is a second backend that exists only in this test. It proves a
// new backend is one file plus one Register call and nothing else.
type stubDevice struct {
	opened  core.FrameFormat
	started int
	paused  int
	resumed int
	closed  int
	err     error
}

func (d *stubDevice) Open(f core.FrameFormat, _ Provider) error {
	d.opened = f

	return nil
}
func (d *stubDevice) Start() error  { d.started++; return nil }
func (d *stubDevice) Pause() error  { d.paused++; return nil }
func (d *stubDevice) Resume() error { d.resumed++; return nil }
func (d *stubDevice) Close() error  { d.closed++; return nil }
func (d *stubDevice) Err() error    { return d.err }
func (d *stubDevice) Latency() time.Duration {
	return 7 * time.Millisecond
}

func init() {
	Register("stub", func() Device { return &stubDevice{} })
}

func TestRegistryOpensRegisteredBackendByName(t *testing.T) {
	d, err := Open("stub")
	if err != nil {
		t.Fatalf("Open(\"stub\"): %v", err)
	}
	if _, ok := d.(*stubDevice); !ok {
		t.Fatalf("Open(\"stub\") returned %T", d)
	}
}

func TestRegistryHandsOutAFreshDevicePerOpen(t *testing.T) {
	a, err := Open("stub")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := Open("stub")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if a == b {
		t.Fatal("Open returned the same Device instance twice; the factory must build one per call")
	}
}

func TestRegistryUnknownBackend(t *testing.T) {
	if _, err := Open("does-not-exist"); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("Open error = %v, want ErrUnknownBackend", err)
	}
}

func TestRegistryNamesIsSortedAndStable(t *testing.T) {
	names := Names()
	if !reflect.DeepEqual(names, Names()) {
		t.Fatalf("Names() is not stable: %v then %v", names, Names())
	}
	if !slices.IsSorted(names) {
		t.Fatalf("Names() = %v, want sorted", names)
	}
	if !slices.Contains(names, "stub") {
		t.Fatalf("Names() = %v, missing the registered stub backend", names)
	}
}

func TestRegisterPanicsOnDuplicateName(t *testing.T) {
	// A silently shadowed backend is a bug that only shows up as the wrong
	// audio device in production, so registration rejects it immediately.
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on a duplicate name")
		}
	}()
	Register("stub", func() Device { return &stubDevice{} })
}

func TestRegisterRejectsEmptyName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on an empty name")
		}
	}()
	Register("", func() Device { return &stubDevice{} })
}

func TestOpenDevicesAreIndependent(t *testing.T) {
	a, _ := Open("stub")
	b, _ := Open("stub")

	if err := a.Open(core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32}, providerFunc(func([]float32) (int, error) { return 0, io.EOF })); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Pause(); err != nil {
		t.Fatalf("Pause on the second device: %v", err)
	}
	if got := a.(*stubDevice).paused; got != 0 {
		t.Fatalf("first device saw %d pauses after the second was paused", got)
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if _, err := Open("stub"); err != nil {
					t.Errorf("Open: %v", err)
				}
				_ = Names()
			}
		}()
	}
	wg.Wait()
}
