// Pure helpers the effect UI leans on. They live in a .ts module rather than in
// a component so the split rules are unit-testable without a DOM, and so the
// base/effect boundary has exactly one definition.

import { fillFrac } from "../../format";
import {
  ParamKind,
  effectiveWidget,
  groupParams,
  isCommonParam,
  meterDB,
  paramLabel,
  type EffectParam,
  type EffectStage,
  type ParamGroup,
} from "../../effect-types";

/**
 * knobBankMin is how many equal knobs a group needs before it is laid out as a
 * grid rather than a column. Two knobs side by side would read as a ranked
 * pair, the way a stereo pair of sliders does; a bank is about a dozen.
 */
export const KNOB_BANK_MIN = 3;

/**
 * isKnobBank reports whether a group is a bank: a set of knobs that differ only
 * in their key, value and label. That is what a graphic EQ, a stereo pair or a
 * matrix is, and they all want the same width, so the grid's columns line up and
 * the row order (low frequency to high) is the reading order.
 *
 * A group holding any other control stays a column: mixing a slider in would
 * make the grid size that control's cell badly, and a slider is a wide control.
 */
export function isKnobBank(params: EffectParam[]): boolean {
  return (
    params.length >= KNOB_BANK_MIN &&
    params.every(
      (p) =>
        effectiveWidget(p) === "knob" &&
        p.kind !== ParamKind.Bool &&
        p.kind !== ParamKind.Enum,
    )
  );
}

/** clamp is used by the level meter and by the numeric coercion below. */
export function clamp(v: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, v));
}

/**
 * commonParam finds one of the standard base controls in a stage schema. It is
 * a read, not a fallback: the engine always prepends the three, but looking the
 * param up by key keeps the panel from inventing a shape if a schema is ever
 * partial.
 */
export function commonParam(
  stage: EffectStage,
  key: string,
): EffectParam | null {
  return stage.schema.find((p) => p.key === key) ?? null;
}

/**
 * effectGroups is groupParams with the standard params (and the standard gain
 * group when it holds nothing else) removed, so the panel's effect section
 * shows only what the effect declared.
 */
export function effectGroups(schema: EffectParam[]): ParamGroup[] {
  const out: ParamGroup[] = [];
  for (const group of groupParams(schema)) {
    const params = group.params.filter((p) => !isCommonParam(p.key));
    if (params.length > 0) {
      out.push({ name: group.name, params });
    }
  }

  return out;
}

/**
 * paramValue reads a param's current value from a stage, falling back to the
 * declaration's default. The values map is keyed by param key, so this is just
 * a guarded lookup.
 */
export function paramValue(stage: EffectStage, p: EffectParam): unknown {
  const v = stage.values[p.key];

  return v === undefined || v === null ? p.default : v;
}

/** numericValue coerces a stored value for a range input. */
export function numericValue(v: unknown, fallback: number): number {
  if (typeof v === "number" && Number.isFinite(v)) {
    return v;
  }
  if (typeof v === "string" && v.trim() !== "") {
    const n = Number(v);
    if (Number.isFinite(n)) {
      return n;
    }
  }

  return fallback;
}

function trimFixed(f: number): string {
  if (!Number.isFinite(f)) {
    return "";
  }
  const text = f.toFixed(2).replace(/\.?0+$/, "");

  // "-0" reads as a real negative value at a glance; collapse it to "0".
  return text === "-0" || text === "-0.0" ? "0" : text;
}

/** formatValue renders a param's value for display: a Bool as On/Off, an Enum
 *  as its option text, a number with at most two decimals plus its unit. */
export function formatValue(p: EffectParam, v: unknown): string {
  const widget = effectiveWidget(p);
  if (widget === "switch" || p.kind === ParamKind.Bool) {
    return v ? "On" : "Off";
  }
  if (p.kind === ParamKind.Enum) {
    return typeof v === "string" ? v : "";
  }
  const n = numericValue(v, 0);
  const text = trimFixed(n);

  return p.unit !== "" ? `${text} ${p.unit}` : text;
}

/** isFiniteNumber reports whether a value is a real number, not a NaN or an
 *  infinity that drifted in over the bridge. */
export function isFiniteNumber(v: unknown): v is number {
  return typeof v === "number" && Number.isFinite(v);
}

function stepDecimals(step: number): number {
  if (!Number.isFinite(step) || step <= 0) {
    return 0;
  }
  const s = String(step);
  if (s.includes("e-")) {
    return Number(s.split("e-")[1]) || 0;
  }
  const dot = s.indexOf(".");

  return dot < 0 ? 0 : s.length - dot - 1;
}

/**
 * roundToStep snaps a number to the nearest step and rounds away the float
 * residue, so a 0.1 step does not produce 0.30000000000000004. The engine
 * clamps anyway; this only keeps the value the UI sends clean.
 */
export function roundToStep(v: number, step: number): number {
  if (!Number.isFinite(v)) {
    return 0;
  }
  if (!Number.isFinite(step) || step <= 0) {
    return v;
  }
  const snapped = Math.round(v / step) * step;
  const d = Math.min(6, stepDecimals(step));

  return Number(snapped.toFixed(d));
}

/**
 * coerceValue turns a raw control value into the typed value the engine wants:
 * number for Float and Int, boolean for Bool, string for Enum. It is the one
 * place that mapping lives, so a param cannot be emitted with the wrong type.
 */
export function coerceValue(p: EffectParam, raw: unknown): unknown {
  switch (p.kind) {
    case ParamKind.Bool:
      return Boolean(raw);
    case ParamKind.Enum:
      return typeof raw === "string" ? raw : String(raw ?? "");
    case ParamKind.Int:
      return Math.round(clamp(roundToStep(numericValue(raw, p.default as number), p.step), p.min, p.max));
    default:
      return clamp(roundToStep(numericValue(raw, p.default as number), p.step), p.min, p.max);
  }
}

/**
 * knobAngle maps a param value to an angle for the rotary indicator. It exists
 * as a helper rather than inline style math so the sweep is checkable: 270
 * degrees, from -135 up to +135, so the indicator never crosses the gap at the
 * bottom.
 */
export function knobAngle(v: number, min: number, max: number): number {
  return -135 + 270 * fillFrac(v, min, max);
}

/** meterFraction maps a dBFS level to the meter's 0..1 fill. A zero-width
 *  range is treated as empty rather than dividing by zero, which would peg the
 *  bar at full scale. */
export function meterFraction(
  db: number | null,
  min: number,
  max: number,
): number {
  if (db === null || !Number.isFinite(db) || max <= min) {
    return 0;
  }

  return clamp((db - min) / (max - min), 0, 1);
}

/** meterText renders a reading with its unit, or "--" when the effect does not
 *  meter at all. The unit defaults to dB for the legacy level meters, and a
 *  reading that carries its own unit passes it so a gain reduction, a percent
 *  or a frequency is never mislabelled. The placeholder matches formatTime's
 *  unknown-value convention rather than a dash glyph, which would read as a
 *  real (tiny) value. */
export function meterText(db: number | null, unit = "dB"): string {
  if (db === null || !Number.isFinite(db)) {
    return "--";
  }

  return unit === "" ? db.toFixed(1) : `${db.toFixed(1)} ${unit}`;
}

/**
 * finalIndex converts a drop gap in the current list into the final index the
 * engine's MoveEffect expects. The two differ by one when the dragged item sits
 * before the gap, because removing it first shifts everything after it left.
 * A gap at the very end lands on the last index. The result is clamped to the
 * list, so a drop past the end is a move to the end rather than an error.
 */
export function finalIndex(
  fromIndex: number,
  gap: number,
  length: number,
): number {
  if (length <= 0) {
    return 0;
  }
  const adjusted = gap - (fromIndex < gap ? 1 : 0);

  return clamp(adjusted, 0, length - 1);
}

/** indexOfStage finds a stage's position, or -1 when it is not in the list. */
export function indexOfStage(stages: { id: string }[], id: string): number {
  return stages.findIndex((s) => s.id === id);
}

export { effectiveWidget, isCommonParam, meterDB, paramLabel };