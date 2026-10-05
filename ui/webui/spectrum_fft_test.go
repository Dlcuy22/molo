package main

import "testing"

// spectrumService builds a service with the engine stubbed, because
// ConfigureSpectrum publishes a snapshot and the real facade has no player here.
func spectrumService(t *testing.T) *PlayerService {
	t.Helper()

	svc := newPlayerService()
	svc.player = newFakePlayer()

	return svc
}

// countingTap is the narrowest spectrum.Tap: it records that the pump reached
// the transform without producing samples, so a test can tell a skipped frame
// from a computed one.
type countingTap struct{ reads int }

func (c *countingTap) Read([]float32) int {
	c.reads++

	return 0
}

// TestSpectrumConfigRoundTripsTheFFTSize covers the dropdown's whole path: what
// the UI reads back is what it sent. A resolver that dropped the field would
// make the dropdown snap to the default on the next tick.
func TestSpectrumConfigRoundTripsTheFFTSize(t *testing.T) {
	svc := spectrumService(t)

	if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 150, MinHz: 20, MaxHz: 20000, FFT: 4096}); err != nil {
		t.Fatalf("ConfigureSpectrum: %v", err)
	}
	if got := svc.SpectrumConfig().FFT; got != 4096 {
		t.Fatalf("SpectrumConfig().FFT = %d, want 4096", got)
	}

	for _, size := range []int{1024, 2048, 8192, 16384} {
		if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 150, MinHz: 20, MaxHz: 20000, FFT: size}); err != nil {
			t.Fatalf("ConfigureSpectrum(%d): %v", size, err)
		}
		if got := svc.SpectrumConfig().FFT; got != size {
			t.Errorf("round trip of %d came back as %d", size, got)
		}
	}
}

// TestConfigureSpectrumZeroFFTKeepsTheDefault covers the older frontend that
// does not send the field: a zero must fall back, not resize the transform to
// nothing.
func TestConfigureSpectrumZeroFFTKeepsTheDefault(t *testing.T) {
	svc := spectrumService(t)

	if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 150, MinHz: 20, MaxHz: 20000}); err != nil {
		t.Fatalf("ConfigureSpectrum: %v", err)
	}
	if got := svc.SpectrumConfig().FFT; got != 8192 {
		t.Fatalf("FFT = %d, want the shipped default 8192", got)
	}
}

// TestSpectrumSchemaOffersTheFFTDropdown pins that the schema tells the UI a
// dropdown exists, with choices, rather than leaving it to infer one.
func TestSpectrumSchemaOffersTheFFTDropdown(t *testing.T) {
	svc := newPlayerService()

	var fft *struct {
		key      string
		kind     string
		choices  int
		fallback float64
	}
	for _, p := range svc.SpectrumSchema() {
		if p.Key == "fft" {
			fft = &struct {
				key      string
				kind     string
				choices  int
				fallback float64
			}{p.Key, p.Kind, len(p.Choices), p.Default}
		}
	}
	if fft == nil {
		t.Fatal("SpectrumSchema has no fft param")
	}
	if fft.kind != "choice" {
		t.Fatalf("fft kind = %q, want %q", fft.kind, "choice")
	}
	if fft.choices != 5 {
		t.Fatalf("fft offers %d choices, want 5", fft.choices)
	}
	if fft.fallback != 8192 {
		t.Fatalf("fft default = %v, want 8192", fft.fallback)
	}
}

// TestSpectrumEnableGateSkipsTheTransform pins that disabling the visualizer
// stops the pump from touching the tap at all, so the off state costs no CPU,
// and that re-enabling resumes it.
func TestSpectrumEnableGateSkipsTheTransform(t *testing.T) {
	svc := spectrumService(t)
	svc.tap = &countingTap{}
	if err := svc.rebuildRunner(); err != nil {
		t.Fatalf("rebuildRunner: %v", err)
	}

	if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 150, MinHz: 20, MaxHz: 20000, Enabled: false}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if svc.SpectrumConfig().Enabled {
		t.Fatal("Enabled = true after disabling")
	}
	svc.publishSpectrum()
	if reads := svc.tap.(*countingTap).reads; reads != 0 {
		t.Fatalf("disabled pump read the tap %d times, want 0", reads)
	}

	if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 150, MinHz: 20, MaxHz: 20000, Enabled: true}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !svc.SpectrumConfig().Enabled {
		t.Fatal("Enabled = false after enabling")
	}
	svc.publishSpectrum()
	if reads := svc.tap.(*countingTap).reads; reads == 0 {
		t.Fatal("enabled pump never read the tap")
	}
}

// TestSpectrumConfigShipsEnabled pins the startup default: a fresh service
// visualizes without the UI having to ask for it.
func TestSpectrumConfigShipsEnabled(t *testing.T) {
	svc := spectrumService(t)

	if !svc.SpectrumConfig().Enabled {
		t.Fatal("a fresh service should ship with the visualizer enabled")
	}
}

// TestSpectrumRebuildKeepsTheEnableGate checks that a shape change carries the
// gate through: what the caller sends is what the service reports back.
func TestSpectrumRebuildKeepsTheEnableGate(t *testing.T) {
	svc := spectrumService(t)
	if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 150, MinHz: 20, MaxHz: 20000, Enabled: false}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 128, MinHz: 20, MaxHz: 20000, Enabled: false}); err != nil {
		t.Fatalf("reshape: %v", err)
	}
	if svc.SpectrumConfig().Enabled {
		t.Fatal("reshape re-enabled a disabled visualizer")
	}
	if got := svc.spectrumCfg.Bars; got != 128 {
		t.Fatalf("Bars = %d, want 128", got)
	}
}

// TestSpectrumEnableToggleKeepsTheRunner pins that flipping only the enable flag
// does not rebuild the transform, so switching back on resumes from the live
// window instead of cold-starting.
func TestSpectrumEnableToggleKeepsTheRunner(t *testing.T) {
	svc := spectrumService(t)
	svc.tap = &countingTap{}
	if err := svc.rebuildRunner(); err != nil {
		t.Fatalf("rebuildRunner: %v", err)
	}
	before := svc.runner

	if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 150, MinHz: 20, MaxHz: 20000, FFT: 8192, Enabled: false}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if svc.runner != before {
		t.Fatal("disabling rebuilt the runner")
	}
	if err := svc.ConfigureSpectrum(SpectrumConfig{Bars: 150, MinHz: 20, MaxHz: 20000, FFT: 8192, Enabled: true}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if svc.runner != before {
		t.Fatal("re-enabling rebuilt the runner")
	}
}
