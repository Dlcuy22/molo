# player

A ready-to-use audio engine for Go, batteries included: decoder, parser,
streamer, ring buffer, spectrum analyzer, audio device, DSP, and metadata
reader. Built with extendability and portability in mind.

It plays local files today and hands a UI everything it needs to draw them, so
a consumer does not have to assemble the decoder-to-device stack itself.
Playback is local-file only for now; a network source would need a new source
layer, though the decoder side is already reader-friendly.

## Status

player is in early development. The core pieces are in place (decoders,
streamer, ring buffer, device output, DSP, metadata, and the facade), but the
project is not finished. Current gaps:

- Some formats cannot seek. M4A and AAC have no native seek.
- Some formats cannot report a duration up front. AAC (ADTS) stores no total in
  its header, so the length is learned while playing.
- A non-48 kHz file reports an unknown duration when its sample count does not
  divide evenly into the 48 kHz output. 44.1 kHz FLAC is the common trigger,
  and the same conversion is used by the MP3, WAV, and M4A decoders. Audio and
  seeking are unaffected (where the format supports seeking); only the length
  is unknown. A UI that gates its seek bar on a known duration disables it, so
  such a track plays without a scrubbable position or a percentage.
- The effect set is small. Crossfeed and gain are the two that ship today.

These are being worked on.

## Architecture and approach

player is a set of components wired together, not one monolith. Decoders and
audio devices implement interfaces owned by their own component, and player is
the glue plus the components those libraries need to work: a streamer, a ring
buffer, and so on.

The pipeline is one path: a parser frames the file, a decoder turns it into PCM,
the streamer feeds it through a ring buffer, and the device pulls it out.
Volume and effects run after the ring, just before the device, so they never
touch the decode side.

The code is layered so a UI never reaches past the facade. `core` holds only
types and contracts. `internal/session` owns the queue, the state machine, and
the device, and it is unimportable from outside the module on purpose, so it can
change without breaking consumers. Nothing below the facade imports a UI
framework.

### Extending it

Fork the library and add a codec by implementing the decoder interface and
registering it. The same registry pattern covers playback backends, DSP
effects, and metadata resolvers, so a new piece is one file plus one `init()`
that calls `Register`. A decoder implements only what it needs: seeking,
probing, and reader input are separate optional interfaces, not a wide base
type.

The engine does not depend on a large library such as ffmpeg. When a reference
codec implementation is genuinely needed, the project reaches it through
`ebitengine/purego` without cgo, as it already does for the fast Opus path
(`libopusfile`).

### Portability, in two senses

1. Platform portable. The main decoders and the audio device are pure Go, so
   the engine builds and cross-compiles across platforms without cgo. The
   `libopusfile` path is optional and sits behind a build tag, and a stub keeps
   the registry shape identical where it is unavailable.
2. UI portable. The same engine drives a CLI, a TUI, or a GUI. The facade
   exposes cheap queries (`Snapshot`) and non-blocking commands, so a consumer
   never has to manage the low-level machinery, which the library already owns.

## Features

- Canonical format. Everything is normalized to 48 kHz stereo float32 before
  the ring, so the audio device opens once and is reused across tracks.
- Decoders for Opus, FLAC, WAV, MP3, M4A, and AAC. Opus has three
  implementations with different seek trade-offs.
- Codec selection by name, or automatic by weight. `player -codecs` lists what
  the build supports.
- Streaming through a power-of-two SPSC ring (about 300 ms) with a decoder
  goroutine, high and low watermarks, and 100 ms chunks.
- Seeking native where the format supports it, with latest-wins collapsing so a
  burst of seeks resolves to the last target.
- DSP with a post-ring gain stage, a bs2b crossfeed, an effect registry, and a
  parameter schema a UI can render controls from.
- Metadata from embedded container tags first, filename fallback last, so a UI
  never shows an empty row. Embedded cover art rides along as raw bytes and a
  MIME type; a UI decodes and scales it.
- Visualization through a non-blocking `Tap` that reads post-gain, pre-device
  samples, plus offline waveform analysis with a disk cache.
- Events for discrete transitions and a polled snapshot for the continuous
  position. Neither blocks the engine.

### Decoder implementations

| Name | Label | Weight | Extensions | Notes |
|---|---|---|---|---|
| `opus-pion` | Portable | 90 | `.opus`, `.ogg` | Pure Go, 80 ms seek warm-up |
| `opus-pion-exact` | Bit-perfect | 85 | `.opus`, `.ogg` | 800 ms warm-up, byte-identical after seek |
| `opus-libopusfile` | Fastest | 80 | `.opus`, `.ogg` | Via purego, needs `libopusfile.so.0` |
| `flac` | Lossless | 90 | `.flac` | Bit-exact, normalized to 48 kHz |
| `wav` | Wav | 90 | `.wav` | Uncompressed |
| `mp3` | Mp3 | 90 | `.mp3` | Exact sample seek |
| `m4a` | M4a | 90 | `.m4a`, `.mp4` | AAC-LC only, no seek |
| `aac` | Aac | 90 | `.aac` | ADTS, duration unknown until played |

Weight only breaks ties between decoders that claim the same extension, and it
is ignored when a name is requested explicitly.

A non-48 kHz source whose sample count does not divide evenly into the 48 kHz
output is the case behind the unknown-duration gap in Status. 44.1 kHz FLAC is
the common trigger. The decoder still plays and seeks correctly; only the length
is reported as unknown.

## Using the library

The facade is `github.com/dlcuy22/player`. A minimal program:

```go
p, err := player.New()
if err != nil {
    log.Fatal(err)
}
defer p.Close()

if err := p.PlayQueue([]string{"a.opus", "b.flac"}); err != nil {
    log.Fatal(err)
}

go func() {
    for ev := range p.Events() {
        switch ev := ev.(type) {
        case player.TrackChanged:
            fmt.Println("track:", ev.Path)
        case player.Failed:
            fmt.Println("error:", ev.Err)
        }
    }
}()

for range time.Tick(250 * time.Millisecond) {
    snap := p.Snapshot()
    fmt.Printf("%s %v / %v\n", snap.State, snap.Position, snap.Duration)
}
```

Options such as the backend, decoder, ring size, and probe mode are set at
`New` and can be changed at runtime through `Settings`.

## Example UIs

The repository ships three front ends, each in its own module, all talking to
the same facade and sharing no UI code. They are the portability claim made
concrete.

`cmd/player` is the headless CLI, plain Go. It plays the paths given on the
command line as a queue and keeps single-key controls when stdin is a terminal.

`ui/tui` is a terminal UI built on Bubble Tea and Lip Gloss. It draws a
now-playing panel, a progress bar, a level meter, an optional waveform, and the
queue.

`ui/webui` is a desktop app: a Wails v3 Go backend with a Svelte 5 frontend.
It has transport controls, a clickable queue, decoder and backend selection,
album artwork in the now-playing row, and a bar visualizer driven by the
engine's `Tap`. It is the one front end that renders cover art today.

The TUI and the desktop app keep their own `go.mod`, joined to the engine by a
`go.work` at the root, so the engine stays free of UI dependencies.

## Contributing

Contributions are welcome. The repository is a Go workspace with three modules,
so run the tests from the module you changed:

```sh
go test ./...                    # engine
(cd ui/tui && go test ./...)     # TUI
(cd ui/webui && go test ./...)   # desktop backend
```

A new codec is one file under `decode/` with an `init()` that registers its
factory, plus a test. A new effect is the same shape under `dsp/`.

## License

MIT. Copyright 2026 Asrian Putra. See [LICENSE](LICENSE).
