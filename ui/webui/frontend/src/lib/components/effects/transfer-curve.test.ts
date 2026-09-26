import { describe, expect, it } from "vitest";
import {
  NEUTRAL_KNEE,
  NEUTRAL_MAKEUP,
  NEUTRAL_RATIO,
  NEUTRAL_THRESHOLD,
  gainReduction,
  readParam,
  resolveParams,
  transferLabel,
  transferPoints,
  transferSample,
  type TransferParams,
} from "./transfer-curve";

// A realistic compressor: -18 dB threshold, 4:1, a hard knee unless stated.
const comp: TransferParams = { threshold: -18, ratio: 4, makeup: 0, knee: 0 };

describe("transferSample below the threshold", () => {
  it("is unity with makeup added", () => {
    expect(transferSample(-30, comp)).toBe(-30);
    expect(transferSample(-50, { threshold: -18, ratio: 4, makeup: 6 })).toBe(-44);
    expect(transferSample(-18, comp)).toBe(-18);
  });

  it("does not compress the quietest input", () => {
    expect(transferSample(-60, comp)).toBe(-60);
  });
});

describe("transferSample above the threshold", () => {
  it("rises by 1/ratio of the input above the threshold", () => {
    // 12 dB above a 4:1 threshold adds 3 dB.
    expect(transferSample(-6, comp)).toBeCloseTo(-15, 10);
    expect(transferSample(0, comp)).toBeCloseTo(-13.5, 10);
  });

  it("adds makeup on top of the compressed segment", () => {
    const withMakeup: TransferParams = { threshold: -18, ratio: 4, makeup: 6 };
    expect(transferSample(-6, withMakeup)).toBeCloseTo(-9, 10);
  });

  it("is monotonic: more in never means less out", () => {
    let prev = -Infinity;
    for (let x = -60; x <= 0; x += 0.5) {
      const y = transferSample(x, comp);
      expect(y).toBeGreaterThanOrEqual(prev);
      prev = y;
    }
  });

  it("is unity everywhere at ratio 1", () => {
    const unity: TransferParams = { threshold: -18, ratio: 1, makeup: 0 };
    expect(transferSample(-6, unity)).toBe(-6);
    expect(transferSample(0, unity)).toBe(0);
  });

  it("never expands when a ratio below 1 drifts in", () => {
    // A ratio under 1 would expand; clamping to 1 makes it the unity line, so
    // full-scale 0 dB in gives 0 dB out rather than a gain above unity.
    const bad: TransferParams = { threshold: -18, ratio: 0.25, makeup: 0 };
    expect(transferSample(0, bad)).toBe(0);
  });
});

describe("transferSample with a knee", () => {
  const soft: TransferParams = { threshold: -18, ratio: 4, makeup: 0, knee: 6 };

  it("stays continuous at both edges of the knee", () => {
    // Lower edge: -21, upper edge: -15.
    expect(transferSample(-21, soft)).toBeCloseTo(-21, 10);
    expect(transferSample(-15, soft)).toBeCloseTo(-17.25, 10);
  });

  it("softens the corner: it bends before the threshold, the hard law does not", () => {
    // At the threshold the hard law is still exactly on the unity diagonal;
    // the soft law has already started into gain reduction, which is the point
    // of a knee: no sharp corner to hear.
    expect(transferSample(-18, comp)).toBe(-18);
    expect(transferSample(-18, soft)).toBeLessThan(-18);
  });

  it("leaves the hard law untouched outside the knee band", () => {
    expect(transferSample(-30, soft)).toBe(transferSample(-30, comp));
    expect(transferSample(0, soft)).toBeCloseTo(transferSample(0, comp), 10);
  });

  it("is still monotonic", () => {
    let prev = -Infinity;
    for (let x = -60; x <= 0; x += 0.5) {
      const y = transferSample(x, soft);
      expect(y).toBeGreaterThanOrEqual(prev);
      prev = y;
    }
  });

  it("treats a zero knee as a hard knee", () => {
    const zero: TransferParams = { threshold: -18, ratio: 4, makeup: 0, knee: 0 };
    for (let x = -21; x <= -15; x += 0.5) {
      expect(transferSample(x, zero)).toBe(transferSample(x, comp));
    }
  });
});

describe("resolveParams is defensive", () => {
  it("reads the declared keys", () => {
    expect(
      resolveParams({ threshold: -12, ratio: 3, knee: 4, makeup: 2 }, [
        "threshold",
        "ratio",
        "knee",
        "makeup",
      ]),
    ).toEqual({
      threshold: -12,
      ratio: 3,
      makeup: 2,
      knee: 4,
    });
  });

  it("falls back to a neutral for a missing, non-numeric or non-finite value", () => {
    const p = resolveParams(
      {
        threshold: "not a number",
        ratio: Number.NaN,
        makeup: Number.POSITIVE_INFINITY,
        // knee absent
      },
      ["threshold", "ratio", "makeup", "knee"],
    );
    expect(p.threshold).toBe(NEUTRAL_THRESHOLD);
    expect(p.ratio).toBe(NEUTRAL_RATIO);
    expect(p.makeup).toBe(NEUTRAL_MAKEUP);
    expect(p.knee).toBe(0);
  });

  it("does not read a role the visual did not declare, even when values carries it", () => {
    // The visual names the keys the curve is derived from. A knee key in values
    // is not a knee unless the visual declared it, so the drawn curve is the
    // hard-knee law the effect asked for.
    const p = resolveParams({ threshold: -18, ratio: 4, knee: 6 }, ["threshold", "ratio"]);
    expect(p.knee).toBe(NEUTRAL_KNEE);
    expect(transferSample(-18, p)).toBe(-18);
    expect(transferSample(-18, { ...p, knee: 6 })).toBeLessThan(-18);
  });

  it("draws unity when a renamed threshold key leaves the role undeclared", () => {
    // A visual that names some other key for its corner contributes no
    // threshold, so the curve stays on the unity diagonal rather than guessing.
    const p = resolveParams({ level: -18, ratio: 4 }, ["level", "ratio"]);
    expect(p.threshold).toBe(NEUTRAL_THRESHOLD);
    expect(transferSample(-30, p)).toBe(-30);
    expect(transferSample(0, p)).toBe(0);
  });

  it("accepts a numeric string, which is how a value can cross the bridge", () => {
    expect(readParam({ threshold: "-18" }, "threshold", 0)).toBe(-18);
  });

  it("draws the unity diagonal when nothing is declared", () => {
    const p = resolveParams({}, []);
    expect(transferSample(-30, p)).toBe(-30);
    expect(transferSample(0, p)).toBe(0);
  });
});

describe("transferPoints", () => {
  it("spans the range with one more point than steps", () => {
    const pts = transferPoints(comp, -60, 0, 4);
    expect(pts).toHaveLength(5);
    expect(pts[0].x).toBe(-60);
    expect(pts[pts.length - 1].x).toBe(0);
  });

  it("samples the law at every point", () => {
    const pts = transferPoints(comp, -60, 0, 2);
    expect(pts.map((p) => p.y)).toEqual([-60, transferSample(-30, comp), transferSample(0, comp)]);
  });

  it("returns nothing for a degenerate or non-finite range", () => {
    expect(transferPoints(comp, -60, -60, 10)).toEqual([]);
    expect(transferPoints(comp, -60, Number.NaN, 10)).toEqual([]);
  });
});

describe("gainReduction", () => {
  it("is zero below the threshold", () => {
    expect(gainReduction(-30, comp)).toBe(0);
  });

  it("is the amount the curve pulls the signal down above the threshold", () => {
    // 12 dB over a 4:1 threshold: 3 dB out minus 12 dB in is a 9 dB reduction.
    expect(gainReduction(-6, comp)).toBeCloseTo(-9, 10);
  });
});

describe("transferLabel", () => {
  it("summarises the shape for the accessible name", () => {
    expect(transferLabel(comp)).toBe("threshold -18 dB, ratio 4:1");
  });

  it("names the makeup and knee when they are set", () => {
    expect(transferLabel({ threshold: -18, ratio: 4, makeup: 6, knee: 6 })).toBe(
      "threshold -18 dB, ratio 4:1, makeup 6 dB, knee 6 dB",
    );
  });

  it("renders a non-integer ratio to one decimal", () => {
    expect(transferLabel({ threshold: -12, ratio: 2.5, makeup: 0, knee: 0 })).toBe(
      "threshold -12 dB, ratio 2.5:1",
    );
  });
});
