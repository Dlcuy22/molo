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
} from "../../bindings/github.com/dlcuy22/player/ui/webui/models";
import type {
  EffectKind,
  EffectParam,
  EffectStage,
  Widget,
} from "./effect-types";

const WIDGETS: readonly Widget[] = ["", "slider", "knob", "switch", "select"];

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

/** toEffectStage maps one wire stage to the frozen shape. */
export function toEffectStage(s: WireStage): EffectStage {
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
