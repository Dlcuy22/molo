import { describe, expect, it } from "vitest";
import type { Reading } from "../../effect-types";
import { readingFraction, readingValue } from "./readings-helpers";

function reading(over: Partial<Reading>): Reading {
  return {
    key: "k",
    label: "K",
    unit: "dB",
    min: -30,
    max: 0,
    kind: "level",
    ...over,
  };
}

describe("readingValue", () => {
  it("reads one key out of the meters map", () => {
    expect(readingValue({ gr: -6, in: -12 }, "gr")).toBe(-6);
  });

  it("is null for a key the meters do not carry", () => {
    expect(readingValue({ in: -12 }, "gr")).toBeNull();
  });

  it("is null for a non-finite value and for no meters at all", () => {
    expect(readingValue({ gr: Number.NaN }, "gr")).toBeNull();
    expect(readingValue({ gr: Number.POSITIVE_INFINITY }, "gr")).toBeNull();
    expect(readingValue(null, "gr")).toBeNull();
  });

  it("keeps a real zero distinct from an absent reading", () => {
    // A zero is real and drawn; only null is the inert row.
    expect(readingValue({ gr: 0 }, "gr")).toBe(0);
    expect(readingValue({}, "gr")).toBeNull();
  });
});

// A reading draws on its own range, not the legacy -60..0 scale.
describe("readingFraction per-reading range", () => {
  it("maps a level on its own range", () => {
    const r = reading({ kind: "level", min: -24, max: 0 });
    expect(readingFraction(-24, r)).toBe(0);
    expect(readingFraction(0, r)).toBe(1);
    expect(readingFraction(-12, r)).toBeCloseTo(0.5, 10);
  });

  it("maps a scalar on its own range and unit", () => {
    const r = reading({ kind: "scalar", min: 0, max: 10, unit: "Hz" });
    expect(readingFraction(0, r)).toBe(0);
    expect(readingFraction(10, r)).toBe(1);
    expect(readingFraction(5, r)).toBeCloseTo(0.5, 10);
  });

  it("clamps a value past the edge of the range onto the bar", () => {
    const r = reading({ kind: "level", min: -24, max: 0 });
    expect(readingFraction(-99, r)).toBe(0);
    expect(readingFraction(99, r)).toBe(1);
  });

  it("is null for a missing reading, whatever the kind", () => {
    expect(readingFraction(null, reading({ kind: "level" }))).toBeNull();
    expect(readingFraction(Number.NaN, reading({ kind: "scalar" }))).toBeNull();
  });
});

// A gain reduction is a negative dB, so a floor-up fill would read backwards;
// the inverted fraction makes the bar full at no reduction.
describe("readingFraction gain-reduction inversion", () => {
  const gr = reading({ kind: "gain-reduction", min: -30, max: 0 });

  it("is full at zero reduction", () => {
    expect(readingFraction(0, gr)).toBe(1);
  });

  it("is empty at the bottom of the range", () => {
    expect(readingFraction(-30, gr)).toBe(0);
  });

  it("shrinks as the reduction grows, inverted against a magnitude bar", () => {
    const light = readingFraction(-6, gr) as number;
    const heavy = readingFraction(-18, gr) as number;
    expect(light).toBeCloseTo(0.8, 10);
    expect(heavy).toBeCloseTo(0.4, 10);
    expect(light).toBeGreaterThan(heavy);
  });

  it("is null before it is zero, so an unread meter stays inert", () => {
    expect(readingFraction(null, gr)).toBeNull();
    expect(readingFraction(Number.NaN, gr)).toBeNull();
  });
});
