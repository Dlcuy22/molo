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
  EffectChainInfo,
  EffectKindInfo,
  EffectParamInfo,
  EffectStageInfo,
  Options,
  PreviewConfig,
  Snapshot,
  SpectrumConfig,
  YTMResult,
} from "../../bindings/github.com/dlcuy22/player/ui/webui/models";
import type { Param } from "../../bindings/github.com/dlcuy22/player/ui/webui/internal/spectrum/models";
import type { EffectKind, EffectStage } from "./effect-types";
import { toEffectKind, toEffectStage } from "./effect-adapter";

// The event names mirror the Go constants. They are repeated here rather than
// imported because the generator does not emit string constants for register
// calls; the payload types below are the compile-time check that they match.
const EVENT_SNAPSHOT = "player:snapshot";
const EVENT_FRAME = "player:spectrum";
const EVENT_EFFECT_METERS = "player:effect-meters";

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
  preview: { active: false, path: "", position: 0, duration: 0, loop: false },
};

export const player = writable<Snapshot>(EMPTY);
export const options = writable<Options>({ codecs: [], backends: [], effects: [] });
export const spectrumConfig = writable<SpectrumConfig>({ bars: 150, minHz: 20, maxHz: 20000 });
export const spectrumSchema = writable<Param[]>([]);

/** previewConfig is the preview window the Go side holds. The UI edits it
 *  through SetPreviewConfig and mirrors the accepted value here. */
export const previewConfig = writable<PreviewConfig>({
  startMs: 0,
  lengthMs: 30000,
  fadeMs: 300,
  loop: true,
  volume: 1,
});

/** cover is the current artwork as an inline data URL, or "" when the track has
 *  none. The snapshot carries only the artwork id, so the 4 Hz payload stays
 *  small and the bytes cross the bridge once, when the id changes. */
export const cover = writable<string>("");

/** queueCoverCache memoises the palette's per-track artwork by cover id, so a
 *  row scrolled back into view, or a shared album cover, is not re-fetched.
 *  The value is a promise so concurrent asks for the same id share one call.
 *  It is capped because a long session can scroll a large queue; the oldest
 *  entry is dropped first, matching the backend's own cover cache. */
const queueCoverCache = new Map<string, Promise<string>>();
const queueCoverCacheMax = 64;

// The effect window is the same bundle as the main window, so it is told apart
// by the route rather than a second build.
export const isEffectWindow =
  new URLSearchParams(window.location.search).get("window") === "effects";

/** effectChain is the chain the effect window renders. It is refreshed on the
 *  snapshot tick and after every editor command, so the panel and its meters
 *  track the engine without a second subscription. */
export const effectChain = writable<EffectStage[]>([]);

/** effectKinds is the catalogue the registry tab offers. It is static once
 *  loaded, so it is fetched once on connect. */
export const effectKinds = writable<EffectKind[]>([]);

/** syncEffectChain reads the chain once and maps it into the frozen shape. */
async function syncEffectChain(): Promise<void> {
  try {
    const chain = await PlayerService.EffectChain();
    effectChain.set((chain.stages ?? []).map(toEffectStage));
  } catch {
    // A read failure is not worth the error line; the next tick retries.
  }
}

/** queueCover fetches a queued track's artwork once per cover id. An empty id
 *  means the track has no art, which resolves to "" without a call. */
export function queueCover(id: string, path: string): Promise<string> {
  if (id === "") {
    return Promise.resolve("");
  }
  const hit = queueCoverCache.get(id);
  if (hit) {
    return hit;
  }

  const p = commands.queueCover(path).catch(() => "");
  if (queueCoverCache.size >= queueCoverCacheMax) {
    const oldest = queueCoverCache.keys().next().value;
    if (oldest !== undefined) {
      queueCoverCache.delete(oldest);
    }
  }
  queueCoverCache.set(id, p);

  return p;
}

/** ytmCover memoises YouTube Music artwork by the reference the row carries. It
 *  is separate from the queue cache because its key is the reference, not a
 *  file path, and a search hit is drawn before it is ever queued. */
const ytmCoverCache = new Map<string, Promise<string>>();

/** ytmCover fetches a YouTube Music track's artwork once per reference. The
 *  backend downloads and re-encodes it, so the webview never talks to the
 *  image host itself. */
export function ytmCover(ref: string): Promise<string> {
  if (ref === "") {
    return Promise.resolve("");
  }
  const hit = ytmCoverCache.get(ref);
  if (hit) {
    return hit;
  }

  const p = commands
    .ytmCover(ref)
    .catch(() => "")
    .then((url) => {
      // Do not pin an empty result: the art may not have been fetched yet when
      // the row was first drawn, and a later ask should be able to pick it up.
      if (url === "") {
        ytmCoverCache.delete(ref);
      }

      return url;
    });
  if (ytmCoverCache.size >= queueCoverCacheMax) {
    const oldest = ytmCoverCache.keys().next().value;
    if (oldest !== undefined) {
      ytmCoverCache.delete(oldest);
    }
  }
  ytmCoverCache.set(ref, p);

  return p;
}

/** ytmSearch memoises a settled query's results, so arrowing back through a
 *  half-typed query and returning does not spend a request. A failure is not
 *  cached, so a retry reaches the network. */
const ytmSearchCache = new Map<string, Promise<YTMResult[]>>();
const ytmSearchCacheMax = 32;

/** searchYTM runs a YouTube Music search. The caller owns the debounce; this is
 *  the deduplicated call underneath it. */
export function searchYTM(query: string): Promise<YTMResult[]> {
  const key = query.trim().toLowerCase();
  if (key === "") {
    return Promise.resolve([]);
  }
  const hit = ytmSearchCache.get(key);
  if (hit) {
    return hit;
  }

  const p = commands.searchYTM(query).then(
    (rows) => rows ?? [],
    (err) => {
      // Drop the failure so the next attempt is not served a rejection.
      ytmSearchCache.delete(key);
      throw err;
    },
  );
  if (ytmSearchCache.size >= ytmSearchCacheMax) {
    const oldest = ytmSearchCache.keys().next().value;
    if (oldest !== undefined) {
      ytmSearchCache.delete(oldest);
    }
  }
  ytmSearchCache.set(key, p);

  return p;
}

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

/** EffectMetersInfo is the lean fast-tick payload: one stage's live meters. */
export interface EffectMetersInfo {
  id: string;
  meters: Record<string, number> | null;
}

/**
 * applyEffectMeters merges a fast-tick meter payload into the chain store
 * without touching schema or values. It only updates stages the payload names,
 * so a stage added between ticks keeps the meters it already had until the next
 * full snapshot. The panel's curve and readings read from here, which is what
 * lets the gain-reduction needle move at ~30 Hz instead of the 4 Hz snapshot.
 */
function applyEffectMeters(rows: EffectMetersInfo[]): void {
  if (rows.length === 0) {
    return;
  }
  const byId = new Map(rows.map((r) => [r.id, r.meters]));
  effectChain.update((stages) =>
    stages.map((s) => {
      const meters = byId.get(s.id);
      return meters === undefined ? s : { ...s, meters };
    }),
  );
}

/**
 * connect subscribes to the Go push stream and loads the static data. It is
 * called once from the root component's onMount and returns the teardown.
 */
export function connect(): () => void {
  const apply = (snap: Snapshot) => {
    player.set(snap);
    syncCover(snap.coverId);
    ready.set(true);
    // The effect window reads the chain on the same tick, so its meters move
    // without a subscription of its own.
    if (isEffectWindow) {
      void syncEffectChain();
    }
  };

  const offSnapshot = Events.On(EVENT_SNAPSHOT, (ev) => {
    apply(ev.data as Snapshot);
  });

  // The fast meter tick is a lean payload, so it only touches the chain store.
  // It is registered regardless of window and filters itself: a non-effect
  // window has an empty chain, so the merge is a no-op.
  const offMeters = Events.On(EVENT_EFFECT_METERS, (ev) => {
    applyEffectMeters(ev.data as EffectMetersInfo[]);
  });

  // The first read catches up on anything the engine did before the listener
  // was attached.
  PlayerService.Snapshot().then(apply);
  PlayerService.Options().then(options.set);
  PlayerService.SpectrumConfig().then(spectrumConfig.set);
  PlayerService.SpectrumSchema().then(spectrumSchema.set);
  PlayerService.PreviewConfig().then(previewConfig.set);
  if (isEffectWindow) {
    void syncEffectChain();
    PlayerService.EffectKinds().then((kinds) => {
      effectKinds.set((kinds ?? []).map(toEffectKind));
    });
  }

  return () => {
    offSnapshot();
    offMeters();
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

/**
 * onEffectMeters registers the fast meter listener. It is separate from connect
 * for the same reason onSpectrum is: a component that plots the meters owns the
 * subscription, so a window with no such plot does not pay for the tick. The
 * payload is passed through as-is; the caller filters the rows it wants.
 */
export function onEffectMeters(
  handler: (rows: { id: string; meters: Record<string, number> | null }[]) => void,
): () => void {
  return Events.On(EVENT_EFFECT_METERS, (ev) => {
    handler(ev.data as EffectMetersInfo[]);
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
  // queueCover is the palette's lazy artwork fetch. It is a plain read, not an
  // invoke: a failure means "no art", which is not an error worth surfacing.
  queueCover: (path: string) => PlayerService.QueueCover(path),
  // YouTube Music is catalogue work, not engine work. A search is a plain read
  // whose failure the palette reports in its own body, next to the query; a
  // play is an engine command like any other, so it goes through invoke and
  // lands on the error line. ytmCover reuses the same read path as queueCover,
  // because the backend serves both as a data URL.
  searchYTM: (query: string) => PlayerService.SearchYTMSongs(query),
  insertNextYTM: (videoID: string) => invoke(PlayerService.InsertNextYTM(videoID)),
  appendYTM: (videoID: string) => invoke(PlayerService.AppendYTM(videoID)),
  ytmCover: (ref: string) => PlayerService.QueueCover(ref),
  // The preview calls are plain too: a preview is an audition, so a rejected
  // one should quietly do nothing rather than take over the error line. The
  // config setter mirrors the value locally so a slider stays responsive.
  previewStart: (path: string) => PlayerService.PreviewStart(path).catch(() => {}),
  previewStop: () => PlayerService.PreviewStop().catch(() => {}),
  setPreviewConfig: (cfg: PreviewConfig) => {
    previewConfig.set(cfg);
    PlayerService.SetPreviewConfig(cfg).catch(() => {});
  },

  // Effect commands. openEffectWindow is the main window's entry point; the
  // rest are the effect window's editor calls.
  openEffectWindow: () => invoke(PlayerService.OpenEffectWindow()),
  addEffect: (kind: string, impl: string) => editEffect(() => PlayerService.AddEffect(kind, impl)),
  removeEffect: (id: string) => editEffect(() => PlayerService.RemoveEffect(id)),
  moveEffect: (id: string, to: number) => editEffect(() => PlayerService.MoveEffect(id, to)),
  setEffectBypass: (id: string, bypassed: boolean) => {
    // Optimistic: the checkbox and the panel must react on the click, not on
    // the next snapshot. The 4 Hz tick reconciles the true value.
    effectChain.update((stages) =>
      stages.map((s) =>
        s.id === id
          ? { ...s, bypassed, values: { ...s.values, bypass: bypassed } }
          : s,
      ),
    );
    invoke(PlayerService.SetEffectBypass(id, bypassed));
  },
  // setEffectParam is called on every slider input, so it updates the store
  // first and does not await a chain read. The engine call still lands, and the
  // snapshot tick reconciles any value the effect clamped differently.
  setEffectParam: (id: string, key: string, value: unknown) => {
    effectChain.update((stages) =>
      stages.map((s) =>
        s.id === id ? { ...s, values: { ...s.values, [key]: value } } : s,
      ),
    );
    invoke(PlayerService.SetEffectParam(id, key, value));
  },
};

/** invoke awaits one engine call and folds a rejection into the error line. */
async function invoke(p: Promise<unknown>): Promise<void> {
  try {
    await p;
  } catch (err) {
    player.update((s) => ({ ...s, error: errorText(err) }));
  }
}

/**
 * editEffect runs an editor command and then re-reads the chain. The engine
 * rebuilds the chain for add, remove and move, so the store's copy is stale
 * until the read lands; awaiting it keeps the sidebar in step with what the
 * engine actually installed.
 */
async function editEffect(fn: () => Promise<unknown>): Promise<void> {
  try {
    await fn();
  } catch (err) {
    player.update((s) => ({ ...s, error: errorText(err) }));
  }
  await syncEffectChain();
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
