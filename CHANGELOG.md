# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- The module path is now `github.com/dlcuy22/molo`, renamed from
  `github.com/dlcuy22/player`. This is a breaking change: update every import
  path, and `go get github.com/dlcuy22/molo`. The public package is `molo`, the
  headless CLI is `cmd/molo`, and the TUI is `cmd/molo-tui`.

### Added

- Continuous integration on GitHub Actions: `go vet`, `go build`, `go test`,
  and `go test -race` for the engine, a `CGO_ENABLED=0` cross-compile matrix,
  and a test job for each UI module.
- `CHANGELOG.md` and `CONTRIBUTING.md`.
- Native seek for ADTS AAC and M4A/AAC. ADTS builds a byte index from its frame
  headers, and M4A uses the MP4 sample table, so both land on an access unit
  instead of the reopen-and-discard fallback.
- Exact duration in the source domain. `StreamInfo` carries `SourceSamples` and
  `SourceRate`, so a 44.1 kHz source whose samples do not divide evenly into
  48 kHz frames reports its real length rather than an unknown one.

### Fixed

- Guard the ADTS sample-rate table against a reserved `sampling_frequency_index`,
  which could index past the table and panic on a malformed file.

## [1.0.0]

First release. The engine, its decoders, its DSP and scripting layers, and the
three front ends are usable together. The release date is added when the tag is
cut.

### Added

- The `github.com/dlcuy22/molo` facade: a play queue, non-blocking commands,
  and a polled `Snapshot` for a UI to read.
- One canonical pipeline. Every decoder normalizes to 48 kHz stereo float32
  before a power-of-two SPSC ring (about 300 ms), with a decoder goroutine, high
  and low watermarks, and 100 ms chunks.
- Decoders for Ogg Opus, WebM/Matroska Opus, FLAC, WAV, MP3, M4A, and AAC,
  selected by extension or by header magic.
- Three Opus implementations with different seek trade-offs: a portable pure-Go
  `opus-pion`, a bit-perfect `opus-pion-exact`, and a `opus-libopusfile` path
  through `ebitengine/purego` behind a build tag.
- Codec profiles (friendly name and weight), automatic selection by weight, and
  named selection through `OpenNamed` and `player -codecs`.
- Native seeking where the format supports it, with latest-wins collapsing so a
  burst of seeks resolves to the last target, and a reopen-and-discard fallback
  for forward-only sources.
- A post-ring effect chain: gain, a bs2b crossfeed, and a linear fade, plus a
  pipeline registry with a parameter schema a UI can render controls from.
- Lua scripting: sandboxed DSP effects built from Lua descriptions, bundled
  effects (20-band EQ, compressor, delay, lowpass, tremolo), and a declared
  latency, readings, and visual for the effect panel.
- A metadata reader that prefers embedded container tags and falls back to the
  filename, with cover art carried as raw bytes and a MIME type.
- Visualization through a non-blocking `Tap` on post-gain, pre-device samples,
  plus offline waveform analysis with a disk cache.
- A provider seam: an `AudioProvider` interface with `LocalAudio` as the default,
  so a non-file source can be added without the engine importing a network
  library.
- Events for discrete transitions and a polled snapshot for the continuous
  position. Neither blocks the engine.
- Example front ends, each in its own module: `cmd/molo`, a headless CLI;
  `ui/tui`, a Bubble Tea and Lip Gloss terminal UI; and `ui/webui`, a Wails v3
  and Svelte 5 desktop app.

### Known limitations

- M4A and AAC have no native seek.
- ADTS AAC stores no total in its header, so its duration is learned while
  playing.
- A non-48 kHz source whose sample count does not divide evenly into the 48 kHz
  output reports an unknown duration. 44.1 kHz FLAC is the common trigger. Audio
  and seeking are unaffected; only the length is unknown.
- The built-in effect set is small: gain and crossfeed are the two that ship.

[Unreleased]: https://github.com/Dlcuy22/player/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/Dlcuy22/player/releases/tag/v1.0.0
