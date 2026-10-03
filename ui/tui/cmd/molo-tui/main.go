// Command molo-tui is the Bubble Tea front end for the molo engine. It
// plays the files and folders named on the command line as a queue and draws
// the now playing panel, a progress bar, a level meter, an optional waveform
// and the queue.
//
// A folder argument is expanded to the playable files inside it, recursively,
// so "molo-tui ~/Music" works the way a listener expects. Files named
// directly are queued where they appear, so an explicit order is preserved.
//
// It talks to the engine through the public facade only, so it shares no code
// with the headless CLI beyond the contract in molo.go.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/dlcuy22/molo"
	"github.com/dlcuy22/molo/analysis"
	"github.com/dlcuy22/molo/ui/tui"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: molo-tui FILE|DIR [FILE|DIR...]")

		return 2
	}

	queue, err := expandArgs(args, supportedExtSet(molo.SupportedExtensions()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "molo-tui: %v\n", err)

		return 1
	}

	p, err := molo.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "molo-tui: %v\n", err)

		return 1
	}
	defer func() { _ = p.Close() }()

	// The queue starts before the program so the first frame already has a
	// track; a failure surfaces through the event bridge as a Failed message.
	if err := p.PlayQueue(queue); err != nil {
		fmt.Fprintf(os.Stderr, "molo-tui: %v\n", err)

		return 1
	}

	prog := tea.NewProgram(tui.NewWithWaveform(p, waveform))
	if _, err := prog.Run(); err != nil {
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "molo-tui: %v\n", err)

		return 1
	}

	return 0
}

// supportedExtSet turns the engine's extension list into a lookup set, each
// extension lowercased so a ".FLAC" matches ".flac". The engine already
// lowercases its list; this keeps the helper correct if that ever changes.
func supportedExtSet(exts []string) map[string]bool {
	set := make(map[string]bool, len(exts))
	for _, e := range exts {
		set[strings.ToLower(e)] = true
	}

	return set
}

// expandArgs turns command-line arguments into a play queue.
//
// A file argument is kept where it appears, in the order written. A directory
// argument is replaced by the playable files under it, sorted, so the same
// folder always yields the same queue. A path that does not exist is an error,
// and a directory with nothing playable in it is an error too: silently
// starting an empty player would look like a hang. Duplicates collapse, so
// naming a file and its folder does not queue it twice.
func expandArgs(args []string, supported map[string]bool) ([]string, error) {
	var queue []string
	seen := make(map[string]bool)

	add := func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		queue = append(queue, path)
	}

	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}

		if !info.IsDir() {
			add(arg)

			continue
		}

		found, err := collectDir(arg, supported)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("%s: no playable files in folder", arg)
		}
		for _, path := range found {
			add(path)
		}
	}

	return queue, nil
}

// collectDir walks root and returns every playable file under it, sorted, so
// the queue is deterministic. A file extension the engine cannot decode is
// skipped rather than queued and then failed on: an album folder commonly holds
// cover art and a playlist file that are not audio.
func collectDir(root string, supported map[string]bool) ([]string, error) {
	var found []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not fatal: the user asked for what is
			// there, and a permission-denied album should not lose the rest.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}
		if d.IsDir() {
			return nil
		}
		if supported[strings.ToLower(filepath.Ext(path))] {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.Sort(found)

	return found, nil
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
