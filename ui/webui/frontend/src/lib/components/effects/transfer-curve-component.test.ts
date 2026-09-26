import { describe, expect, it } from "vitest";
import { render } from "svelte/server";
import type { Visual } from "../../effect-types";
import TransferCurve from "./TransferCurve.svelte";

// The component is a data display, so it is rendered to HTML and asserted on
// the output: no DOM is needed to prove that a degraded input does not throw
// and does not leak a NaN coordinate into the SVG.

function visual(over: Partial<Visual> = {}): Visual {
  return {
    kind: "transfer",
    params: ["threshold", "ratio", "makeup"],
    overlays: ["in", "gr"],
    xMin: -60,
    xMax: 0,
    yMin: -60,
    yMax: 0,
    ...over,
  };
}

function renderCurve(props: {
  visual?: Visual;
  values?: Record<string, unknown>;
  meters?: Record<string, number> | null;
}): string {
  const { body } = render(TransferCurve, {
    props: {
      visual: props.visual ?? visual(),
      values: props.values ?? { threshold: -18, ratio: 4, makeup: 0 },
      meters: props.meters === undefined ? null : props.meters,
    },
  });

  return body;
}

describe("TransferCurve degraded input", () => {
  it("renders with null meters and no live markers", () => {
    const body = renderCurve({ meters: null });
    expect(body).toContain("Compressor transfer curve");
    expect(body).not.toContain("text-danger");
  });

  it("does not throw and does not emit NaN when a meter value is non-finite", () => {
    const body = renderCurve({ meters: { in: Number.NaN, gr: Number.POSITIVE_INFINITY } });
    expect(body).not.toContain("NaN");
    expect(body).not.toContain("Infinity");
    // A bad reading is treated as absent, so no live dot is drawn.
    expect(body).not.toContain("text-danger");
  });
});

describe("TransferCurve bad range", () => {
  it("draws an empty plot rather than collapsing onto an edge", () => {
    const body = renderCurve({
      visual: visual({ yMax: Number.POSITIVE_INFINITY }),
      meters: { in: -12, gr: -3 },
    });
    expect(body).not.toContain("NaN");
    expect(body).not.toContain("Infinity");
    // The curve path is empty, so there is no misleading flat line and no dot.
    expect(body).toContain('d=""');
    expect(body).not.toContain("text-danger");
  });

  it("does not throw for any non-finite or zero range end", () => {
    const ends: Array<Partial<Visual>> = [
      { xMin: Number.NaN },
      { xMax: Number.NEGATIVE_INFINITY },
      { yMin: Number.POSITIVE_INFINITY },
      { yMax: Number.NaN },
      { xMax: -60 },
      { yMax: -60 },
    ];
    for (const over of ends) {
      const body = renderCurve({ visual: visual(over), meters: { in: -12 } });
      expect(body).not.toContain("NaN");
      expect(body).not.toContain("Infinity");
    }
  });
});

describe("TransferCurve reads only the declared params", () => {
  it("does not apply a knee for a visual whose params omit it", () => {
    // The effect carries a knee key in values, but the visual did not declare
    // it, so the drawn curve is the hard-knee law.
    const body = renderCurve({
      visual: visual({ params: ["threshold", "ratio", "makeup"] }),
      values: { threshold: -18, ratio: 4, makeup: 0, knee: 6 },
    });
    expect(body).not.toContain("knee");
    expect(body).toContain("Compressor transfer curve: threshold -18 dB, ratio 4:1");
  });

  it("draws a unity curve when the threshold key is renamed", () => {
    const body = renderCurve({
      visual: visual({ params: ["level", "ratio"] }),
      values: { level: -18, ratio: 4 },
    });
    expect(body).toContain("Compressor transfer curve: threshold 0 dB, ratio 4:1");
  });

  it("names a declared knee in the label", () => {
    const body = renderCurve({
      visual: visual({ params: ["threshold", "ratio", "makeup", "knee"] }),
      values: { threshold: -18, ratio: 4, makeup: 0, knee: 6 },
    });
    expect(body).toContain("knee 6 dB");
  });
});
