// Frozen shapes for the effect UI.
//
// These mirror the Go types the bridge exposes (EffectKind, EffectStage,
// EffectChain) and the generated Wails binding for dsp.Param. They are
// restated here so the generic control components can be written and checked
// before the bindings are regenerated, and so a component depends on the
// shape rather than on the generated module. Keep the two in step: a field
// added here must exist in the Go struct, and vice versa.

/** ParamKind mirrors dsp.Kind. The numbers are the Go iota order. */
export const ParamKind = {
  Float: 0,
  Int: 1,
  Bool: 2,
  Enum: 3,
} as const;

/** Widget is the control hint dsp.Param carries. Empty means "pick from Kind". */
export type Widget = "" | "slider" | "knob" | "switch" | "select";

/** EffectParam mirrors dsp.Param, the one description a UI renders from. */
export interface EffectParam {
  key: string;
  kind: number;
  min: number;
  max: number;
  step: number;
  unit: string;
  options: string[] | null;
  default: unknown;
  /** Human name. Empty means derive from key. */
  label: string;
  /** Section. Empty means the effect's unnamed section. */
  group: string;
  widget: Widget;
}

/** EffectKind is one effect that can be added to the chain. */
export interface EffectKind {
  kind: string;
  impl: string;
  label: string;
  weight: number;
  /** True when the effect came from a Lua script. */
  scripted: boolean;
}

/** ReadingKind mirrors dsp.ReadingKind: how a reading is drawn. A string type
 *  for the same reason Widget is, because the value crosses the JSON bridge. */
export type ReadingKind = "level" | "gain-reduction" | "scalar";

/** Reading mirrors dsp.Reading: one Meters key explained, so a UI can draw it
 *  without knowing which effect it is looking at. The number stays the
 *  effect's to publish; this only says how to render it. */
export interface Reading {
  /** Matches a Meters key, e.g. "gr". */
  key: string;
  label: string;
  /** "dB", "%", "Hz". */
  unit: string;
  /** The drawn range. */
  min: number;
  max: number;
  kind: ReadingKind;
}

/** VisualKind mirrors dsp.VisualKind: a plot the UI knows how to draw. The
 *  effect names the kind; the UI owns the renderer. */
export type VisualKind = "transfer" | "gain-reduction";

/** Visual mirrors dsp.Visual: a plot declaration. Params are schema keys the
 *  curve is derived from, so a static curve is recomputed from Values rather
 *  than carried on the meter path. Overlays are reading keys drawn live. */
export interface Visual {
  kind: VisualKind;
  params: string[];
  overlays: string[];
  xMin: number;
  xMax: number;
  yMin: number;
  yMax: number;
}

/** EffectStage is one instance in the chain. */
export interface EffectStage {
  id: string;
  kind: string;
  impl: string;
  label: string;
  bypassed: boolean;
  /** Includes the three standard params; use isCommonParam to split them. */
  schema: EffectParam[];
  values: Record<string, unknown>;
  /** dsp.MeterIn / dsp.MeterOut in dBFS. Null when the effect does not meter. */
  meters: Record<string, number> | null;
  /** dsp.Described. Empty when the effect implements only Metered; use
   *  effectiveReadings to get the drawable set either way. */
  readings: Reading[];
  /** dsp.Visualized. Null when the effect wants no plot. */
  visual: Visual | null;
}

/** EffectChain is the whole post-ring chain, in processing order. */
export interface EffectChain {
  stages: EffectStage[];
}

/** The three standard parameters every effect carries. A UI pins these as its
 *  fixed base controls; an effect cannot hide or re-declare them. Mirrors
 *  dsp.CommonParam. */
export const COMMON_PARAMS = ["bypass", "input-gain", "output-gain"] as const;

/** isCommonParam reports whether key is one of the standard base controls. */
export function isCommonParam(key: string): boolean {
  return (COMMON_PARAMS as readonly string[]).includes(key);
}

/** The standard meter keys, mirroring dsp.MeterIn / dsp.MeterOut. */
export const METER_IN = "in";
export const METER_OUT = "out";

/** meterDB reads one standard meter from a stage, or null when it is absent. */
export function meterDB(
  stage: EffectStage,
  key: string,
): number | null {
  const v = stage.meters?.[key];

  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/**
 * effectiveReadings resolves the readings a UI should draw by augmenting the
 * standard pair with the effect's declared ones. The standard In/Out level
 * readings are synthesised from the meters when the effect meters at all, and
 * the stage's declared readings follow. A declared reading that already names
 * a standard key wins over the synthesised one, so no key is drawn twice. An
 * effect that predates telemetry implements only Metered and declares nothing,
 * so it still gets the pair; one that declares only e.g. "gr" keeps the pair
 * it would otherwise have lost. When the effect does not meter, only the
 * declared readings are returned. This is the one place that rule lives, so a
 * component and a test agree.
 */
export function effectiveReadings(stage: EffectStage): Reading[] {
  const declared = stage.readings;
  if (stage.meters === null) {
    return declared;
  }

  const declaredKeys = new Set(declared.map((r) => r.key));
  const pair: Reading[] = [];
  if (!declaredKeys.has(METER_IN)) {
    pair.push({ key: METER_IN, label: "In", unit: "dBFS", min: -60, max: 0, kind: "level" });
  }
  if (!declaredKeys.has(METER_OUT)) {
    pair.push({ key: METER_OUT, label: "Out", unit: "dBFS", min: -60, max: 0, kind: "level" });
  }

  return [...pair, ...declared];
}

/**
 * effectiveWidget resolves the control to draw: the explicit hint when the
 * effect gave one, otherwise a sensible default for the Kind. It is the one
 * place that mapping lives, so a component and a test agree.
 */
export function effectiveWidget(p: EffectParam): Exclude<Widget, ""> {
  if (p.widget !== "") {
    return p.widget;
  }
  switch (p.kind) {
    case ParamKind.Bool:
      return "switch";
    case ParamKind.Enum:
      return "select";
    default:
      return "slider";
  }
}

/**
 * paramLabel is the text a control shows: the explicit label, else the key
 * with separators turned into spaces and each word capitalised.
 */
export function paramLabel(p: EffectParam): string {
  if (p.label.trim() !== "") {
    return p.label;
  }
  return p.key
    .split(/[-_]/)
    .filter((s) => s !== "")
    .map((s) => s.charAt(0).toUpperCase() + s.slice(1))
    .join(" ");
}

/**
 * groupParams splits a schema into ordered groups. Group order follows first
 * appearance, which is the order the effect declared. It does not sort or
 * hoist anything: a caller that wants the base controls first gets that
 * because the engine prepends them, not because of this function.
 */
export interface ParamGroup {
  name: string;
  params: EffectParam[];
}

export function groupParams(schema: EffectParam[]): ParamGroup[] {
  const order: string[] = [];
  const byName = new Map<string, EffectParam[]>();
  for (const p of schema) {
    const name = p.group;
    let list = byName.get(name);
    if (!list) {
      list = [];
      byName.set(name, list);
      order.push(name);
    }
    list.push(p);
  }

  return order.map((name) => ({ name, params: byName.get(name) ?? [] }));
}
