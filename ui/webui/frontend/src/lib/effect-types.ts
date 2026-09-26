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
