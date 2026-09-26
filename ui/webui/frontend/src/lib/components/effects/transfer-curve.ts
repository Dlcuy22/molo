// Pure maths for the compressor transfer plot. It lives in a .ts module rather
// than in the component so the law is unit-testable without a DOM, and so the
// param names the curve reads have exactly one definition.

/**
 * Params the transfer law reads. The visual names the schema keys, so a role it
 * does not declare is neutral rather than an error: a curve must still draw for
 * an effect that declares fewer params, which is why every field carries a
 * neutral that leaves the curve at unity in and out.
 */
export interface TransferParams {
  /** Threshold in dB. Neutral 0 means no meaningful corner. */
  threshold: number;
  ratio: number;
  makeup: number;
  /** Knee width in dB, centred on the threshold. 0 is a hard corner. */
  knee?: number;
}

/** Neutral defaults for a param the effect did not declare or publish. 1:1 with
 *  no makeup draws the unity diagonal, which is honest about having no curve
 *  information rather than inventing a compression that is not there. */
export const NEUTRAL_THRESHOLD = 0;
export const NEUTRAL_RATIO = 1;
export const NEUTRAL_MAKEUP = 0;
export const NEUTRAL_KNEE = 0;

const MIN_RATIO = 1;
const MIN_KNEE = 0;

/**
 * readParam pulls a param from a values map, keyed by the schema key the
 * visual named. A value can arrive as a number-like string over the bridge, so
 * a numeric string is accepted; anything else, a NaN, an infinity, a missing
 * key, a boolean, falls back to the neutral. The fallback is not a guess at
 * what the effect meant, it is the value that keeps the curve drawable.
 */
export function readParam(
  values: Record<string, unknown>,
  key: string,
  fallback: number,
): number {
  const raw = values[key];
  if (typeof raw === "number" && Number.isFinite(raw)) {
    return raw;
  }
  if (typeof raw === "string" && raw.trim() !== "") {
    const n = Number(raw);
    if (Number.isFinite(n)) {
      return n;
    }
  }

  return fallback;
}

/**
 * resolveParams reads the roles the visual declared out of a values map. The
 * visual names the schema keys the curve is derived from, so this is the
 * contract: a role is read only when its key appears in `params`. A role the
 * visual did not declare is neutral (threshold 0, ratio 1, makeup 0, knee 0),
 * not guessed from a same-named value the effect may carry for another purpose.
 * That keeps the drawn curve to exactly what the effect declared; a visual
 * whose params omit the knee draws a hard knee even when values has a knee key.
 */
export function resolveParams(
  values: Record<string, unknown>,
  params: string[],
): TransferParams {
  const declared = new Set(params);

  return {
    threshold: declared.has("threshold")
      ? readParam(values, "threshold", NEUTRAL_THRESHOLD)
      : NEUTRAL_THRESHOLD,
    ratio: declared.has("ratio") ? readParam(values, "ratio", NEUTRAL_RATIO) : NEUTRAL_RATIO,
    makeup: declared.has("makeup") ? readParam(values, "makeup", NEUTRAL_MAKEUP) : NEUTRAL_MAKEUP,
    knee: declared.has("knee") ? readParam(values, "knee", NEUTRAL_KNEE) : NEUTRAL_KNEE,
  };
}

/**
 * transferSample maps one input level in dBFS to one output level in dBFS,
 * before any plot scaling.
 *
 * The law is the standard feed-forward compressor in the dB domain:
 *
 *   below the threshold, gain is unity, so y = x + makeup.
 *   above it, the input rises by `ratio` while the output rises by 1, so
 *   y = threshold + makeup + (x - threshold) / ratio.
 *
 * A knee softens the corner by interpolating the slope across a band of width
 * `knee` dB centred on the threshold, the usual quadratic soft-knee. Without
 * it the two straight segments meet at a hard angle, which is what the plot is
 * meant to show when the effect declares no knee.
 *
 * Ratio is clamped to at least 1 (a ratio below 1 would expand, not compress)
 * and the knee to at least 0, so a stray value cannot invert the curve.
 */
export function transferSample(x: number, p: TransferParams): number {
  const threshold = p.threshold;
  const ratio = Math.max(MIN_RATIO, p.ratio);
  const knee = Math.max(MIN_KNEE, p.knee ?? NEUTRAL_KNEE);
  const makeup = p.makeup;

  if (knee <= 0) {
    return x <= threshold
      ? x + makeup
      : threshold + (x - threshold) / ratio + makeup;
  }

  const half = knee / 2;
  const d = x - threshold;
  // The quadratic is the one whose value and slope match unity at the lower
  // edge and 1/ratio at the upper edge, so the curve stays continuous in both
  // directions across the joint.
  if (d <= -half) {
    return x + makeup;
  }
  if (d >= half) {
    return threshold + d / ratio + makeup;
  }

  const eased = (1 / ratio - 1) * ((d + half) * (d + half)) / (2 * knee);

  return x + eased + makeup;
}

/**
 * transferPoints samples the law across the plot's input range. The range is
 * the visual's own x range, so an effect that draws -60..0 gets a path that
 * spans exactly the plot; the sampler itself never assumes that range.
 *
 * `steps` is the number of segments, so the returned list is one point longer.
 * The caller maps dB to pixels: keeping this in dB means the maths does not
 * know the SVG, and the test asserts levels rather than coordinates.
 */
export function transferPoints(
  p: TransferParams,
  xMin: number,
  xMax: number,
  steps: number,
): { x: number; y: number }[] {
  const out: { x: number; y: number }[] = [];
  if (!Number.isFinite(xMin) || !Number.isFinite(xMax) || xMax === xMin) {
    return out;
  }
  const n = Math.max(1, Math.floor(steps));
  const lo = Math.min(xMin, xMax);
  const hi = Math.max(xMin, xMax);
  for (let i = 0; i <= n; i++) {
    const x = lo + ((hi - lo) * i) / n;
    out.push({ x, y: transferSample(x, p) });
  }

  return out;
}

/**
 * gainReduction is how far the curve pulls the signal down at one input level,
 * in dB (0 when not compressing). It is the plot's live `gr` overlay as the law
 * sees it, and the sane way to place the marker without a second meter: the dot
 * already gives the input, so the reduction is transfer(x) - (x + makeup).
 */
export function gainReduction(x: number, p: TransferParams): number {
  return transferSample(x, p) - (x + p.makeup);
}

/** label formats the curve's declared shape for the accessible name. A ratio
 *  renders as N:1, the way a compressor is spoken about. */
export function transferLabel(p: TransferParams): string {
  const ratioText = Number.isInteger(p.ratio) ? `${p.ratio}:1` : `${p.ratio.toFixed(1)}:1`;
  const parts = [`threshold ${trim(p.threshold)} dB`, `ratio ${ratioText}`];
  if (p.makeup !== 0) {
    parts.push(`makeup ${trim(p.makeup)} dB`);
  }
  if ((p.knee ?? 0) > 0) {
    parts.push(`knee ${trim(p.knee ?? 0)} dB`);
  }

  return parts.join(", ");
}

/**
 * trim renders a level without a trailing ".0", so "-18" not "-18.0". The
 * hyphen is the ASCII one, so the label stays byte-for-byte with the sign the
 * app's other number formats use.
 */
function trim(n: number): string {
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}
