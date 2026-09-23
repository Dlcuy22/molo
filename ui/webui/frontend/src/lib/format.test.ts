import { describe, expect, it } from "vitest";
import { fillFrac } from "./format";

// The range fill style takes a 0..1 fraction and meets the thumb centre only
// when the fraction equals the value's position in [min, max]. These pin the
// mapping, including the slider cases in the visualizer panel.
describe("fillFrac", () => {
  it("maps the points slider value to its track position", () => {
    // Points 150 on [16, 256], the reported broken case.
    expect(fillFrac(150, 16, 256)).toBeCloseTo(134 / 240, 10);
  });

  it("maps the low cut slider value to its track position", () => {
    expect(fillFrac(20, 10, 500)).toBeCloseTo(10 / 490, 10);
  });

  it("pins the extremes so the fill meets the thumb at both ends", () => {
    expect(fillFrac(10, 10, 500)).toBe(0);
    expect(fillFrac(500, 10, 500)).toBe(1);
    expect(fillFrac(0, 0, 100)).toBe(0);
    expect(fillFrac(100, 0, 100)).toBe(1);
  });

  it("clamps wayward values instead of pushing past the thumb", () => {
    expect(fillFrac(-5, 0, 100)).toBe(0);
    expect(fillFrac(150, 0, 100)).toBe(1);
  });

  it("returns 0 for a zero span or non-finite input", () => {
    expect(fillFrac(50, 100, 100)).toBe(0);
    expect(fillFrac(50, 100, 50)).toBe(0);
    expect(fillFrac(NaN, 0, 100)).toBe(0);
    expect(fillFrac(50, 0, NaN)).toBe(0);
  });
});
