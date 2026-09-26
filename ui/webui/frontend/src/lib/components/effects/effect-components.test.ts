import { describe, expect, it } from "vitest";
import {
  ParamKind,
  effectiveWidget,
  type EffectParam,
  type EffectStage,
} from "../../effect-types";
import {
  coerceValue,
  commonParam,
  effectGroups,
  finalIndex,
  formatValue,
  indexOfStage,
  isFiniteNumber,
  isKnobBank,
  meterFraction,
  meterText,
  paramValue,
  roundToStep,
} from "./effects-helpers";

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

// A realistic Lua effect schema: the three standard params in front, then a
// declared group with a label, a group and a widget set.
const luaSchema: EffectParam[] = [
  param({ key: "bypass", kind: ParamKind.Bool, default: false, label: "Bypass", widget: "switch" }),
  param({
    key: "input-gain",
    kind: ParamKind.Float,
    min: -36,
    max: 36,
    step: 0.1,
    unit: "dB",
    label: "Input Gain",
    group: "Gain",
    widget: "slider",
  }),
  param({
    key: "output-gain",
    kind: ParamKind.Float,
    min: -36,
    max: 36,
    step: 0.1,
    unit: "dB",
    label: "Output Gain",
    group: "Gain",
    widget: "slider",
  }),
  param({
    key: "rate",
    kind: ParamKind.Float,
    min: 0.1,
    max: 20,
    step: 0.1,
    unit: "Hz",
    default: 5,
    label: "Rate",
    group: "Tremolo",
    widget: "knob",
  }),
  param({
    key: "shape",
    kind: ParamKind.Enum,
    options: ["sine", "square"],
    default: "sine",
    label: "Shape",
    group: "Tremolo",
    widget: "",
  }),
];

function stage(over: Partial<EffectStage>): EffectStage {
  return {
    id: "s1",
    kind: "script",
    impl: "Tremolo",
    label: "Tremolo",
    bypassed: false,
    schema: luaSchema,
    values: {},
    meters: null,
    readings: [],
    visual: null,
    ...over,
  };
}

describe("effectGroups", () => {
  it("drops the standard params from the effect's own sections", () => {
    const groups = effectGroups(luaSchema);
    expect(groups.map((g) => g.name)).toEqual(["Tremolo"]);
    expect(groups[0].params.map((p) => p.key)).toEqual(["rate", "shape"]);
  });

  it("drops a gain group that holds only the standard gains", () => {
    const groups = effectGroups(luaSchema.map((p) => p));
    expect(groups.some((g) => g.name === "Gain")).toBe(false);
  });

  it("keeps declaration order across an effect group and a gain group", () => {
    const schema = [
      param({ key: "bypass", kind: ParamKind.Bool }),
      param({ key: "input-gain", group: "Gain" }),
      param({ key: "output-gain", group: "Gain" }),
      param({ key: "tone", group: "Filter" }),
      param({ key: "trim", group: "Gain" }),
    ];
    const groups = effectGroups(schema);
    expect(groups.map((g) => g.name)).toEqual(["Gain", "Filter"]);
    expect(groups[0].params.map((p) => p.key)).toEqual(["trim"]);
  });

  it("returns nothing for a schema of only standard params", () => {
    expect(effectGroups(luaSchema.slice(0, 3))).toEqual([]);
  });
});

describe("commonParam", () => {
  it("finds the pinned base controls by key", () => {
    const s = stage({});
    expect(commonParam(s, "bypass")?.label).toBe("Bypass");
    expect(commonParam(s, "input-gain")?.unit).toBe("dB");
    expect(commonParam(s, "output-gain")?.min).toBe(-36);
  });

  it("is null for a key the schema does not carry", () => {
    expect(commonParam(stage({}), "cutoff")).toBeNull();
  });
});

describe("effectiveWidget", () => {
  it("honours an explicit hint over the kind default", () => {
    expect(effectiveWidget(param({ kind: ParamKind.Float, widget: "knob" }))).toBe("knob");
    expect(effectiveWidget(param({ kind: ParamKind.Float, widget: "slider" }))).toBe("slider");
    expect(effectiveWidget(param({ kind: ParamKind.Bool, widget: "slider" }))).toBe("slider");
  });

  it("falls back per kind when no hint is given", () => {
    expect(effectiveWidget(param({ kind: ParamKind.Float, widget: "" }))).toBe("slider");
    expect(effectiveWidget(param({ kind: ParamKind.Int, widget: "" }))).toBe("slider");
    expect(effectiveWidget(param({ kind: ParamKind.Bool, widget: "" }))).toBe("switch");
    expect(effectiveWidget(param({ kind: ParamKind.Enum, widget: "" }))).toBe("select");
  });

  it("treats a knob hint on a float as a knob", () => {
    const p = luaSchema.find((x) => x.key === "rate") as EffectParam;
    expect(effectiveWidget(p)).toBe("knob");
  });
});

describe("isKnobBank", () => {
  // The EQ's shape: twenty knobs that differ only in key, label and value.
  const eqBand = (i: number) =>
    param({
      key: `gain${String(i).padStart(2, "0")}`,
      min: -12,
      max: 12,
      step: 0.1,
      unit: "dB",
      label: `${25 * Math.pow(2, i)} Hz`,
      group: "Bands",
      widget: "knob",
    });

  it("treats a group of same-shaped knobs as a bank", () => {
    const bands = Array.from({ length: 20 }, (_, i) => eqBand(i));
    expect(isKnobBank(bands)).toBe(true);
  });

  it("is a bank however the knobs differ in range or label", () => {
    const mixed = [
      param({ key: "a", widget: "knob", min: -12, max: 12 }),
      param({ key: "b", widget: "knob", min: 0, max: 1, unit: "Hz" }),
      param({ key: "c", widget: "knob", kind: ParamKind.Int, min: 0, max: 4, step: 1 }),
    ];
    expect(isKnobBank(mixed)).toBe(true);
  });

  it("is not a bank below the minimum, so a pair of knobs stays a column", () => {
    expect(isKnobBank([eqBand(0), eqBand(1)])).toBe(false);
    expect(isKnobBank([eqBand(0)])).toBe(false);
    expect(isKnobBank([])).toBe(false);
  });

  it("is not a bank when any control is not a knob", () => {
    const withSlider = [eqBand(0), eqBand(1), param({ key: "mix", widget: "slider" })];
    expect(isKnobBank(withSlider)).toBe(false);

    // An auto-widget float falls back to a slider, so it breaks the bank too.
    const withAuto = [eqBand(0), eqBand(1), param({ key: "mix", widget: "" })];
    expect(isKnobBank(withAuto)).toBe(false);
  });

  it("is not a bank when a switch or a select is in the group", () => {
    const withSwitch = [
      eqBand(0),
      eqBand(1),
      param({ key: "solo", kind: ParamKind.Bool, widget: "switch" }),
    ];
    expect(isKnobBank(withSwitch)).toBe(false);

    const withSelect = [
      eqBand(0),
      eqBand(1),
      param({ key: "shape", kind: ParamKind.Enum, options: ["sine"], widget: "knob" }),
    ];
    expect(isKnobBank(withSelect)).toBe(false);
  });
});

describe("paramValue", () => {
  it("reads the current value from the stage", () => {
    const s = stage({ values: { rate: 7.5 } });
    const p = luaSchema.find((x) => x.key === "rate") as EffectParam;
    expect(paramValue(s, p)).toBe(7.5);
  });

  it("falls back to the declared default when the value is absent", () => {
    const s = stage({ values: {} });
    const p = luaSchema.find((x) => x.key === "rate") as EffectParam;
    expect(paramValue(s, p)).toBe(5);
  });
});

describe("coerceValue", () => {
  it("emits a number for a Float", () => {
    const p = param({ kind: ParamKind.Float, min: 0, max: 10, step: 0.1 });
    expect(coerceValue(p, "3.5")).toBe(3.5);
  });

  it("emits an integer for an Int", () => {
    const p = param({ kind: ParamKind.Int, min: 0, max: 10, step: 1 });
    expect(coerceValue(p, 3.7)).toBe(4);
  });

  it("emits a boolean for a Bool", () => {
    const p = param({ kind: ParamKind.Bool });
    expect(coerceValue(p, 1)).toBe(true);
    expect(coerceValue(p, 0)).toBe(false);
  });

  it("emits a string for an Enum", () => {
    const p = param({ kind: ParamKind.Enum, options: ["a", "b"] });
    expect(coerceValue(p, "b")).toBe("b");
  });

  it("clamps a number into the declared range", () => {
    const p = param({ kind: ParamKind.Float, min: -36, max: 36, step: 0.1 });
    expect(coerceValue(p, 999)).toBe(36);
    expect(coerceValue(p, -999)).toBe(-36);
  });
});

describe("roundToStep", () => {
  it("removes the float residue a step leaves behind", () => {
    expect(roundToStep(0.1 + 0.2, 0.1)).toBe(0.3);
    expect(roundToStep(3.14159, 0.1)).toBe(3.1);
  });

  it("snaps to the nearest step rather than truncating", () => {
    expect(roundToStep(1.26, 0.5)).toBe(1.5);
    expect(roundToStep(1.24, 0.5)).toBe(1);
  });

  it("passes a non-finite value through as a safe zero", () => {
    expect(roundToStep(Number.NaN, 0.1)).toBe(0);
  });
});

describe("formatValue", () => {
  it("renders a float with its unit", () => {
    const p = param({ kind: ParamKind.Float, unit: "dB" });
    expect(formatValue(p, 1.5)).toBe("1.5 dB");
  });

  it("renders a bool as On or Off", () => {
    const p = param({ kind: ParamKind.Bool });
    expect(formatValue(p, true)).toBe("On");
    expect(formatValue(p, false)).toBe("Off");
  });

  it("renders an enum as its option text", () => {
    const p = param({ kind: ParamKind.Enum, options: ["sine"] });
    expect(formatValue(p, "sine")).toBe("sine");
  });
});

describe("meterFraction", () => {
  it("maps a dBFS level onto the meter", () => {
    expect(meterFraction(-6, -60, 0)).toBeCloseTo(54 / 60, 10);
    expect(meterFraction(0, -60, 0)).toBe(1);
    expect(meterFraction(-60, -60, 0)).toBe(0);
  });

  it("clamps a reading above the top of the range", () => {
    expect(meterFraction(6, -60, 0)).toBe(1);
  });

  it("is an empty track when the effect does not meter", () => {
    expect(meterFraction(null, -60, 0)).toBe(0);
  });
});

describe("meterText", () => {
  it("renders a reading to one decimal", () => {
    expect(meterText(-6.023)).toBe("-6.0 dB");
  });

  it("renders the unknown-value placeholder when there is no reading", () => {
    // "--" is the same placeholder formatTime uses, and it keeps the panel
    // free of a dash glyph that could read as a real level.
    expect(meterText(null)).toBe("--");
    expect(meterText(Number.NaN)).toBe("--");
  });
});

describe("isFiniteNumber", () => {
  it("accepts a real number and rejects NaN, infinities and non-numbers", () => {
    expect(isFiniteNumber(1.5)).toBe(true);
    expect(isFiniteNumber(Number.NaN)).toBe(false);
    expect(isFiniteNumber(Number.POSITIVE_INFINITY)).toBe(false);
    expect(isFiniteNumber("1.5")).toBe(false);
    expect(isFiniteNumber(null)).toBe(false);
  });
});

// The reorder index is the one place a UI bug reorders the audio signal path,
// so the conversion is pinned against the engine's MoveEffect splice for every
// from/gap pair, not just the happy path.
describe("finalIndex", () => {
  // A transcription of the engine's splice, so the test asserts the contract
  // rather than restating the helper's arithmetic.
  function engineMove(list: string[], from: number, to: number): string[] {
    const next: string[] = [];
    for (let i = 0; i < list.length; i++) {
      if (i === from) continue;
      if (next.length === to) next.push(list[from]);
      next.push(list[i]);
    }
    if (next.length === to) next.push(list[from]);
    return next;
  }

  it("drops the item at the gap the user pointed at, dragging down", () => {
    const list = ["A", "B", "C", "D"];
    // Drag A onto the gap before C (index 2).
    const to = finalIndex(0, 2, list.length);
    expect(engineMove(list, 0, to)).toEqual(["B", "A", "C", "D"]);
  });

  it("lands an end drop on the last index instead of erroring", () => {
    const list = ["A", "B", "C", "D"];
    const to = finalIndex(0, list.length, list.length);
    expect(to).toBe(3);
    expect(engineMove(list, 0, to)).toEqual(["B", "C", "D", "A"]);
  });

  it("keeps an upward drag at the gap index", () => {
    const list = ["A", "B", "C", "D"];
    const to = finalIndex(3, 1, list.length);
    expect(engineMove(list, 3, to)).toEqual(["A", "D", "B", "C"]);
  });

  it("is a no-op when the gap is the item's own position", () => {
    const list = ["A", "B", "C"];
    expect(finalIndex(1, 1, list.length)).toBe(1);
  });

  it("matches the engine for every from/gap pair", () => {
    const list = ["A", "B", "C", "D", "E"];
    for (let from = 0; from < list.length; from++) {
      for (let gap = 0; gap <= list.length; gap++) {
        const to = finalIndex(from, gap, list.length);
        expect(to).toBeGreaterThanOrEqual(0);
        expect(to).toBeLessThan(list.length);
        const moved = engineMove(list, from, to);
        expect(moved).toContain(list[from]);
        expect(moved.length).toBe(list.length);
      }
    }
  });
});

describe("indexOfStage", () => {
  it("finds a stage by id and reports -1 when absent", () => {
    const stages = [{ id: "a" }, { id: "b" }];
    expect(indexOfStage(stages, "b")).toBe(1);
    expect(indexOfStage(stages, "z")).toBe(-1);
  });
});

describe("trimFixed negative zero", () => {
  it("does not render a signed zero", () => {
    expect(formatValue(param({ unit: "dB" }), -0.001)).toBe("0 dB");
  });
});

describe("meterFraction zero span", () => {
  it("returns 0 instead of pegging full when min equals max", () => {
    expect(meterFraction(0, -60, -60)).toBe(0);
  });
});
