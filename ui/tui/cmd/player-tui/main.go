// Command player-tui is the Bubble Tea front end for the player engine. It
// plays the files named on the command line as a queue and draws the now
// playing panel, a progress bar, a level meter, an optional waveform and the
// queue.
//
// It talks to the engine through the public facade only, so it shares no code
// with the headless CLI beyond the contract in player.go.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/analysis"
	"github.com/dlcuy22/player/ui/tui"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: player-tui FILE [FILE...]")

		return 2
	}

	p, err := player.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "player-tui: %v\n", err)

		return 1
	}
	defer func() { _ = p.Close() }()

	// The queue starts before the program so the first frame already has a
	// track; a failure surfaces through the event bridge as a Failed message.
	if err := p.PlayQueue(args); err != nil {
		fmt.Fprintf(os.Stderr, "player-tui: %v\n", err)

		return 1
	}

	prog := tea.NewProgram(tui.NewWithWaveform(p, waveform))
	if _, err := prog.Run(); err != nil {
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "player-tui: %v\n", err)

		return 1
	}

	return 0
}

// waveform adapts analysis.Waveform to the shape the UI expects. It reduces the
// buckets to their RMS, which is what the display draws, and never touches the
// playback ring: analysis opens its own decoder.
func waveform(ctx context.Context, path string, buckets int) (*tui.Wave, error) {
	res, err := analysis.Waveform(ctx, path, analysis.Options{Buckets: buckets})
	if err != nil {
		return nil, err
	}

	out := &tui.Wave{Buckets: make([]float32, len(res.Buckets)), Frames: res.Frames}
	for i, b := range res.Buckets {
		out.Buckets[i] = b.RMS
	}

	return out, nil
}
