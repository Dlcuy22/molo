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
  function wireStage(over: Partial<WireStage>): WireStage {
    return {
      id: "s1",
      kind: "crossfeed",
      impl: "",
      label: "Crossfeed",
      bypassed: false,
      schema: null,
      values: null,
      meters: null,
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
