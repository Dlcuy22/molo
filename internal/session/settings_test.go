package session

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
	"github.com/dlcuy22/player/dsp"
	"github.com/dlcuy22/player/playback"
)

// namedDecoder records which codec name opened it, so a test can prove that a
// preference change actually reaches the decoder that builds the next track.
type namedDecoder struct {
	toneDecoder
	codec string
}

func (d *namedDecoder) DecoderName() string { return d.codec }
func (d *namedDecoder) ParserName() string  { return d.codec + "-parser" }

// openerLog records the decoder names an opener was asked for. The opener runs
// on a build worker while the test reads from its own goroutine, so the append
// and the read need a lock; without it the race detector flags the test, not the
// engine.
type openerLog struct {
	mu      sync.Mutex
	names   []string
	namesCh chan struct{}
}

func newOpenerLog() *openerLog {
	return &openerLog{namesCh: make(chan struct{}, 16)}
}

func (l *openerLog) add(name string) {
	l.mu.Lock()
	l.names = append(l.names, name)
	l.mu.Unlock()

	select {
	case l.namesCh <- struct{}{}:
	default:
	}
}

// had reports whether any open was recorded at all, which distinguishes "the
// automatic name was used" from "nothing opened".
func (l *openerLog) had() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.names) > 0
}

func (l *openerLog) at(i int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i >= len(l.names) {
		return ""
	}

	return l.names[i]
}

// openers returns an openDecoder that builds a namedDecoder and records every
// name it was asked for, in order.
func (l *openerLog) openers() func(name, path string) (decode.Decoder, error) {
	return func(name, path string) (decode.Decoder, error) {
		l.add(name)
		label := name
		if label == "" {
			label = "auto"
		}

		return &namedDecoder{
			toneDecoder: toneDecoder{value: 0.5, total: 1 << 40},
			codec:       label,
		}, nil
	}
}

// acceptCodecs is the validator that goes with namedOpeners: every name is
// legal, because the opener accepts every name.
func acceptCodecs(name string) error {
	switch name {
	case "", "codec-a", "codec-b":
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownDecoder, name)
	}
}

// TestDecoderPreferenceIsReadPerTrack is the load-bearing test for runtime
// switching: the preference must be consulted when each track is built, so a
// change lands on the next track without rebuilding the session.
func TestDecoderPreferenceIsReadPerTrack(t *testing.T) {
	log := newOpenerLog()

	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = log.openers()
	cfg.validateDecoder = acceptCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.PlayQueue([]string{"one", "two"}); err != nil {
		t.Fatalf("PlayQueue: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the first track to be built", func() bool { return log.at(0) != "" })

	if got := log.at(0); got != "codec-a" {
		t.Fatalf("first track opened with %q, want codec-a", got)
	}

	// Switch while the first track is still playing.
	if err := s.SetSettings(Settings{
		Volume:    1,
		Decoder:   "codec-b",
		Backend:   cfg.Backend,
		ProbeMode: core.DurationProbe,
	}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	eventually(t, 3*time.Second, "the second track to be built", func() bool { return log.at(1) != "" })

	if got := log.at(1); got != "codec-b" {
		t.Fatalf("second track opened with %q, want codec-b", got)
	}
}

// TestDecoderPreferenceDoesNotAffectTheCurrentTrack pins the documented
// limitation: a change is for the next track, not a live swap.
func TestDecoderPreferenceDoesNotAffectTheCurrentTrack(t *testing.T) {
	log := newOpenerLog()

	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = log.openers()
	cfg.validateDecoder = acceptCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("one"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the track to be built", func() bool { return log.at(0) != "" })

	if err := s.SetSettings(Settings{Volume: 1, Decoder: "codec-b", Backend: cfg.Backend, ProbeMode: core.DurationProbe}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	// The snapshot still describes the decoder the running track was built
	// with; nothing re-opened it.
	if got := s.Snapshot().Decoder; got != "codec-a" {
		t.Fatalf("Snapshot.Decoder = %q, want codec-a (the running track's decoder)", got)
	}
	if got := log.at(1); got != "" {
		t.Fatalf("a preference change re-opened the track: second open %q", got)
	}
}

func TestEmptyDecoderPreferenceMeansAutomatic(t *testing.T) {
	log := newOpenerLog()

	cfg := testConfig()
	cfg.openDecoder = log.openers()
	cfg.validateDecoder = acceptCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newPumpDevice(nil) }}).new
	s := newSession(t, cfg)

	if err := s.Play("one"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, s, StatePlaying, 3*time.Second)
	eventually(t, 2*time.Second, "the track to be built", func() bool { return log.had() })

	if got := log.at(0); got != "" || !log.had() {
		t.Fatalf("opened with %q, want the empty automatic preference", got)
	}
}

func TestSetSettingsRejectsUnknownDecoderAndChangesNothing(t *testing.T) {
	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.validateDecoder = acceptCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	err := s.SetSettings(Settings{Volume: 1, Decoder: "nope", Backend: cfg.Backend, ProbeMode: core.DurationProbe})
	if !errors.Is(err, ErrUnknownDecoder) {
		t.Fatalf("SetSettings error = %v, want ErrUnknownDecoder", err)
	}
	if got := s.Settings().Decoder; got != "codec-a" {
		t.Fatalf("Settings().Decoder = %q, want the unchanged codec-a", got)
	}
}

func TestSetSettingsIsAtomicAcrossFields(t *testing.T) {
	cfg := testConfig()
	cfg.Decoder = "codec-a"
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.validateDecoder = acceptCodecs
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	before := s.Settings()

	// Volume is valid, the decoder is not: nothing may change, including the
	// volume, or a caller that got an error would still have altered state.
	err := s.SetSettings(Settings{
		Volume:    0.25,
		Decoder:   "nope",
		Backend:   cfg.Backend,
		ProbeMode: core.DurationScan,
	})
	if !errors.Is(err, ErrUnknownDecoder) {
		t.Fatalf("SetSettings error = %v, want ErrUnknownDecoder", err)
	}

	after := s.Settings()
	if after != before {
		t.Fatalf("a rejected update changed state: before %+v, after %+v", before, after)
	}
}

func TestSetSettingsValidAppliesEverything(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	next := Settings{Volume: 0.5, Decoder: "", Backend: "oto", ProbeMode: core.DurationScan}
	if err := s.SetSettings(next); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if got := s.Settings(); got != next {
		t.Fatalf("Settings() = %+v, want %+v", got, next)
	}
	// Volume is the one field that applies immediately.
	if got := s.gain.Volume(); got != 0.5 {
		t.Fatalf("gain volume = %v, want 0.5", got)
	}
}

func TestSetSettingsValidationCoversEachField(t *testing.T) {
	cfg := testConfig()
	cfg.openDecoder = func(_, _ string) (decode.Decoder, error) {
		return &toneDecoder{value: 0.5, total: 1 << 40}, nil
	}
	cfg.newDevice = (&recorderFactory{build: func() playback.Device { return newRecordingDevice(nil) }}).new
	s := newSession(t, cfg)

	base := Settings{Volume: 1, Decoder: "", Backend: "oto", ProbeMode: core.DurationProbe}

	tests := []struct {
		name   string
		mutate func(*Settings)
		want   error
	}{
		{"volume above max", func(v *Settings) { v.Volume = dsp.MaxVolume + 0.5 }, ErrBadVolume},
		{"volume below min", func(v *Settings) { v.Volume = -0.5 }, ErrBadVolume},
		{"unknown backend", func(v *Settings) { v.Backend = "nope" }, ErrUnknownBackend},
		{"unknown probe mode", func(v *Settings) { v.ProbeMode = core.DurationMode(99) }, ErrBadProbeMode},
		{"unknown decoder", func(v *Settings) { v.Decoder = "nope" }, ErrUnknownDecoder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := base
			tt.mutate(&next)
			if err := s.SetSettings(next); !errors.Is(err, tt.want) {
				t.Fatalf("SetSettings error = %v, want %v", err, tt.want)
			}
			if got := s.Settings(); got != base {
				t.Fatalf("rejected update changed state: %+v", got)
			}
		})
	}
}

func TestValidateHelpersAcceptTheDefaults(t *testing.T) {
	if err := ValidateDecoder(""); err != nil {
		t.Fatalf("ValidateDecoder(\"\") = %v, want nil for automatic", err)
	}
	if err := ValidateBackend(""); err != nil {
		t.Fatalf("ValidateBackend(\"\") = %v, want nil for the default backend", err)
	}
	if err := ValidateVolume(0); err != nil {
		t.Fatalf("ValidateVolume(0) = %v, want nil", err)
	}
	if err := ValidateVolume(1); err != nil {
		t.Fatalf("ValidateVolume(1) = %v, want nil", err)
	}
	for _, mode := range []core.DurationMode{core.DurationUnknown, core.DurationProbe, core.DurationScan} {
		if err := ValidateProbeMode(mode); err != nil {
			t.Fatalf("ValidateProbeMode(%d) = %v, want nil", mode, err)
		}
	}
	// A real registered codec name must validate, otherwise the chooser could
	// not select anything.
	if codecs := decode.Default.Codecs(); len(codecs) > 0 {
		if err := ValidateDecoder(codecs[0].Name); err != nil {
			t.Fatalf("ValidateDecoder(%q) = %v, want nil", codecs[0].Name, err)
		}
	}
}
