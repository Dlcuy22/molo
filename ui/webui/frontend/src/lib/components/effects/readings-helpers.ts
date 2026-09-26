// Unit-testable without a DOM; the gain-reduction inversion has one definition.

import { clamp, meterFraction } from "./effects-helpers";
import type { Reading } from "../../effect-types";

/**
 * A missing key or a non-finite value is null: an absent reading must stay
 * distinct from a real zero.
 */
export function readingValue(
  meters: Record<string, number> | null,
  key: string,
): number | null {
  const v = meters?.[key];

  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/**
 * Maps a value onto its bar's 0..1 fill using the reading's own range. A level
 * or scalar fills from the floor up; a gain-reduction reading (a negative dB
 * where 0 is no reduction) is inverted, so the bar is full at 0 and shrinks as
 * gain is removed. A null value is a null fraction, drawn as an inert row.
 */
export function readingFraction(
  value: number | null,
  reading: Reading,
): number | null {
  if (value === null || !Number.isFinite(value)) {
    return null;
  }
  if (reading.kind === "gain-reduction") {
    const span = reading.max - reading.min;
    if (span <= 0) {
      return meterFraction(value, reading.min, reading.max);
    }
    const reduction = reading.max - value;

    return clamp(1 - reduction / span, 0, 1);
  }

  return meterFraction(value, reading.min, reading.max);
}
