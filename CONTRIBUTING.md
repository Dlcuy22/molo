# Contributing

Contributions are welcome. This document covers the workspace layout, the test
commands, and the two extension points a new feature usually touches.

## Workspace layout

The repository is a Go workspace with three modules, joined by the `go.work` at
the root:

| Module | Path | Contents |
|---|---|---|
| engine | `.` | `core`, `decode`, `stream`, `playback`, `dsp`, `script`, `meta`, `analysis`, `provider`, `internal/session`, and the `molo` facade |
| TUI | `ui/tui` | the Bubble Tea front end |
| desktop | `ui/webui` | the Wails v3 backend and Svelte 5 frontend |

The engine depends on no UI framework. Keep it that way: a UI import belongs in
its own module, not in the root.

## Running the tests

Run the tests from the module you changed. The engine is the root module, so:

```sh
go test ./...                    # engine
(cd ui/tui && go test ./...)     # TUI
(cd ui/webui && go test ./...)   # desktop backend
```

Add `-race` to the engine suite before you send a change:

```sh
go test -race ./...
```

`go vet ./...` and `go build ./...` are expected to stay clean as well. CI runs
all of the above plus a `CGO_ENABLED=0` cross-compile matrix; see
`.github/workflows/ci.yml`.

## Adding a codec

A codec is one file under `decode/` plus a test. Implement the `decode.Factory`
interface and register it from an `init`:

```go
func init() {
    Register(NewMyFactory())
}
```

`Factory` is `Name`, `Exts`, `Match`, and `Open`. `Match` is a cheap content
check on the file header, so it must not open or seek. A factory may also
implement any of the optional interfaces:

- `Profile` (`FriendlyName`, `Weight`) to appear in automatic selection and in
  `molo -codecs`.
- `Prober` to report stream shape without decoding.
- `Seeker` when the format supports a native seek. Without it the streamer falls
  back to reopen-and-discard, which still plays but is slower.
- `ReaderOpener` for a pure-Go decoder that can read from any `io.Reader`.

Look at `decode/wav.go` for the smallest complete example. The shared
`pcmConverter` handles rate, channel, and sample-width normalization, so every
decoder emits the canonical 48 kHz stereo float32. The dispatch logic in
`decode/registry.go` never changes for a new codec.

## Adding an effect

There are two ways, and both end at the same `dsp.Effect` interface.

### In Go, under `dsp/`

Implement `dsp.Factory` (`Kind`, `Impl`, `FriendlyName`, `Weight`, `Placement`,
`Schema`, `New`) and register it from an `init`:

```go
func init() {
    Register(NewMyFactory())
}
```

`Kind` is the pipeline slot a preset names; `Impl` is this realisation of it;
`Weight` only breaks ties between implementations of one kind. `New` builds an
`dsp.Effect`, which processes interleaved float32 in place without allocating or
blocking. An effect can also implement `Bypassable`, `Latent`, `Metered`,
`Visualized`, or `Described` for the chain and the UI. See `dsp/crossfeed.go`
for a full example.

### In Lua, under `script/`

A script is a single `.lua` file that returns an `effect { ... }` table with its
parameters and a build function. The `script` package wraps it as an ordinary
`dsp.Factory` under the `script` kind, so the rest of the engine treats it like
a built-in. The bundled scripts live in `script/testdata/`; `eq20.lua` shows the
parameter declarations and `compressor.lua` shows the per-sample graph, latency,
readings, and visual.

Whichever route you take, add a test. The registry and telemetry tests in `dsp/`
and `script/` are the models to copy.

## Commits and pull requests

The history uses Conventional Commits with a scope, for example
`feat(decode): add a Vorbis decoder` or `fix(stream): keep the ring full after a
seek`. Use the scope that names the package you changed.

Before opening a pull request:

- Run the tests for every module you touched, with `-race` for the engine.
- Keep `go vet ./...` and `gofmt` clean.
- Keep the engine free of UI dependencies.
- Prefer editing an existing file over adding one, and keep the diff focused on
  the change.

If a behaviour change is user-visible, add an entry under `## [Unreleased]` in
`CHANGELOG.md`.
