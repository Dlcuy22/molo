// volume.ts: the optimistic volume and mute, shared by the transport and the
// keyboard.
//
// Both the volume slider and the shortcuts must move the same value, so the
// state lives here rather than inside Transport.svelte. The engine is the
// source of truth, but a round trip is a visible lag on a slider drag and an
// audible one on a held key, so a local override leads the engine and is
// dropped when the server value catches up.
//
// Circular import, resolved deliberately: store.ts needs syncVolumeFromServer
// to reconcile on every snapshot, and volume.ts needs the engine call. Rather
// than a two-way import that only works by module-init luck, volume.ts imports
// nothing from store.ts and exposes initVolume(fn); store.ts injects
// commands.setVolume once, after commands exists. The dependency runs one way,
// store -> volume.

import { get, writable } from "svelte/store";

/** Volume step per keypress, 5%. */
export const VOLUME_STEP = 0.05;

/** volumeOverride is the value the UI is showing ahead of the engine. Null
 *  means "trust the server", which is the state before any edit and again
 *  after the server confirms one. */
export const volumeOverride = writable<number | null>(null);

/** muted is the UI's mute state. Mute is not an engine mode: it is gain 0, so
 *  the engine only ever sees a volume, and this flag is what lets the control
 *  remember the level to restore. */
export const muted = writable<boolean>(false);

// setVolume is commands.setVolume, injected once by store.ts. A no-op before
// injection keeps a stray early call from throwing.
let setVolume: (v: number) => void = () => {};

/** initVolume wires the engine call in. store.ts calls it with
 *  commands.setVolume; it is the seam that breaks the import cycle. */
export function initVolume(cmd: (v: number) => void): void {
  setVolume = cmd;
}

// serverVolume mirrors the last snapshot value, so volumeBy can add to it
// without importing the player store. lastServerVolume gates the reconciliation
// so an unchanged value does not clear an override that is still leading.
let serverVolume = 1;
let lastServerVolume: number | null = null;

/**
 * syncVolumeFromServer reconciles the override with a snapshot value. The first
 * call only records the value. After that, a value that differs from the last
 * seen is the engine confirming a change, so the override is dropped and the
 * UI follows the server again.
 */
export function syncVolumeFromServer(server: number): void {
  serverVolume = server;
  if (lastServerVolume === null) {
    lastServerVolume = server;

    return;
  }
  if (server !== lastServerVolume) {
    lastServerVolume = server;
    volumeOverride.set(null);
  }
}

/** volume is the value the controls should draw: the override while one is
 *  leading, the server value otherwise. */
export function volume(): number {
  return get(volumeOverride) ?? serverVolume;
}

function clamp(v: number): number {
  return Math.min(1, Math.max(0, v));
}

/**
 * volumeBy nudges the volume by delta, clamped to [0, 1]. It fires the engine
 * call on every call, even at a bound, because SetVolume is atomic and must not
 * be second-guessed by the UI. A keypress that raises the volume also unmutes,
 * the same way dragging the slider does.
 */
export function volumeBy(delta: number): void {
  muted.set(false);
  const next = clamp(volume() + delta);
  volumeOverride.set(next);
  setVolume(next);
}

// volumeBeforeMute remembers the level across a mute, so unmute restores what
// was playing rather than silence.
let volumeBeforeMute = 1;

/**
 * toggleMute flips the mute. Muting remembers the current level and sends gain
 * 0; unmuting sends the remembered level back. A remembered 0 (muting from
 * silence) restores to full, never to 0, because unmuting to nothing would look
 * like the button did not work.
 */
export function toggleMute(): void {
  if (get(muted)) {
    muted.set(false);
    setVolume(volumeBeforeMute || 1);

    return;
  }
  volumeBeforeMute = volume();
  muted.set(true);
  setVolume(0);
}
