package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// waveBuckets is the horizontal resolution of the waveform. One bucket per
// display cell means the drawing never resamples at render time.
const waveBuckets = 96

// Wave is the part of a waveform the UI draws. It is a narrow local type rather
// than analysis.Result so this package never links the decoder, and the model's
// tests never need a real file.
type Wave struct {
	Buckets []float32
	Frames  int64
}

// WaveFunc computes a waveform for a path. The caller supplies it so package
// tui imports no engine package beyond the facade: the command wires
// analysis.Waveform in, a test wires a stub.
type WaveFunc func(ctx context.Context, path string, buckets int) (*Wave, error)

// loadWave runs one waveform pass as a command. It is a Cmd because decoding a
// whole file takes orders of magnitude longer than a frame, and the context
// lets a track change cancel it mid-decode.
func loadWave(ctx context.Context, fn WaveFunc, seq uint64, path string, buckets int) tea.Cmd {
	if fn == nil || path == "" {
		return nil
	}

	return func() tea.Msg {
		w, err := fn(ctx, path, buckets)

		return waveMsg{seq: seq, wave: w, err: err}
	}
}

// renderWave draws the amplitude envelope as a row of block characters, one per
// display cell. Buckets are max-pooled into the cells so a transient is not
// lost when a long file is squeezed into a narrow terminal.
func renderWave(w *Wave, width int) string {
	if w == nil || width <= 0 || len(w.Buckets) == 0 {
		return ""
	}

	const levels = " ▁▂▃▄▅▆▇█"

	cells := make([]byte, width)
	for i := 0; i < width; i++ {
		start := i * len(w.Buckets) / width
		end := (i + 1) * len(w.Buckets) / width
		if end <= start {
			end = start + 1
		}

		var peak float32
		for _, b := range w.Buckets[start:end] {
			if b > peak {
				peak = b
			}
		}
		if peak < 0 {
			peak = 0
		}
		if peak > 1 {
			peak = 1
		}

		cells[i] = levels[int(peak*float32(len(levels)-1)+0.5)]
	}

	return string(cells)
}
