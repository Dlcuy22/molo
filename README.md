# molo

A batteries-included audio engine for Go, with decoder, parser, streamer, ring
buffer, audio device, DSP, and metadata reader. Built with extendability and
portability in mind.

It plays audio from local files or from a pluggable provider, and it hands a UI
everything it needs to draw playback, so a consumer does not have to assemble
the decoder-to-device stack itself.

Its DSP scripting engine is one of its core features: effects can be written in
Lua and loaded at runtime, alongside the built-in effects, with no recompile.

## Status

molo is at v1.0.0. The public surface is frozen and the core pieces are in
place: decoders, streamer, ring buffer, device output, DSP, metadata, the Lua
scripting engine, and the facade. The current known limitations are:

- The AAC paths decode AAC-LC only; an HE-AAC (SBR or PS) stream is not supported.
- A forward-only source, such as a network body or a pipe, has no native seek,
  so the streamer reopens and discards to position it. Every local file format
  seeks natively.
- The duration is exact only after the asynchronous probe completes. Until then
  it is reported as unknown, so a UI that gates its seek bar on the duration
  shows no position for the first moment of a track.

## Architecture and approach

molo is a set of components wired together, not one monolith. Decoders and
audio devices implement interfaces owned by their own component, and molo is
the glue plus the components those libraries need to work: a streamer, a ring
buffer, and so on.

The pipeline is one path: a parser frames the source, a decoder turns it into
PCM, the streamer feeds it through a ring buffer, and the device pulls it out.
Volume and effects run after the ring, just before the device, so they never
touch the decode side.

A track reference does not have to be a path. The session hands each ref to the
first configured provider whose `Match` claims it.

Two providers ship built in: `provider.NewNetworkProvider` streams a direct
`http://` or `https://` audio URL through ranged GETs (so seeking is a range
request, not a re-download), and `provider.LocalAudio` is the catch-all that
keeps the historic local-file behaviour, so it stays last in the list. With no
providers configured, the engine uses both, in that order.

A provider (`provider.AudioProvider` returning a `provider.Source`) only ever
returns bytes and metadata, so the engine never imports a network library of its
own. The network provider reads through the same decode registry as a local
file; only the codecs that can open a stream (Ogg Opus and WebM/Opus today) can
play a URL.

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

A DSP effect does not need a fork at all. Write it in Lua, load it with
`script.Load` or `script.LoadDir`, and register the factory with `dsp.Register`
(or `script.Register`, which refuses a duplicate name). The engine then treats
it exactly like a built-in: the same pipeline, the same parameter schema, the
same editor. Scripts run in a sandbox and describe a graph the Go side compiles,
so no Lua runs on the audio thread.

### Portability, in two senses

1. Platform portable. The main decoders and the audio device are pure Go, so
   the engine builds and cross-compiles across platforms without cgo, and it
   does not depend on a large library such as ffmpeg. When a reference codec
   implementation is genuinely needed, the project reaches it through
   `ebitengine/purego`, as it already does for the fast Opus path
   (`libopusfile`). That path is optional and sits behind a build tag, and a
   stub keeps the registry shape identical where it is unavailable.
2. UI portable. The same engine drives a CLI, a TUI, or a GUI. The facade
   exposes cheap queries (`Snapshot`) and commands that return immediately, so a
   consumer never has to manage the low-level machinery, which the library
   already owns.

## Features

- Canonical format. Everything is normalized to 48 kHz stereo float32 before
  the ring, so the audio device opens once and is reused across tracks.
- Decoders for Opus, FLAC, WAV, MP3, M4A, AAC, and Opus in WebM/Matroska. Opus
  has three implementations with different seek trade-offs.
- Codec selection by name, or automatic by weight. `molo -codecs` lists what
  the build supports.
- Streaming through a power-of-two SPSC ring (about 300 ms) with a decoder
  goroutine, high and low watermarks, and 100 ms chunks.
- Seeking native where the format supports it, with latest-wins collapsing so a
  burst of seeks resolves to the last target.
- Live swap. `SwapDecoder` or `SwapBackend` changes the decoder or the device on
  the track that is playing, reopening at the position playback has reached,
  without stopping the stream.
- A DSP pipeline with a post-ring gain stage, a bs2b crossfeed, a fade, an
  effect registry, and a parameter schema a UI can render controls from. An
  effect can also publish live meters, described readings, and plots, and the
  engine exposes a separate effect-chain editor surface (`molo.Effects`) to add,
  remove, move, parameterize, and bypass stages without rebuilding the chain.
  Lua effects from the `script` package sit alongside the built-ins; see
  [Extending it](#extending-it).
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
| `opus` | Portable | 90 | `.opus`, `.ogg` | Pure Go (go-opus), 80 ms seek warm-up |
| `opus-libopusfile` | Fastest | 80 | `.opus`, `.ogg` | Via purego, needs `libopusfile.so.0` |
| `flac` | Lossless | 90 | `.flac` | Bit-exact, normalized to 48 kHz |
| `wav` | Wav | 90 | `.wav` | Uncompressed |
| `mp3` | Mp3 | 90 | `.mp3` | Exact sample seek |
| `m4a` | M4a | 90 | `.m4a`, `.mp4` | AAC-LC only, native seek from the sample table |
| `aac` | Aac | 90 | `.aac` | ADTS, native seek and exact duration from a header index |
| `webm-opus` | WebM | 90 | `.webm`, `.weba` | Opus in WebM/Matroska |

Weight only breaks ties between decoders that claim the same extension, and it
is ignored when a name is requested explicitly.

## Using the library

The facade is `github.com/dlcuy22/molo`. A minimal program:

```go
p, err := molo.New()
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
        case molo.TrackChanged:
            fmt.Println("track:", ev.Path)
        case molo.Failed:
            fmt.Println("error:", ev.Err)
        }
    }
}()

for range time.Tick(250 * time.Millisecond) {
    snap := p.Snapshot()
    fmt.Printf("%s %v / %v\n", snap.State, snap.Position, snap.Duration)
}
```

Options such as the backend, decoder, ring size, probe mode, and providers are
set at `New` (`WithBackend`, `WithDecoder`, `WithRingFrames`, `WithProbeMode`,
`WithProviders`). Volume, decoder, backend, probe mode, and the effect pipeline
can change at runtime through `Settings`, or with `SwapDecoder` and
`SwapBackend` when the change must land on the track that is playing.

## Example UIs

The repository ships three front ends behind one facade, and they share no UI
code.

`cmd/molo` is the headless CLI, plain Go. It lives in the engine module rather
than in a module of its own. It plays the paths given on the command line as a
queue and keeps single-key controls when stdin is a terminal. this is just a reference
implementation of the engine's facade, not actively maintained.

`ui/tui` is a terminal UI built on Bubble Tea and Lip Gloss. It draws a
now-playing panel, a progress bar, a level meter, an optional waveform, and the
queue. this frontend is not actively maintained.

`ui/webui` is a desktop app: a Wails v3 Go backend with a Svelte 5 frontend. It
has transport controls, a clickable queue, decoder and backend selection, album
artwork in the now-playing row, a bar visualizer driven by the engine's `Tap`,
the effect editor, and YouTube Music search and queueing through the provider
seam. It is the one front end that renders cover art today. 

The TUI and the desktop app keep their own `go.mod`, joined to the engine by a
`go.work` at the root, so the engine stays free of UI dependencies.

## License

molo is licensed under the MIT License. See [LICENSE](LICENSE).
