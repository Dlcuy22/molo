<script lang="ts">
  import { CaretDown } from "phosphor-svelte";
  import { fillFrac } from "../../format";
  import type { EffectParam } from "../../effect-types";
  import {
    coerceValue,
    formatValue,
    knobAngle,
    numericValue,
    paramLabel,
    effectiveWidget,
  } from "./effects-helpers";

  // One control for one param. This is presentational: it reads the value prop
  // and emits the correctly typed value, and never reaches for a store.
  let {
    param,
    value,
    onchange,
  }: { param: EffectParam; value: unknown; onchange: (value: unknown) => void } = $props();

  const widget = $derived(effectiveWidget(param));
  const label = $derived(paramLabel(param));
  const numeric = $derived(numericValue(value, numericValue(param.default, 0)));
  const fill = $derived(fillFrac(numeric, param.min, param.max));
  const angle = $derived(knobAngle(numeric, param.min, param.max));
  const text = $derived(formatValue(param, value));

  // An Enum without options cannot be a select, so it falls back to the slider
  // path rather than rendering an unusable empty dropdown.
  const useSelect = $derived(
    widget === "select" && param.options !== null && param.options.length > 0,
  );

  // Compare as strings: the value can arrive as a number-like string, and a
  // strict match against an unknown would silently fall back to the first
  // option, showing a selection the engine does not hold.
  const selected = $derived(value === undefined || value === null ? "" : String(value));

  function emit(raw: unknown) {
    onchange(coerceValue(param, raw));
  }

  function onRange(e: Event) {
    emit(Number((e.currentTarget as HTMLInputElement).value));
  }

  function onToggle(e: Event) {
    emit((e.currentTarget as HTMLInputElement).checked);
  }

  function onSelect(e: Event) {
    emit((e.currentTarget as HTMLSelectElement).value);
  }

  function onKnobKey(e: KeyboardEvent) {
    const el = e.currentTarget as HTMLInputElement;
    const step = param.step > 0 ? param.step : (param.max - param.min) / 100 || 1;
    const big = step * 10;
    let v = numeric;
    if (e.key === "ArrowUp" || e.key === "ArrowRight" || e.key === "PageUp") {
      v = numeric + (e.key === "PageUp" ? big : step);
    } else if (e.key === "ArrowDown" || e.key === "ArrowLeft" || e.key === "PageDown") {
      v = numeric - (e.key === "PageDown" ? big : step);
    } else if (e.key === "Home") {
      v = param.min;
    } else if (e.key === "End") {
      v = param.max;
    } else {
      return;
    }
    e.preventDefault();
    el.value = String(e.key === "Home" ? param.min : e.key === "End" ? param.max : v);
    emit(el.value);
  }
</script>

{#if widget === "switch"}
  <label class="flex items-center justify-between gap-3 text-xs text-muted">
    <span>{label}</span>
    <span class="relative inline-flex shrink-0 items-center">
      <input
        class="peer h-4 w-7 cursor-pointer appearance-none rounded-full bg-track transition-colors checked:bg-accent"
        type="checkbox"
        checked={Boolean(value)}
        aria-label={label}
        onchange={onToggle}
      />
      <span
        class="pointer-events-none absolute left-0.5 size-3 rounded-full bg-fg transition-transform peer-checked:translate-x-3"
        aria-hidden="true"
      ></span>
    </span>
  </label>
{:else if useSelect}
  <label class="flex items-center justify-between gap-3 text-xs text-muted">
    <span>{label}</span>
    <span class="relative">
      <select
        class="appearance-none rounded-[4px] border border-line bg-surface py-1.5 pl-2 pr-7 text-xs text-fg transition-colors hover:bg-hover"
        aria-label={label}
        onchange={onSelect}
      >
        {#each param.options ?? [] as o (o)}
          <option value={o} selected={o === selected}>{o}</option>
        {/each}
      </select>
      <CaretDown size="12" class="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 text-muted" aria-hidden="true" />
    </span>
  </label>
{:else if widget === "knob"}
  <!--
    A knob is a range input under the paint. Keeping the native element means
    Tab, the arrow keys, Home and End and the visible focus ring are the
    browser's own, not a reimplementation on a div. The rotation and the
    indicator are only decoration over a control that already works.
  -->
  <label class="flex w-16 flex-col items-center gap-1 text-xs text-muted">
    <span
      class="relative grid size-11 place-items-center rounded-full border border-line bg-surface focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-focus"
      style="--knob-angle: {angle}deg"
    >
      <span
        class="pointer-events-none absolute inset-1 rounded-full"
        aria-hidden="true"
        style="background: conic-gradient(from 225deg, var(--color-track) 0deg 270deg, transparent 270deg 360deg)"
      ></span>
      <span
        class="pointer-events-none absolute left-1/2 top-1/2 h-[42%] w-[2px] origin-bottom rounded-full bg-fg"
        aria-hidden="true"
        style="transform: translate(-50%, -100%) rotate(var(--knob-angle))"
      ></span>
      <input
        class="absolute inset-0 cursor-pointer opacity-0"
        type="range"
        min={param.min}
        max={param.max}
        step={param.step > 0 ? param.step : "any"}
        value={numeric}
        aria-label={label}
        aria-valuetext={text}
        oninput={onRange}
        onkeydown={onKnobKey}
      />
    </span>
    <!-- The value sits under the dial, not inside it: the needle sweeps the
         centre, so text there collides with it at most positions. -->
    <span class="tabular-nums text-[11px] text-fg">{text}</span>
    <span class="w-full truncate text-center text-[11px]">{label}</span>
  </label>
{:else}
  <label class="flex flex-col gap-1 text-xs text-muted">
    <span class="flex items-center justify-between gap-2">
      <span class="truncate">{label}</span>
      <span class="tabular-nums text-fg">{text}</span>
    </span>
    <input
      class="range"
      type="range"
      min={param.min}
      max={param.max}
      step={param.step > 0 ? param.step : "any"}
      value={numeric}
      style="--fill-frac: {fill}"
      aria-label={label}
      aria-valuetext={text}
      oninput={onRange}
    />
  </label>
{/if}
