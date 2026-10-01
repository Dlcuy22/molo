import { describe, expect, it } from "vitest";
import { baseName, displayTitle, fillFrac, formatTime, isRemoteRef } from "./format";

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

// A provider reference is an identifier, not a path. If it leaked into the
// display helpers, the UI would show "ytm:abc123" as a track title.
describe("remote references", () => {
  it("are recognised by their scheme", () => {
    expect(isRemoteRef("ytm:abc123")).toBe(true);
    expect(isRemoteRef("/m/a.opus")).toBe(false);
    expect(isRemoteRef("")).toBe(false);
  });

  it("have no file name to fall back on", () => {
    expect(baseName("ytm:abc123")).toBe("");
    expect(displayTitle("", "ytm:abc123")).toBe("");
  });

  it("still show the tag when the track has one", () => {
    expect(displayTitle("A Song", "ytm:abc123")).toBe("A Song");
  });
});

// A YouTube Music search result carries its own duration. Zero is a real
// duration here, so the row only asks for a label when the catalogue gave one;
// the helper itself must not read 0 as unknown, because the transport shows
// "0:00" at the start of a track.
describe("formatTime", () => {
  it("renders an unknown duration as unknown, not as zero", () => {
    expect(formatTime(-1)).toBe("--:--");
    expect(formatTime(NaN)).toBe("--:--");
  });

  it("renders a real zero as zero", () => {
    expect(formatTime(0)).toBe("0:00");
  });

  it("renders m:ss, and h:mm:ss past an hour", () => {
    expect(formatTime(210000)).toBe("3:30");
    expect(formatTime(3_661_000)).toBe("1:01:01");
  });
});
