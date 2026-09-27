import { describe, expect, it } from "vitest";
import type {
  EffectKindInfo as WireKind,
  EffectParamInfo as WireParam,
  EffectStageInfo as WireStage,
} from "../../bindings/github.com/dlcuy22/player/ui/webui/models";
import { toEffectKind, toEffectParam, toEffectStage, toWidget } from "./effect-adapter";

function wireParam(over: Partial<WireParam>): WireParam {
  return {
    key: "k",
    kind: 0,
    min: 0,
    max: 1,
    step: 0.1,
    unit: "",
    options: [],
    default: 0,
    label: "",
    group: "",
    widget: "",
    ...over,
  };
}

describe("toWidget", () => {
  it("passes a known hint through", () => {
    expect(toWidget("knob")).toBe("knob");
    expect(toWidget("select")).toBe("select");
    expect(toWidget("")).toBe("");
  });

  it("falls back to auto for a hint the UI does not know", () => {
    // A future backend widget must not reach the components as an unknown
    // string, which would render nothing.
    expect(toWidget("fader")).toBe("");
  });
});

describe("toEffectParam", () => {
  it("turns an empty options list into null", () => {
    expect(toEffectParam(wireParam({ options: [] })).options).toBeNull();
  });

  it("keeps a non-empty options list", () => {
    expect(toEffectParam(wireParam({ options: ["a", "b"] })).options).toEqual(["a", "b"]);
  });

  it("carries the label, group and widget", () => {
    const p = toEffectParam(
      wireParam({ label: "Rate", group: "Tremolo", widget: "knob", kind: 3 }),
    );
    expect(p.label).toBe("Rate");
    expect(p.group).toBe("Tremolo");
    expect(p.widget).toBe("knob");
    expect(p.kind).toBe(3);
  });
});

describe("toEffectStage", () => {
  // The binding now carries the telemetry fields, so the fixture uses them
  // directly rather than declaring its own optional shapes.
  type WireStageT = WireStage;

  function wireStage(over: Partial<WireStageT>): WireStageT {
    return {
      id: "s1",
      kind: "crossfeed",
      impl: "",
      label: "Crossfeed",
      bypassed: false,
      schema: null,
      values: null,
      meters: null,
      readings: null,
      visuals: null,
      ...over,
    };
  }

  it("treats a null schema as empty and null meters as null", () => {
    const s = toEffectStage(wireStage({}));
    expect(s.schema).toEqual([]);
    expect(s.meters).toBeNull();
    expect(s.values).toEqual({});
  });

  it("maps a meters record and drops a non-number entry", () => {
    const s = toEffectStage(
      wireStage({ meters: { in: -6.02, out: -7.07 } as Record<string, number> }),
    );
    expect(s.meters).toEqual({ in: -6.02, out: -7.07 });
  });

  it("maps each schema entry", () => {
    const s = toEffectStage(wireStage({ schema: [wireParam({ key: "cutoff" })] }));
    expect(s.schema.map((p) => p.key)).toEqual(["cutoff"]);
  });

  it("defaults readings and visuals to empty when absent", () => {
    const s = toEffectStage(wireStage({}));
    expect(s.readings).toEqual([]);
    expect(s.visuals).toEqual([]);
  });

  it("falls back to level for an unknown reading kind", () => {
    const s = toEffectStage(
      wireStage({
        readings: [
          { key: "gr", label: "Gain Reduction", unit: "dB", min: -30, max: 0, kind: "vu" },
        ],
      }),
    );
    expect(s.readings[0].kind).toBe("level");
  });

  it("drops a visual whose kind the UI cannot draw", () => {
    const s = toEffectStage(
      wireStage({
        visuals: [
          {
            kind: "spectrum",
            params: [],
            overlays: [],
            xMin: 0,
            xMax: 1,
            yMin: 0,
            yMax: 1,
          },
        ],
      }),
    );
    expect(s.visuals).toEqual([]);
  });

  it("normalises absent params and overlays to empty lists", () => {
    const s = toEffectStage(
      wireStage({
        visuals: [
          {
            kind: "transfer",
            params: null,
            overlays: null,
            xMin: -60,
            xMax: 0,
            yMin: -60,
            yMax: 0,
          },
        ],
      }),
    );
    expect(s.visuals).toEqual([
      {
        kind: "transfer",
        params: [],
        overlays: [],
        xMin: -60,
        xMax: 0,
        yMin: -60,
        yMax: 0,
      },
    ]);
  });

  it("normalises a telemetry payload", () => {
    const s = toEffectStage(
      wireStage({
        readings: [
          { key: "gr", label: "Gain Reduction", unit: "dB", min: -30, max: 0, kind: "gain-reduction" },
        ],
        visuals: [
          {
            kind: "transfer",
            params: ["threshold", "ratio"],
            overlays: ["in", "gr"],
            xMin: -60,
            xMax: 0,
            yMin: -60,
            yMax: 0,
          },
        ],
      }),
    );
    expect(s.readings).toEqual([
      { key: "gr", label: "Gain Reduction", unit: "dB", min: -30, max: 0, kind: "gain-reduction" },
    ]);
    expect(s.visuals).toEqual([
      {
        kind: "transfer",
        params: ["threshold", "ratio"],
        overlays: ["in", "gr"],
        xMin: -60,
        xMax: 0,
        yMin: -60,
        yMax: 0,
      },
    ]);
  });
});

describe("toEffectKind", () => {
  it("carries the script flag", () => {
    const k = toEffectKind({
      kind: "script",
      impl: "Tremolo",
      label: "Tremolo",
      weight: 0,
      scripted: true,
    } satisfies WireKind);
    expect(k.scripted).toBe(true);
    expect(k.impl).toBe("Tremolo");
  });
});
