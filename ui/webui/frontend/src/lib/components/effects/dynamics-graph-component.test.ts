import { describe, expect, it, vi } from "vitest";
import { render } from "svelte/server";
import type { Reading, Visual } from "../../effect-types";

// The store touches window at module load, which does not exist under
// svelte/server; the component only needs the meter subscription shape, so it
// is stubbed. onMount and $effect do not run in server rendering, so nothing
// here exercises the subscription, the rAF loop, or the teardown; that
// lifecycle is verified by inspection of the component, and the drawing maths
// is covered by the plotX tests in dynamics-graph.test.ts.
vi.mock("../../store", () => ({
  onEffectMeters: () => () => {},
}));

import DynamicsGraph from "./DynamicsGraph.svelte";

// Server-rendered to HTML only, to prove that degraded input does not throw and
// that the accessible summary reaches the output. The canvas and the mounted
// lifecycle are not observable here.

function visual(over: Partial<Visual> = {}): Visual {
  return {
    kind: "dynamics",
    params: [],
    overlays: ["in", "out", "gr"],
    xMin: 0,
    xMax: 3,
    yMin: -60,
    yMax: 0,
    ...over,
  };
}

const readings: Reading[] = [
  { key: "in", label: "In", unit: "dBFS", min: -60, max: 0, kind: "level" },
  { key: "out", label: "Out", unit: "dBFS", min: -60, max: 0, kind: "level" },
  { key: "gr", label: "Gain Reduction", unit: "dB", min: -24, max: 0, kind: "gain-reduction" },
];

function renderGraph(props: {
  visual?: Visual;
  readings?: Reading[];
  meters?: Record<string, number> | null;
}): string {
  const { body } = render(DynamicsGraph, {
    props: {
      visual: props.visual ?? visual(),
      readings: props.readings ?? readings,
      stageId: "stage-1",
      meters: props.meters === undefined ? null : props.meters,
    },
  });

  return body;
}

describe("DynamicsGraph degraded input", () => {
  it("renders with null meters and an empty ring", () => {
    const body = renderGraph({ meters: null });
    expect(body).toContain("Dynamics graph");
    expect(body).not.toContain("NaN");
    expect(body).not.toContain("Infinity");
  });

  it("does not throw when no series are declared", () => {
    const body = renderGraph({ visual: visual({ overlays: [] }) });
    expect(body).toContain("no series declared");
  });

  it("still renders an empty plot for a bad y range", () => {
    const body = renderGraph({ visual: visual({ yMax: Number.POSITIVE_INFINITY }) });
    expect(body).not.toContain("NaN");
    expect(body).not.toContain("Infinity");
    expect(body).toContain("Dynamics graph");
  });
});

describe("DynamicsGraph accessible summary", () => {
  it("names the series and their kinds", () => {
    const body = renderGraph({});
    expect(body).toContain("Dynamics graph, newest on the right");
    expect(body).toContain("gr (gain-reduction)");
  });

  it("hides the canvas from assistive tech", () => {
    const body = renderGraph({});
    expect(body).toContain('aria-hidden="true"');
  });
});
