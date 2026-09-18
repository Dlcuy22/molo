package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/dlcuy22/player"
	"github.com/dlcuy22/player/core"
	"github.com/dlcuy22/player/decode"
)

// env is every seam run needs, so the whole command is exercisable without a
// terminal or an audio device.
type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// interactive is true only when a single-key terminal session is possible.
	// It gates both raw mode and the repainting display; without it the CLI
	// must never read stdin or emit control codes.
	interactive bool

	// makeRaw switches the terminal to single-key mode and returns a restore
	// function. It is called only when interactive is true.
	makeRaw func() (func(), error)

	// newPlayer builds the facade. It is injected so tests never open audio.
	newPlayer func(...player.Option) (player.Player, error)

	// probe resolves a duration for the preflight. A nil probe disables it.
	probe func(path string, mode core.DurationMode) (time.Duration, error)

	// signals delivers termination requests. A nil channel is legal and simply
	// never fires.
	signals <-chan os.Signal

	// pollInterval is the display refresh period.
	pollInterval time.Duration
}

// run is the whole command minus os.Exit. It returns the process exit code.
func run(args []string, e env) int {
	opts, err := parseArgs(args)
	if err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintf(e.stderr, "player: %v\n\n%s", err, usageText)
		} else {
			fmt.Fprintf(e.stderr, "player: %v\n", err)
		}

		return classify(err)
	}
	if opts.help {
		fmt.Fprint(e.stdout, usageText)

		return exitOK
	}
	if opts.codecs {
		printCodecs(e.stdout)

		return exitOK
	}

	paths, err := resolveQueue(opts.paths, decode.Default.Supported())
	if err != nil {
		fmt.Fprintf(e.stderr, "player: %v\n", err)

		return classify(err)
	}
	if len(paths) == 0 {
		fmt.Fprintf(e.stderr, "player: %v\n", errNoInput)

		return classify(errNoInput)
	}

	if opts.list {
		for _, path := range paths {
			fmt.Fprintln(e.stdout, path)
		}

		return exitOK
	}

	// Resolve durations before playback so the display can show a total during
	// the window before the engine's own async probe answers. This is the one
	// place the engine's probe mode is not configurable through the facade, so
	// the CLI pays for it itself. A non-interactive run paints no progress
	// line, so it must not pay for a preflight it would never show.
	var durations []time.Duration
	if e.interactive {
		durations = preflight(paths, opts.probe, e.probe)
	}

	restore := func() {}
	if e.interactive && e.makeRaw != nil {
		r, err := e.makeRaw()
		if err != nil {
			fmt.Fprintf(e.stderr, "player: terminal: %v\n", err)

			return exitFailure
		}
		restore = r
	}
	defer restore()

	playerOpts := []player.Option{
		player.WithBackend(opts.backend),
		player.WithProbeMode(opts.probe),
	}
	// An empty -decoder means automatic selection, which is the facade's own
	// default, so the option is omitted rather than passed as empty.
	if opts.decoder != "" {
		playerOpts = append(playerOpts, player.WithDecoder(opts.decoder))
	}

	p, err := e.newPlayer(playerOpts...)
	if err != nil {
		fmt.Fprintf(e.stderr, "player: %v\n", err)

		return exitFailure
	}

	// SetVolume is what makes -volume 0 mute; the facade treats a zero config
	// volume as "unset, use unity".
	p.SetVolume(opts.volume)

	return classify(play(p, opts, paths, durations, e))
}

// play runs the queue and returns the terminal error, if any. It owns the
// player's lifecycle and joins every goroutine it starts except the key reader,
// which is blocked on stdin and cannot be interrupted; see the note below.
func play(p player.Player, opts options, paths []string, durations []time.Duration, e env) error {
	finish := make(chan struct{})
	keyEnd := make(chan action, 1)
	var once sync.Once
	end := func() { once.Do(func() { close(finish) }) }

	var (
		wg        sync.WaitGroup
		failed    error
		sawActive bool
	)

	// The event consumer is its own goroutine so a slow writer can never block
	// the engine: the engine drops events rather than waiting for this loop.
	wg.Add(1)
	go func() {
		defer wg.Done()

		var current string
		for {
			select {
			case <-finish:
				return
			case ev, ok := <-p.Events():
				if !ok {
					return
				}
				switch ev := ev.(type) {
				case player.TrackChanged:
					sawActive = true
					current = ev.Path
					announceTrack(e, ev.Path)
				case player.TrackEnded:
					announceEnd(e, current)
				case player.StateChanged:
					if ev.To == player.Playing || ev.To == player.Paused {
						sawActive = true
					}
					if ev.To == player.Stopped && sawActive {
						end()
					}
				case player.Failed:
					failed = ev.Err
					fmt.Fprintf(e.stderr, "player: %v\n", ev.Err)
					end()
				}
			}
		}
	}()

	if e.interactive {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// One paint before the first tick guarantees a visible line even on
			// a queue that ends immediately.
			paint(e, p, durations, opts.probe)

			ticker := time.NewTicker(e.pollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-finish:
					return
				case <-ticker.C:
					paint(e, p, durations, opts.probe)
				}
			}
		}()

		// The key reader is intentionally not in the WaitGroup: a blocking read
		// on a real terminal cannot be interrupted, and the process exits
		// shortly after play returns. Waiting on it would deadlock quit. The
		// action travels over a channel rather than a shared variable so the
		// handoff to the main select is synchronised.
		go func() {
			for {
				select {
				case <-finish:
					return
				default:
				}

				k, err := readKey(e.stdin)
				if err != nil {
					// End of input is not a command: keep playing.
					return
				}
				if a := handleKey(p, k); a != actionNone {
					select {
					case keyEnd <- a:
					case <-finish:
					}

					return
				}
			}
		}()
	}

	// Start the queue only after both consumers are running: a track short
	// enough to end immediately must not have its events missed.
	if err := p.PlayQueue(paths); err != nil {
		end()
		wg.Wait()
		_ = p.Close()

		return err
	}

	var (
		signal os.Signal
		keyed  action
	)
	select {
	case <-finish:
	case signal = <-e.signals:
		end()
	case keyed = <-keyEnd:
		end()
	}

	end()
	wg.Wait()

	if e.interactive {
		fmt.Fprintln(e.stdout)
	}
	if err := p.Close(); err != nil && failed == nil && signal == nil && keyed == actionNone {
		failed = err
	}
	switch {
	case signal != nil:
		return fmt.Errorf("interrupted by %v", signal)
	case keyed == actionInterrupt:
		return errors.New("interrupted")
	default:
		return failed
	}
}

// paint renders one progress line in place. The carriage return plus erase is
// the entire reason interactive output is gated on a terminal.
func paint(e env, p player.Player, durations []time.Duration, mode core.DurationMode) {
	snap := p.Snapshot()
	snap.Duration = displayDuration(snap, durations, mode)
	fmt.Fprintf(e.stdout, "\r%s\x1b[K", formatProgress(snap))
}

// announceTrack and announceEnd are the plain-line equivalents of the
// repainting display: they are all a non-TTY consumer sees.
func announceTrack(e env, path string) {
	if e.interactive {
		return
	}
	fmt.Fprintf(e.stdout, "playing %s\n", displayName("", path))
}

func announceEnd(e env, path string) {
	if e.interactive || path == "" {
		return
	}
	fmt.Fprintf(e.stdout, "ended %s\n", displayName("", path))
}

// preflight resolves a duration for every track up front. DurationUnknown and
// a missing probe both mean "do not preflight", which keeps startup instant.
func preflight(paths []string, mode core.DurationMode, probe func(string, core.DurationMode) (time.Duration, error)) []time.Duration {
	if mode == core.DurationUnknown || probe == nil {
		return nil
	}

	durations := make([]time.Duration, len(paths))
	for i, path := range paths {
		d, err := probe(path, mode)
		if err != nil {
			continue
		}
		durations[i] = d
	}

	return durations
}

// displayDuration picks the total to show. The engine's own probe is the
// source of truth once it lands; the CLI's preflight is only a placeholder so
// the first frames have a total. Preferring the engine matters because the two
// can legitimately differ (a scan versus a tail probe), and a displayed total
// that disagrees with what is playing is worse than a brief unknown.
func displayDuration(snap player.Snapshot, durations []time.Duration, mode core.DurationMode) time.Duration {
	if snap.Duration > 0 {
		return snap.Duration
	}
	if mode == core.DurationUnknown {
		return 0
	}
	if snap.QueueIndex >= 0 && snap.QueueIndex < len(durations) && durations[snap.QueueIndex] > 0 {
		return durations[snap.QueueIndex]
	}

	return 0
}
