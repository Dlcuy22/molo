import { describe, expect, it } from "vitest";
import {
  METER_IN,
  METER_OUT,
  ParamKind,
  effectiveReadings,
  effectiveWidget,
  groupParams,
  isCommonParam,
  meterDB,
  paramLabel,
  type EffectParam,
  type EffectStage,
  type Reading,
} from "./effect-types";

function param(over: Partial<EffectParam>): EffectParam {
  return {
    key: "k",
    kind: ParamKind.Float,
    min: 0,
    max: 1,
    step: 0.1,
    unit: "",
    options: null,
    default: 0,
    label: "",
    group: "",
    widget: "",
    ...over,
  };
}

// The widget mapping is the contract that lets a script pick "knob" without
// the frontend knowing which effect it is looking at. These pin the fallback
// as well, because an effect that states no widget must still render.
describe("effectiveWidget", () => {
  it("honours an explicit hint over the kind default", () => {
    expect(effectiveWidget(param({ kind: ParamKind.Float, widget: "knob" }))).toBe("knob");
    expect(effectiveWidget(param({ kind: ParamKind.Float, widget: "slider" }))).toBe("slider");
  });

  it("falls back per kind when no hint is given", () => {
    expect(effectiveWidget(param({ kind: ParamKind.Float, widget: "" }))).toBe("slider");
    expect(effectiveWidget(param({ kind: ParamKind.Int, widget: "" }))).toBe("slider");
    expect(effectiveWidget(param({ kind: ParamKind.Bool, widget: "" }))).toBe("switch");
    expect(effectiveWidget(param({ kind: ParamKind.Enum, widget: "" }))).toBe("select");
  });
});

describe("paramLabel", () => {
  it("prefers the declared label", () => {
    expect(paramLabel(param({ key: "input-gain", label: "Input Gain" }))).toBe("Input Gain");
  });

  it("derives a readable name from the key when no label is set", () => {
    expect(paramLabel(param({ key: "input-gain" }))).toBe("Input Gain");
    expect(paramLabel(param({ key: "cutoff" }))).toBe("Cutoff");
    expect(paramLabel(param({ key: "dry_wet" }))).toBe("Dry Wet");
  });

  it("does not treat a whitespace label as a real one", () => {
    expect(paramLabel(param({ key: "rate", label: "   " }))).toBe("Rate");
  });
});

describe("isCommonParam", () => {
  it("knows the three standard controls", () => {
    expect(isCommonParam("bypass")).toBe(true);
    expect(isCommonParam("input-gain")).toBe(true);
    expect(isCommonParam("output-gain")).toBe(true);
    expect(isCommonParam("cutoff")).toBe(false);
  });
});

describe("groupParams", () => {
  it("keeps first-appearance order and files each param once", () => {
    const schema = [
      param({ key: "bypass", group: "" }),
      param({ key: "input-gain", group: "Gain" }),
      param({ key: "output-gain", group: "Gain" }),
      param({ key: "rate", group: "Tremolo" }),
      param({ key: "depth", group: "Tremolo" }),
    ];
    const groups = groupParams(schema);
    expect(groups.map((g) => g.name)).toEqual(["", "Gain", "Tremolo"]);
    expect(groups[1].params.map((p) => p.key)).toEqual(["input-gain", "output-gain"]);
    expect(groups[2].params.map((p) => p.key)).toEqual(["rate", "depth"]);
  });

  it("returns an empty list for an empty schema", () => {
    expect(groupParams([])).toEqual([]);
  });
});

function stage(over: Partial<EffectStage>): EffectStage {
  return {
    id: "s1",
    kind: "script",
    impl: "Tremolo",
    label: "Tremolo",
    bypassed: false,
    schema: [],
    values: {},
    meters: null,
    readings: [],
    visual: null,
    ...over,
  };
}

describe("meterDB", () => {
  it("reads a finite level", () => {
    expect(meterDB(stage({ meters: { in: -6.02, out: -7.07 } }), "in")).toBe(-6.02);
  });

  it("is null when the effect does not meter", () => {
    expect(meterDB(stage({ meters: null }), "in")).toBeNull();
    expect(meterDB(stage({ meters: {} }), "out")).toBeNull();
  });

  it("rejects a non-finite reading instead of drawing it", () => {
    expect(meterDB(stage({ meters: { in: Number.NaN } }), "in")).toBeNull();
    expect(meterDB(stage({ meters: { in: Number.POSITIVE_INFINITY } }), "in")).toBeNull();
  });
});

describe("effectiveReadings", () => {
  const gr: Reading = {
    key: "gr",
    label: "Gain Reduction",
    unit: "dB",
    min: -30,
    max: 0,
    kind: "gain-reduction",
  };

  it("returns only the declared readings when the effect does not meter", () => {
    const s = stage({ readings: [gr], meters: null });
    expect(effectiveReadings(s)).toEqual([gr]);
  });

  it("returns only the synthesised pair when nothing is declared", () => {
    const s = stage({ readings: [], meters: { in: -6, out: -7 } });
    const r = effectiveReadings(s);
    expect(r.map((x) => x.key)).toEqual([METER_IN, METER_OUT]);
    expect(r.map((x) => x.kind)).toEqual(["level", "level"]);
    expect(r.map((x) => x.unit)).toEqual(["dBFS", "dBFS"]);
  });

  it("augments the pair with the declared readings, pair first", () => {
    const s = stage({ readings: [gr], meters: { in: -6, out: -7 } });
    expect(effectiveReadings(s)).toEqual([
      { key: METER_IN, label: "In", unit: "dBFS", min: -60, max: 0, kind: "level" },
      { key: METER_OUT, label: "Out", unit: "dBFS", min: -60, max: 0, kind: "level" },
      gr,
    ]);
  });

  it("lets a declared standard key win over the synthesised one", () => {
    const inReading: Reading = {
      key: METER_IN,
      label: "Input",
      unit: "dBFS",
      min: -48,
      max: 6,
      kind: "level",
    };
    const s = stage({ readings: [inReading, gr], meters: { in: -6, out: -7 } });
    expect(effectiveReadings(s)).toEqual([
      { key: METER_OUT, label: "Out", unit: "dBFS", min: -60, max: 0, kind: "level" },
      inReading,
      gr,
    ]);
  });

  it("returns an empty list when there are neither readings nor meters", () => {
    expect(effectiveReadings(stage({ readings: [], meters: null }))).toEqual([]);
  });
});
