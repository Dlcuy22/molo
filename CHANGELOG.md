# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.1.0] - 2026-10-24

### Changed

- The visualizer settings moved from the main window's control panel into the
  Ctrl+I settings panel, under a new General tab. Add files and Add folder now
  sit above the Decoder and Output selects, so the panel keeps a tidy column
  instead of wrapping a Visualizer dropdown onto a second line.
- Discord Rich Presence moved from the main window's right column into a new
  Integration tab in the Ctrl+I settings panel, card preview included. The right
  column is now just the engine controls and the Effects entry, so it no longer
  scrolls.
- The module path is now `github.com/dlcuy22/molo`, renamed from
  `github.com/dlcuy22/player`. This is a breaking change: update every import
  path, and `go get github.com/dlcuy22/molo`. The public package is `molo`, the
  headless CLI is `cmd/molo`, and the TUI is `cmd/molo-tui`.
- Discord Rich Presence now shows `Listening to <artist>` instead of the
  application name, matching the reference client. The Discord IPC client is
  now part of the web UI, so the activity can carry the name field the wire
  format accepts; the `github.com/axrona/go-discordrpc` dependency is dropped.
- On Windows the process runs at above-normal priority with a 1 ms timer
  resolution, so the WASAPI render thread wakes on time under heavy CPU load.
  Linux and macOS are unchanged: PulseAudio and CoreAudio already schedule
  their own render threads, so the gap this closes is Windows-specific.

### Added

- The engine has a repeat mode: off, all, or one. Repeat all wraps the queue
  back to the first track when the last one ends; repeat one restarts the track
  that just finished. It is a runtime setting like crossfade, exposed through
  `Player.SetRepeat` and reported in the snapshot, and MPRIS now publishes and
  accepts `LoopStatus` instead of refusing the write. Repeat one plays as a hard
  restart with no crossfade, because a prefetch always targets the next track.
- The desktop app answers the transport from the keyboard while the main window
  has focus: `*`/`/` (or the numpad, or `0`/`9`) for volume, `n`/`.` for next,
  `<`/`,` for previous, `s` shuffle, `v` visualizer, `m` mute, `l` repeat all and
  `L` repeat one. The playback keys are inert while the palette or the settings
  panel is open, and while a text field has focus, so they never fight the modal
  chords.
- A centred status card confirms each keyboard action: a bar for volume, a tile
  with an icon for a toggle, and a card with the album cover for a track change.
  One card is on screen at a time; a new action replaces it rather than stacking.
  The volume bar is debounced so a held key shows one card that keeps updating.
- The Ctrl+I settings panel gains a Repeat picker under Playback. The
  visualizer switch under General now persists to `settings.json`, alongside
  repeat, so both survive a restart. Mute and shuffle deliberately do not.
- Discord Rich Presence now shows the album for a YouTube Music track. The
  search and playlist responses carry the album id but not its name, so the name
  is resolved once through the watch endpoint and cached, filling the cover
  tooltip that was previously empty.
- Track crossfade in the engine and the desktop app. A `CrossfadeSettings`
  runtime setting (`SetCrossfade`/`Crossfade` on the facade) fades the end of a
  track into the start of the next over 1 to 12 seconds, with an equal-power
  curve so the blend holds its level. The engine builds the next track early and
  mixes the two streamers for the length of the ramp, so the effects, the master
  gain and the visualizer all see one blended signal. A manual Next, Prev, Seek
  or Stop cancels the blend and hard-cuts, matching a DJ leaving the transition
  to the user. Two consecutive tracks from the same album are handed over
  gaplessly instead of blended, the way Spotify and Apple Music treat a
  continuous album.
- A settings panel in the desktop app, opened with Ctrl+I (Cmd+I on macOS). It
  is a square overlay with a tab per category (General, Playback, Stream), a
  shared row shape, and reusable switch, slider, text and picker controls. Only
  Playback carries a setting today, the crossfade; General and Stream say they
  are empty rather than inventing controls.
- Persisted application settings in `settings.json` under the platform config
  directory (`os.UserConfigDir()/molo`), beside the Discord choice and the
  effects folder. The write is atomic (a temp file and a rename), and a missing
  or malformed file falls back to the defaults instead of failing startup.
- Direct http/https audio streaming as a built-in source. A URL reference is
  read through ranged GETs by the new `provider.NetworkProvider`, so a seek is a
  range request rather than a whole-file re-download, and the engine still
  imports no network library of its own. The decode registry gained
  `OpenReader`/`OpenReaderNamed`, so a stream is dispatched to a codec by
  content sniffing just like a path; the Ogg Opus and WebM/Opus readers can play
  a URL today. The default provider list is now `Network` then `local`.
- Adding a whole YouTube Music playlist or album to the queue from the desktop
  app's Ctrl+F palette. A playlist or album search hit, or a share URL pasted
  into the field, is resolved to its tracks and appended in catalogue order; the
  queue keeps only `ytm:<videoId>` references, with the catalogue metadata
  cached so each row shows its title and art before it plays.
- MPRIS2 support in the desktop app: molo now appears in the Linux desktop's
  media controls (panel widget, lock screen, media keys, Bluetooth headset
  buttons) and can be driven from them. It is a hand-rolled server on
  `godbus/dbus/v5` rather than a dependency, so the tree stays MIT. The new
  `internal/systemedia` package is the platform seam: MPRIS is the Linux
  backend, and the same interface will carry SMTC on Windows and the macOS
  MediaPlayer framework, with a no-op backend elsewhere.
- A debug console in the desktop app's terminal. When molo is launched from a
  terminal, the engine's consumer-gated `DebugEvents` stream is mirrored there
  as one line per record, so the GUI can be watched without a second webview
  window. A double-clicked GUI launch has no terminal and stays quiet; setting
  `MOLO_DEBUG` allocates a console for that case.
- A consumer-gated debug stream on the session, `DebugEvents`, separate from
  the control event stream so diagnostic traffic can never displace a control
  event. It reports the resolved decoder, parser and backend when a track is
  activated, underrun deltas sampled on the device poll, seek and swap timings,
  pipeline installs, duration probes, state transitions and failures. It costs
  the engine nothing until it is read, and drops the oldest record under
  backpressure, counted in `DebugDropped`.
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

- The YouTube Music search panel no longer labels every album, artist and
  playlist hit as "Preview loop". An idle preview reports an empty path, and a
  hit that cannot be played was given an empty path too, so the two matched and
  the row claimed to be auditioning. The preview label now requires a preview to
  actually be sounding.
- Album, artist and playlist hits in the YouTube Music search panel now draw
  their cover. Those rows carry no playable path, and the artwork was keyed on
  that path, so no fetch was ever made. The cover is now keyed on the row's
  catalogue reference, and the backend indexes every hit rather than only the
  playable ones, so the art has a source.
- The desktop app's queue now scrolls inside its own panel. The queue and the
  right-hand column each scroll on their own and the row is clamped to the
  window height, so scrolling a long queue no longer dragged the right column's
  panels off screen and left them empty. The window's outer scroll is gone once
  the columns sit side by side; it remains only in the stacked layout, where the
  columns cannot fit next to each other.
- Discord Rich Presence no longer shows the " - Topic" suffix on an artist name.
  YouTube's auto-generated channels carry it and yt-dlp copies it into a local
  file's artist tag; only the exact trailing suffix is removed, so a real artist
  whose name contains the word is untouched.
- A crossfade no longer drops a whole buffer to silence when the outgoing
  track's probed duration is longer than its real audio and it ends part-way
  through the blend. The outgoing ending is the end of one side of the blend,
  not of the blend: its remaining frames are silence and the incoming track
  keeps fading in over them, so the handoff has no audible skip. The incoming
  side is also required to have audio buffered before the ramp starts, and a
  blend is not begun while a seek or swap is repositioning the live streamer.
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
- Two Opus implementations with different trade-offs: a portable pure-Go `opus`
  (a fixed-point port of libopus via `tphakala/go-opus`), and an
  `opus-libopusfile` path through `ebitengine/purego` behind a build tag.
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

[Unreleased]: https://github.com/dlcuy22/molo/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/dlcuy22/molo/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/dlcuy22/molo/releases/tag/v1.0.0
