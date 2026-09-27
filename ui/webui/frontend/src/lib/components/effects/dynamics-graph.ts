// Pure helpers for the scrolling dynamics graph. They live in a .ts module rather
// than in the component so the ring buffer and the pixels-per-dB mapping are
// unit-testable without a DOM, and so a value's series rule has one definition.

import type { Reading } from "../../effect-types";

/**
 * Ring is a fixed-capacity series buffer. The graph redraws from the whole
 * series each frame, so the buffer is the only growing state; a fixed capacity
 * caps memory and gives the display its time window.
 */
export interface Ring {
  readonly capacity: number;
  /** Oldest first, one entry per sample, holes kept as null. */
  readonly values: (number | null)[];
  /** The slot the next full-ring push overwrites; stays 0 until the ring fills. */
  next: number;
}

/** createRing builds an empty ring of the given capacity. */
export function createRing(capacity: number): Ring {
  const cap = Math.max(1, Math.floor(capacity));

  return { capacity: cap, values: [], next: 0 };
}

/**
 * push appends one sample. A non-finite value is stored as a null hole rather
 * than a number, so a gap in the meter stream stays a gap the drawer can skip
 * instead of a fake zero that would read as silence.
 */
export function push(ring: Ring, value: number | null): void {
  const v = typeof value === "number" && Number.isFinite(value) ? value : null;
  if (ring.values.length < ring.capacity) {
    // Pre-full the append is the write, and the cursor stays at 0, which is
    // where the first wrap-around write lands.
    ring.values.push(v);

    return;
  }
  ring.values[ring.next] = v;
  ring.next = (ring.next + 1) % ring.capacity;
}

/**
 * series returns the buffer oldest-to-newest. Before the ring is full that is
 * the insertion order; once it has wrapped the write cursor points at the
 * oldest entry, so the tail is rotated to the front. A hole is returned as null.
 */
export function series(ring: Ring): (number | null)[] {
  if (ring.values.length < ring.capacity) {
    return ring.values.slice();
  }

  return [...ring.values.slice(ring.next), ...ring.values.slice(0, ring.next)];
}

/**
 * levelToY maps a dB value onto a pixel y where yMin sits at the bottom (height)
 * and yMax at the top (0). A range that is non-finite or has no span has no
 * defined direction, and a non-finite value has no position, so both return
 * null: the caller skips the sample rather than drawing a NaN coordinate.
 */
export function levelToY(
  value: number,
  yMin: number,
  yMax: number,
  height: number,
): number | null {
  if (
    !Number.isFinite(value) ||
    !Number.isFinite(yMin) ||
    !Number.isFinite(yMax) ||
    !Number.isFinite(height) ||
    yMax <= yMin
  ) {
    return null;
  }
  const frac = (value - yMin) / (yMax - yMin);
  const clamped = Math.min(1, Math.max(0, frac));

  return height * (1 - clamped);
}

/**
 * plotX maps a sample index onto a pixel x. The newest sample (index
 * length - 1) sits at the right edge and the window is a fixed capacity, so
 * before the ring is full the trace grows leftward from the edge instead of
 * being stretched across the width. A capacity below one has no window, so
 * every sample lands on the right edge.
 */
export function plotX(
  index: number,
  length: number,
  width: number,
  capacity: number,
): number {
  const cap = Math.max(1, Math.floor(capacity));
  const right = Math.max(0, width);
  const back = length - 1 - index;
  if (cap <= 1) {
    return right;
  }

  return right - (right * back) / (cap - 1);
}

/**
 * meterValue reads one key out of a meters map. A missing key, a non-finite
 * value, or a null map is null, which lets an absent meter stay distinct from a
 * real zero.
 */
export function meterValue(
  meters: Record<string, number> | null | undefined,
  key: string,
): number | null {
  const v = meters?.[key];

  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/**
 * graphSignature is the identity of a mounted graph. The panel rebuilds the
 * readings and visual objects on every meter tick, so an effect that depended
 * on them directly would restart its draw loop 60 times a second and never
 * paint a frame. The signature is a string, so a re-created object with equal
 * contents yields the same value and does not rebuild. It changes only when the
 * stage, the overlay set, a series kind, or the scale actually changes.
 */
export function graphSignature(
  stageId: string,
  overlays: string[],
  series: SeriesInfo[],
  xMin: number,
  xMax: number,
  yMin: number,
  yMax: number,
): string {
  return JSON.stringify({
    stage: stageId,
    overlays,
    kinds: series.map((s) => `${s.key}:${s.kind}`),
    x: [xMin, xMax],
    y: [yMin, yMax],
  });
}

/** SeriesKind is how one overlay key is drawn. */
export type SeriesKind = "level" | "gain-reduction" | "scalar";

export interface SeriesInfo {
  key: string;
  kind: SeriesKind;
  /** The declared reading, when the key names one. */
  reading: Reading | null;
}

/**
 * resolveSeries gives every overlay key its kind. The declared reading is the
 * authority when one exists; a key with no reading defaults to "level", because
 * the standard In/Out pair is not always listed in the readings and a bare
 * level is the only thing it can be. Order follows the overlay declaration, so
 * the drawer's series order is stable.
 */
export function resolveSeries(
  overlays: string[],
  readings: Reading[],
): SeriesInfo[] {
  const byKey = new Map(readings.map((r) => [r.key, r]));

  return overlays.map((key) => {
    const reading = byKey.get(key) ?? null;

    return { key, kind: reading ? reading.kind : "level", reading };
  });
}
