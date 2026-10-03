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