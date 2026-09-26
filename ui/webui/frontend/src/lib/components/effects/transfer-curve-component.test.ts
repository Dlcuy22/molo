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

describe("TransferCurve scales uniformly", () => {
  // The regression: the plot used a 1-unit viewBox stretched with
  // preserveAspectRatio="none" and relied on vector-effect="non-scaling-stroke"
  // to keep strokes thin. WebKitGTK (the desktop webview) does not apply that
  // attribute, so stroke-width 2 became ~1560px and the whole plot rendered as
  // a solid rectangle. The fix is a fixed viewBox scaled uniformly by
  // aspect-ratio, so no stroke depends on vector-effect.
  it("does not rely on vector-effect to keep strokes from exploding", () => {
    const body = renderCurve({ meters: { in: -12, gr: -3 } });
    expect(body).not.toContain("non-scaling-stroke");
    expect(body).not.toContain("vector-effect");
  });

  it("does not stretch the coordinate system with preserveAspectRatio", () => {
    const body = renderCurve({});
    expect(body).not.toContain("preserveAspectRatio");
  });

  it("keeps every stroke width small in a pixel-sized viewBox", () => {
    // With a ~1000-unit box a sane stroke is a handful of units; the old 1-unit
    // box is what made a width of 2 fill the plot. A width past a few percent of
    // the box means the unit-square bug has returned.
    const body = renderCurve({});
    const widths = [...body.matchAll(/stroke-width="([0-9.]+)"/g)].map((m) => Number(m[1]));
    expect(widths.length).toBeGreaterThan(0);
    for (const w of widths) {
      expect(w).toBeLessThanOrEqual(8);
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
