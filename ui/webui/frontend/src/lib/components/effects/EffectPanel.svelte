<script lang="ts">
  import { effectiveReadings, type EffectStage } from "../../effect-types";
  import { commonParam, effectGroups, isKnobBank, numericValue, paramValue } from "./effects-helpers";
  import DynamicsGraph from "./DynamicsGraph.svelte";
  import ParamControl from "./ParamControl.svelte";
  import Readings from "./Readings.svelte";
  import TransferCurve from "./TransferCurve.svelte";

  // The control surface for one stage. It is presentational: bypass toggles and
  // param edits go out as callbacks, and the base controls are pinned in the
  // panel rather than left to the effect's schema.
  let {
    stage,
    onparam,
    onbypass,
  }: {
    stage: EffectStage;
    onparam: (key: string, value: unknown) => void;
    onbypass: (bypassed: boolean) => void;
  } = $props();

  const title = $derived(stage.label.trim() || stage.impl.trim() || stage.kind);

  const bypass = $derived(commonParam(stage, "bypass"));
  const inGain = $derived(commonParam(stage, "input-gain"));
  const outGain = $derived(commonParam(stage, "output-gain"));
  const gains = $derived([inGain, outGain].filter((g) => g !== null));
  const groups = $derived(effectGroups(stage.schema));

  // How many knobs fit one grid row. The panel tracks the window's columns, so a
  // bank spreads to the available width instead of stacking. It is a viewport
  // breakpoint rather than a container query because the panel is the only thing
  // in its scroll container, so the two widths agree.
  const knobsPerRow = $derived(
    typeof window === "undefined"
      ? 10
      : window.matchMedia("(min-width: 1280px)").matches
        ? 10
        : window.matchMedia("(min-width: 1024px)").matches
          ? 7
          : window.matchMedia("(min-width: 768px)").matches
            ? 5
            : 3,
  );

  // The style object is rebuilt when the column count changes so the custom
  // property the grid's template reads is never stale.
  const bankStyle = $derived(`--knob-cols: ${knobsPerRow}`);

  // The drawable readings: the effect's own declared ones, plus the standard
  // In/Out pair when it only meters. This is what replaces the hardcoded two
  // bars, so an effect with no readings still shows the legacy pair.
  const readings = $derived(effectiveReadings(stage));

  // Split the declared plots by kind so the layout is not at the mercy of the
  // script's declaration order: the scrolling dynamics graph reads as the
  // headline and sits first, the transfer curve stays with the controls it
  // explains.
  const dynamicsVisuals = $derived(stage.visuals.filter((v) => v.kind === "dynamics"));
  const transferVisuals = $derived(stage.visuals.filter((v) => v.kind === "transfer"));

  // The checkbox shows the user's bypass parameter, so it never flips on its
  // own. The dimming follows the effective flag, which an effect may set for a
  // reason other than the user, such as a watchdog fault. Keeping the two
  // separate is what lets the panel say which one it is instead of claiming a
  // deliberate bypass for a fault.
  const userBypass = $derived(bypass !== null && Boolean(paramValue(stage, bypass)));
  const effective = $derived(stage.bypassed);
  const faulted = $derived(effective && !userBypass);
</script>

<section class="flex flex-col gap-3 rounded-[6px] border border-line bg-surface p-3">
  <header class="flex flex-col gap-2">
    <div class="flex items-center justify-between gap-3">
      <h2 class="min-w-0 truncate text-sm font-medium text-fg">{title}</h2>
      {#if bypass}
        <label class="flex shrink-0 items-center gap-2 text-xs text-muted">
          <span>Bypass</span>
          <span class="relative inline-flex shrink-0 items-center">
            <input
              class="peer h-4 w-7 cursor-pointer appearance-none rounded-full bg-track transition-colors checked:bg-accent"
              type="checkbox"
              checked={userBypass}
              aria-label="Bypass {title}"
              onchange={(e) => onbypass((e.currentTarget as HTMLInputElement).checked)}
            />
            <span
              class="pointer-events-none absolute left-0.5 size-3 rounded-full bg-fg transition-transform peer-checked:translate-x-3"
              aria-hidden="true"
            ></span>
          </span>
        </label>
      {/if}
    </div>

    <div class="flex flex-col gap-2 rounded-[4px] bg-bg/40 p-2">
      <Readings {readings} meters={stage.meters} />
    </div>

    <div class="grid grid-cols-2 gap-x-3 gap-y-2">
      {#each gains as gain (gain.key)}
        <ParamControl
          param={gain}
          value={paramValue(stage, gain)}
          onchange={(v) => onparam(gain.key, v)}
        />
      {/each}
    </div>
  </header>

  <div class="h-px bg-line" role="presentation"></div>

  <!--
    The effect's own controls. While bypassed they dim but stay operable, and
    the note states that in words, because a dimmed control reads as disabled
    otherwise. A fault is named as a fault: reporting it as a bypass would tell
    the user they did something they did not.
  -->
  <div class="flex flex-col gap-3 transition-opacity" class:opacity-50={effective}>
    {#if faulted}
      <p class="text-[11px] text-muted" role="status">
        Bypassed automatically: this effect stopped keeping up. Controls still apply.
      </p>
    {:else if effective}
      <p class="text-[11px] text-muted" role="status">Bypassed. Controls still apply.</p>
    {/if}

    <!--
      The plots sit with the controls they explain, above the groups, so the
      curve and the threshold, ratio and knee that shape it read together. The
      dynamics graph comes first: it is the scrolling headline, and the curve
      and knobs below are what shape it. Both dim with the controls while
      bypassed because the live overlays freeze at the last value the effect
      published.
    -->
    {#each dynamicsVisuals as visual, i (i)}
      <DynamicsGraph
        visual={visual}
        {readings}
        stageId={stage.id}
        meters={stage.meters}
      />
    {/each}

    {#each transferVisuals as visual, i (i)}
      <TransferCurve visual={visual} values={stage.values} meters={stage.meters} />
    {/each}

    {#each groups as group (group.name)}
      <div class="flex flex-col gap-2">
        {#if group.name !== ""}
          <h3 class="text-[11px] font-medium uppercase tracking-wide text-muted">{group.name}</h3>
        {/if}
        <!--
          A bank of knobs spreads across the row and wraps; every other group
          stays a column, so a panel that mixes a slider into a knob group does
          not put either one in a cell it does not fit.
        -->
        {#if isKnobBank(group.params)}
          <div class="knob-grid" style={bankStyle}>
            {#each group.params as p (p.key)}
              <ParamControl param={p} value={paramValue(stage, p)} onchange={(v) => onparam(p.key, v)} />
            {/each}
          </div>
        {:else}
          {#each group.params as p (p.key)}
            <ParamControl param={p} value={paramValue(stage, p)} onchange={(v) => onparam(p.key, v)} />
          {/each}
        {/if}
      </div>
    {/each}

    {#if groups.length === 0}
      <p class="text-[11px] text-muted">This effect has no settings beyond the base controls.</p>
    {/if}
  </div>
</section>
