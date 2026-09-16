package core

import (
	"testing"
	"time"
)

func TestFrameFormatBytesPerFrame(t *testing.T) {
	tests := []struct {
		name string
		in   FrameFormat
		want int
	}{
		{"f32 stereo 48k", FrameFormat{Rate: 48000, Ch: 2, Fmt: F32}, 8},
		{"f32 mono", FrameFormat{Rate: 48000, Ch: 1, Fmt: F32}, 4},
		{"s16 stereo", FrameFormat{Rate: 44100, Ch: 2, Fmt: S16}, 4},
		{"s32 mono", FrameFormat{Rate: 96000, Ch: 1, Fmt: S32}, 4},
		{"s32 5.1", FrameFormat{Rate: 48000, Ch: 6, Fmt: S32}, 24},
		{"zero channels", FrameFormat{Rate: 48000, Ch: 0, Fmt: F32}, 0},
		{"unknown sample format", FrameFormat{Rate: 48000, Ch: 2, Fmt: SampleFormat(99)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.BytesPerFrame(); got != tt.want {
				t.Fatalf("BytesPerFrame() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFrameFormatEqual(t *testing.T) {
	base := FrameFormat{Rate: 48000, Ch: 2, Fmt: F32}
	same := FrameFormat{Rate: 48000, Ch: 2, Fmt: F32}

	if !base.Equal(same) {
		t.Fatal("identical formats reported unequal")
	}
	for _, other := range []FrameFormat{
		{Rate: 44100, Ch: 2, Fmt: F32},
		{Rate: 48000, Ch: 1, Fmt: F32},
		{Rate: 48000, Ch: 2, Fmt: S16},
	} {
		if base.Equal(other) {
			t.Fatalf("%+v reported equal to %+v", base, other)
		}
	}
}

func TestStreamInfoDuration(t *testing.T) {
	tests := []struct {
		name   string
		frames int64
		rate   int
		want   time.Duration
	}{
		{"unknown sentinel yields zero", -1, 48000, 0},
		{"empty stream yields zero", 0, 48000, 0},
		{"one second stereo 48k", 48000, 48000, time.Second},
		{"fractional duration truncates", 1, 3, 333333333 * time.Nanosecond},
		{"zero rate yields zero", 48000, 0, 0},
		{"negative rate yields zero", 48000, -48000, 0},
		{"probed short file", 12000, 48000, 250 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := StreamInfo{
				Format:      FrameFormat{Rate: tt.rate, Ch: 2, Fmt: F32},
				TotalFrames: tt.frames,
			}
			if got := info.Duration(); got != tt.want {
				t.Fatalf("Duration() = %v, want %v", got, tt.want)
			}
		})
	}
}

type stubModule struct {
	name    string
	seen    FrameFormat
	resets  int
	process int
}

func (m *stubModule) Name() string { return m.name }

func (m *stubModule) Configure(in FrameFormat) (FrameFormat, error) {
	m.seen = in

	return in, nil
}

func (m *stubModule) Process(buf []float32, frames int) error {
	m.process += frames

	return nil
}

func (m *stubModule) Reset() error {
	m.resets++

	return nil
}

func TestModuleIsSatisfiable(t *testing.T) {
	var mod Module = &stubModule{name: "gain"}

	if mod.Name() != "gain" {
		t.Fatalf("Name() = %q", mod.Name())
	}
	got, err := mod.Configure(FrameFormat{Rate: 48000, Ch: 2, Fmt: F32})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !got.Equal(FrameFormat{Rate: 48000, Ch: 2, Fmt: F32}) {
		t.Fatalf("Configure returned %+v", got)
	}
	if err := mod.Process(make([]float32, 16), 2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := mod.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
}

func TestDurationModesAreDistinct(t *testing.T) {
	if DurationUnknown == DurationProbe || DurationProbe == DurationScan || DurationUnknown == DurationScan {
		t.Fatal("duration modes must be distinct values")
	}
	if DurationUnknown != 0 {
		t.Fatalf("DurationUnknown = %d, want 0", DurationUnknown)
	}
}

func TestCanonicalFormatIsTheEngineContract(t *testing.T) {
	// The whole pipeline assumes this exact layout: decoders normalize to it,
	// the ring stores it, and the device opens for it. Pinning the values here
	// means changing them is a deliberate act with a failing test, not a silent
	// drift that only shows up as wrong pitch or a rejected device.
	want := FrameFormat{Rate: 48000, Ch: 2, Fmt: F32}
	if CanonicalFormat != want {
		t.Fatalf("CanonicalFormat = %+v, want %+v", CanonicalFormat, want)
	}
	if got := CanonicalFormat.BytesPerFrame(); got != 8 {
		t.Fatalf("BytesPerFrame() = %d, want 8", got)
	}
}
