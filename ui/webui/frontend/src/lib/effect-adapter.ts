// Adapters from the generated Wails models to the frozen effect-UI types.
//
// The generator emits plain `string` for the widget and nullable arrays, while
// the components are typed against the narrower frozen shapes. Mapping in one
// place keeps that widening out of the components and gives the conversion a
// single testable home.

import type {
  EffectKindInfo as WireKind,
  EffectParamInfo as WireParam,
  EffectStageInfo as WireStage,
} from "../../bindings/github.com/dlcuy22/molo/ui/webui/models";
import type {
  EffectKind,
  EffectParam,
  EffectStage,
  Reading,
  ReadingKind,
  Visual,
  VisualKind,
  Widget,
} from "./effect-types";

const WIDGETS: readonly Widget[] = ["", "slider", "knob", "switch", "select"];

const READING_KINDS: readonly ReadingKind[] = ["level", "gain-reduction", "scalar"];
const VISUAL_KINDS: readonly VisualKind[] = ["transfer", "gain-reduction", "dynamics"];

// The generated binding is a W4 concern; until it carries these fields they
// are declared here as optional, so this stays correct when the bindings are
// regenerated with the real fields. Omit first: intersecting an optional
// field over a declared one would collapse to never.
type WireStageBase = Omit<WireStage, "readings" | "visuals">;

interface WireReading {
  key: string;
  label: string;
  unit: string;
  min: number;
  max: number;
  kind: string;
}

interface WireVisual {
  kind: string;
  params: string[] | null;
  overlays: string[] | null;
  xMin: number;
  xMax: number;
  yMin: number;
  yMax: number;
}

type WireStageWithTelemetry = WireStageBase & {
  readings?: WireReading[] | null;
  visuals?: WireVisual[] | null;
};

/** toWidget narrows the wire string to a known hint, else "". A hint the UI
 *  does not know must fall back to the Kind default, not crash. */
export function toWidget(v: string): Widget {
  return (WIDGETS as readonly string[]).includes(v) ? (v as Widget) : "";
}

/** toEffectParam maps one wire parameter to the frozen shape. Options becomes
 *  null when empty, which is what tells a control there is no enum list. */
export function toEffectParam(p: WireParam): EffectParam {
  return {
    key: p.key,
    kind: p.kind,
    min: p.min,
    max: p.max,
    step: p.step,
    unit: p.unit,
    options: p.options && p.options.length > 0 ? p.options : null,
    default: p.default,
    label: p.label,
    group: p.group,
    widget: toWidget(p.widget),
  };
}

/** toReading maps one wire reading to the frozen shape. An unknown kind falls
 *  back to "level", which every renderer can draw, rather than reaching a
 *  component as a string it does not know. */
export function toReading(r: WireReading): Reading {
  return {
    key: r.key,
    label: r.label,
    unit: r.unit,
    min: r.min,
    max: r.max,
    kind: (READING_KINDS as readonly string[]).includes(r.kind)
      ? (r.kind as ReadingKind)
      : "level",
  };
}

/** toVisual maps one wire plot to the frozen shape. An unknown kind is dropped
 *  whole: a plot the UI cannot draw is better absent than blank. */
export function toVisual(v: WireVisual): Visual | null {
  if (!(VISUAL_KINDS as readonly string[]).includes(v.kind)) {
    return null;
  }
  return {
    kind: v.kind as VisualKind,
    params: v.params ?? [],
    overlays: v.overlays ?? [],
    xMin: v.xMin,
    xMax: v.xMax,
    yMin: v.yMin,
    yMax: v.yMax,
  };
}

/** toEffectStage maps one wire stage to the frozen shape. */
export function toEffectStage(s: WireStageWithTelemetry): EffectStage {
  const meters: Record<string, number> | null = s.meters
    ? Object.fromEntries(
        Object.entries(s.meters).filter(
          (entry): entry is [string, number] => typeof entry[1] === "number",
        ),
      )
    : null;

  return {
    id: s.id,
    kind: s.kind,
    impl: s.impl,
    label: s.label,
    bypassed: s.bypassed,
    schema: (s.schema ?? []).map(toEffectParam),
    values: s.values ?? {},
    meters,
    readings: (s.readings ?? []).map(toReading),
    visuals: (s.visuals ?? []).map(toVisual).filter((v): v is Visual => v !== null),
  };
}

/** toEffectKind maps one wire kind to the frozen shape. */
export function toEffectKind(k: WireKind): EffectKind {
  return {
    kind: k.kind,
    impl: k.impl,
    label: k.label,
    weight: k.weight,
    scripted: k.scripted,
  };
}
