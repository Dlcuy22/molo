import { describe, expect, it } from "vitest";
import type { Reading } from "../../effect-types";
import {
  createRing,
  graphSignature,
  levelToY,
  meterValue,
  plotX,
  push,
  resolveSeries,
  series,
  type Ring,
} from "./dynamics-graph";

function reading(over: Partial<Reading>): Reading {
  return {
    key: "k",
    label: "K",
    unit: "dB",
    min: -60,
    max: 0,
    kind: "level",
    ...over,
  };
}

describe("ring buffer", () => {
  it("keeps insertion order before it is full", () => {
    const ring = createRing(4);
    push(ring, 1);
    push(ring, 2);
    push(ring, 3);
    expect(series(ring)).toEqual([1, 2, 3]);
  });

  it("drops the oldest once it is full", () => {
    const ring = createRing(3);
    for (const v of [1, 2, 3, 4, 5]) {
      push(ring, v);
    }
    expect(series(ring)).toEqual([3, 4, 5]);
  });

  it("stays oldest-to-newest after it wraps", () => {
    // 5 pushes into a capacity-4 ring wrap the cursor once; the newest value
    // must still land at the right edge, so the stored array is rotated.
    const ring = createRing(4);
    for (const v of [1, 2, 3, 4, 5]) {
      push(ring, v);
    }
    expect(series(ring)).toEqual([2, 3, 4, 5]);

    push(ring, 6);
    expect(series(ring)).toEqual([3, 4, 5, 6]);
  });

  it("keeps a hole as null and a real zero as zero", () => {
    const ring = createRing(3);
    push(ring, 0);
    push(ring, null);
    push(ring, Number.NaN);
    expect(series(ring)).toEqual([0, null, null]);
  });

  it("clamps a capacity below one to a usable size", () => {
    const ring = createRing(0);
    push(ring, 7);
    expect(series(ring)).toEqual([7]);
  });

  it("hands back a copy, not the internal array", () => {
    const ring = createRing(2);
    push(ring, 1);
    const out = series(ring);
    out.push(99);
    expect(series(ring)).toEqual([1]);
  });

  it("does not leak a reference to a caller-held ring", () => {
    const ring: Ring = createRing(2);
    push(ring, 1);
    push(ring, 2);
    expect(series(ring)).toHaveLength(2);
  });
});

describe("levelToY", () => {
  it("puts the floor at the bottom and the ceiling at the top", () => {
    expect(levelToY(-60, -60, 0, 100)).toBe(100);
    expect(levelToY(0, -60, 0, 100)).toBe(0);
  });

  it("is linear across the range", () => {
    expect(levelToY(-30, -60, 0, 100)).toBeCloseTo(50, 10);
  });

  it("clamps a value past either edge onto the plot", () => {
    expect(levelToY(-120, -60, 0, 100)).toBe(100);
    expect(levelToY(12, -60, 0, 100)).toBe(0);
  });

  it("returns null for a bad range instead of a NaN", () => {
    expect(levelToY(-30, 0, 0, 100)).toBeNull();
    expect(levelToY(-30, 0, -60, 100)).toBeNull();
    expect(levelToY(-30, Number.NaN, 0, 100)).toBeNull();
    expect(levelToY(-30, 0, Number.POSITIVE_INFINITY, 100)).toBeNull();
  });

  it("returns null for a non-finite value", () => {
    expect(levelToY(Number.NaN, -60, 0, 100)).toBeNull();
    expect(levelToY(Number.POSITIVE_INFINITY, -60, 0, 100)).toBeNull();
  });
});

describe("plotX", () => {
  it("anchors the newest sample to the right edge", () => {
    expect(plotX(4, 5, 100, 5)).toBe(100);
    expect(plotX(0, 1, 100, 180)).toBe(100);
  });

  it("puts the oldest at the left once the ring is full", () => {
    expect(plotX(0, 5, 100, 5)).toBe(0);
  });

  it("spreads a full ring evenly across the width", () => {
    expect(plotX(2, 5, 100, 5)).toBeCloseTo(50, 10);
  });

  it("stays anchored to the right while priming", () => {
    // Length 1 and length 5 into a capacity-180 window both keep the newest at
    // the edge and grow leftward by one window step, not by the drawn width.
    expect(plotX(0, 1, 180, 180)).toBe(180);
    expect(plotX(4, 5, 180, 180)).toBeCloseTo(180, 10);
    expect(plotX(0, 5, 180, 180)).toBeCloseTo(180 - (4 * 180) / 179, 10);
  });

  it("keeps every priming sample inside the plot", () => {
    for (let i = 0; i < 5; i++) {
      const x = plotX(i, 5, 180, 180);
      expect(x).toBeGreaterThanOrEqual(0);
      expect(x).toBeLessThanOrEqual(180);
    }
  });

  it("puts every sample on the right edge when capacity is below one", () => {
    expect(plotX(0, 3, 100, 0)).toBe(100);
    expect(plotX(2, 3, 100, 0)).toBe(100);
  });
});

describe("meterValue", () => {
  it("reads one key out of the map", () => {
    expect(meterValue({ out: -12 }, "out")).toBe(-12);
  });

  it("is null for an absent key, a bad value, or no map", () => {
    expect(meterValue({}, "out")).toBeNull();
    expect(meterValue({ out: Number.NaN }, "out")).toBeNull();
    expect(meterValue({ out: Number.POSITIVE_INFINITY }, "out")).toBeNull();
    expect(meterValue(null, "out")).toBeNull();
    expect(meterValue(undefined, "out")).toBeNull();
  });

  it("keeps a real zero distinct from an absent reading", () => {
    expect(meterValue({ out: 0 }, "out")).toBe(0);
  });
});

describe("resolveSeries", () => {
  it("uses the declared reading's kind", () => {
    const out = resolveSeries(
      ["in", "gr", "freq"],
      [
        reading({ key: "in", kind: "level" }),
        reading({ key: "gr", kind: "gain-reduction", min: -24, max: 0 }),
        reading({ key: "freq", kind: "scalar", min: 0, max: 1000 }),
      ],
    );
    expect(out.map((s) => s.kind)).toEqual(["level", "gain-reduction", "scalar"]);
  });

  it("defaults a key with no reading to level", () => {
    // The standard In/Out pair is not always declared, so an undeclared overlay
    // is a level rather than a guess at another kind.
    const out = resolveSeries(["in", "out"], []);
    expect(out.map((s) => s.kind)).toEqual(["level", "level"]);
    expect(out[0].reading).toBeNull();
  });

  it("follows the overlay order and carries the reading", () => {
    const gr = reading({ key: "gr", kind: "gain-reduction" });
    const out = resolveSeries(["gr", "in"], [gr]);
    expect(out.map((s) => s.key)).toEqual(["gr", "in"]);
    expect(out[0].reading).toBe(gr);
  });
});

describe("graphSignature", () => {
  const series = resolveSeries(["in", "out", "gr"], [reading({ key: "gr", kind: "gain-reduction" })]);

  it("is stable when a re-created series has equal contents", () => {
    // The panel rebuilds readings and visuals every meter tick; a rebuilt but
    // equal series must not restart the draw loop.
    const rebuilt = resolveSeries(
      ["in", "out", "gr"],
      [reading({ key: "gr", kind: "gain-reduction" })],
    );
    expect(graphSignature("s1", ["in", "out", "gr"], rebuilt, -60, 0, -60, 0)).toBe(
      graphSignature("s1", ["in", "out", "gr"], series, -60, 0, -60, 0),
    );
  });

  it("changes when the stage changes", () => {
    const a = graphSignature("s1", ["in"], series, -60, 0, -60, 0);
    const b = graphSignature("s2", ["in"], series, -60, 0, -60, 0);
    expect(a).not.toBe(b);
  });

  it("changes when an overlay or its kind changes", () => {
    const base = graphSignature("s1", ["in"], series, -60, 0, -60, 0);
    expect(graphSignature("s1", ["out"], series, -60, 0, -60, 0)).not.toBe(base);
    const asLevel = resolveSeries(["gr"], [reading({ key: "gr", kind: "level" })]);
    const asReduction = resolveSeries(["gr"], [reading({ key: "gr", kind: "gain-reduction" })]);
    expect(graphSignature("s1", ["gr"], asLevel, -60, 0, -60, 0)).not.toBe(
      graphSignature("s1", ["gr"], asReduction, -60, 0, -60, 0),
    );
  });

  it("changes when the scale changes", () => {
    const base = graphSignature("s1", ["in"], series, -60, 0, -60, 0);
    expect(graphSignature("s1", ["in"], series, -48, 0, -60, 0)).not.toBe(base);
    expect(graphSignature("s1", ["in"], series, -60, 0, -48, 0)).not.toBe(base);
  });
});
