// Store: the one place that talks to the Go service.
//
// The frontend never polls the engine. Go pushes a whole Snapshot on every
// accepted change, so this module owns exactly one subscription and derives
// everything the components render from that single value. A component reads
// `player` and calls a command; it never reaches across the bridge itself.

import { get, writable } from "svelte/store";
import { Events } from "@wailsio/runtime";
import { PlayerService } from "../../bindings/github.com/dlcuy22/player/ui/webui";
import type {
  Options,
  Snapshot,
  SpectrumConfig,
} from "../../bindings/github.com/dlcuy22/player/ui/webui/models";
import type { Param } from "../../bindings/github.com/dlcuy22/player/ui/webui/internal/spectrum/models";

// The event names mirror the Go constants. They are repeated here rather than
// imported because the generator does not emit string constants for register
// calls; the payload types below are the compile-time check that they match.
const EVENT_SNAPSHOT = "player:snapshot";
const EVENT_FRAME = "player:spectrum";

/** Empty is the honest initial state: nothing is playing and nothing is queued. */
export const EMPTY: Snapshot = {
  state: "idle",
  path: "",
  title: "",
  artist: "",
  album: "",
  coverId: "",
  coverMime: "",
  codec: "",
  container: "",
  position: 0,
  duration: 0,
  volume: 1,
  queue: [],
  queueIdx: -1,
  decoder: "",
  decoderPref: "",
  backend: "",
  sampleRate: 48000,
  channels: 2,
  error: "",
};

export const player = writable<Snapshot>(EMPTY);
export const options = writable<Options>({ codecs: [], backends: [], effects: [] });
export const spectrumConfig = writable<SpectrumConfig>({ bars: 150, minHz: 20, maxHz: 20000 });
export const spectrumSchema = writable<Param[]>([]);

/** cover is the current artwork as an inline data URL, or "" when the track has
 *  none. The snapshot carries only the artwork id, so the 4 Hz payload stays
 *  small and the bytes cross the bridge once, when the id changes. */
export const cover = writable<string>("");

/** coverId tracks which artwork the current cover value belongs to, so a late
 *  reply for a track that already changed is dropped instead of shown. */
let coverId = "";

/** syncCover fetches the artwork for an id at most once. A repeat id (a
 *  redraw, or returning to a track) keeps the value already on screen. */
function syncCover(id: string): void {
  if (id === coverId) {
    return;
  }
  coverId = id;
  if (id === "") {
    cover.set("");

    return;
  }

  PlayerService.Cover(id).then(
    (url) => {
      if (coverId === id) {
        cover.set(url);
      }
    },
    () => {
      if (coverId === id) {
        cover.set("");
      }
    },
  );
}

/** busy is true while a file or folder is being resolved, which is the one slow
 *  command: opening a dialog and walking a directory tree. */
export const busy = writable(false);

/** ready flips once the first snapshot has arrived, so the UI can tell "not yet
 *  loaded" apart from "nothing playing" (R-27). */
export const ready = writable(false);

/**
 * connect subscribes to the Go push stream and loads the static data. It is
 * called once from the root component's onMount and returns the teardown.
 */
export function connect(): () => void {
  const apply = (snap: Snapshot) => {
    player.set(snap);
    syncCover(snap.coverId);
    ready.set(true);
  };

  const offSnapshot = Events.On(EVENT_SNAPSHOT, (ev) => {
    apply(ev.data as Snapshot);
  });

  // The first read catches up on anything the engine did before the listener
  // was attached.
  PlayerService.Snapshot().then(apply);
  PlayerService.Options().then(options.set);
  PlayerService.SpectrumConfig().then(spectrumConfig.set);
  PlayerService.SpectrumSchema().then(spectrumSchema.set);

  return () => {
    offSnapshot();
  };
}

/**
 * onSpectrum registers the visualizer frame listener. It is separate from
 * connect because the spectrum component owns the subscription: a UI that
 * hides the visualizer should not pay for 60 frames a second.
 */
export function onSpectrum(handler: (bands: Float64Array) => void): () => void {
  return Events.On(EVENT_FRAME, (ev) => {
    const data = ev.data as number[] | Float64Array;
    handler(data instanceof Float64Array ? data : Float64Array.from(data));
  });
}

// The command wrappers below keep every engine call in one module, so a
// component never builds a binding import of its own. Every one goes through
// invoke, so a rejection is surfaced in the error line and never becomes an
// unhandled promise.
export const commands = {
  togglePause: () => invoke(PlayerService.TogglePause()),
  next: () => invoke(PlayerService.Next()),
  prev: () => invoke(PlayerService.Prev()),
  stop: () => invoke(PlayerService.Stop()),
  seek: (ms: number) => invoke(PlayerService.SeekTo(Math.max(0, Math.round(ms)))),
  setVolume: (v: number) => invoke(PlayerService.SetVolume(v)),
  playIndex: (i: number) => invoke(PlayerService.PlayIndex(i)),
  setCodec: (name: string) => invoke(PlayerService.SetCodec(name)),
  setBackend: (name: string) => invoke(PlayerService.SetBackend(name)),
  configureSpectrum: (cfg: SpectrumConfig) =>
    invoke(PlayerService.ConfigureSpectrum(cfg)),
  // The two pickers are the slow commands: they open a native dialog and may
  // walk a folder tree, so they also drive the busy flag.
  openFiles: () => run(() => PlayerService.OpenFiles()),
  openFolder: () => run(() => PlayerService.OpenFolder()),
};

/** invoke awaits one engine call and folds a rejection into the error line. */
async function invoke(p: Promise<unknown>): Promise<void> {
  try {
    await p;
  } catch (err) {
    player.update((s) => ({ ...s, error: errorText(err) }));
  }
}

/** run is invoke plus the busy flag, for the commands that can take a while. */
async function run(fn: () => Promise<void>): Promise<void> {
  busy.set(true);
  try {
    await fn();
  } catch (err) {
    player.update((s) => ({ ...s, error: errorText(err) }));
  } finally {
    busy.set(false);
  }
}

/** errorText unwraps the message the engine sends. The binding rejects with a
 *  plain string, not an Error, so both shapes are read. */
function errorText(err: unknown): string {
  if (err instanceof Error) {
    return err.message;
  }
  if (typeof err === "string" && err.length > 0) {
    return err;
  }

  return "the player rejected that action";
}


/** current reads the store without subscribing, for event handlers. */
export function current(): Snapshot {
  return get(player);
}
