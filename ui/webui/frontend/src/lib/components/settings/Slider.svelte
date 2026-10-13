<script lang="ts">
  import { fillFrac } from "../../format";

  // Slider: a range input with the shared `.range` styling. The caller supplies
  // the bounds, the step and the unit label, so the same control serves a
  // seconds setting and any future numeric one. It reads the value it is given
  // rather than holding its own, so the panel's store stays the single source.
  let {
    value,
    min,
    max,
    step,
    label,
    unit = "",
    disabled = false,
    format,
    onchange,
  }: {
    value: number;
    min: number;
    max: number;
    step: number;
    label: string;
    unit?: string;
    disabled?: boolean;
    format?: (value: number) => string;
    onchange: (value: number) => void;
  } = $props();

  const shown = $derived(format ? format(value) : `${value}${unit}`);
</script>

<label class="flex w-full flex-col gap-1 text-xs text-muted">
  <span class="flex items-center justify-between gap-2">
    <span class="truncate">{label}</span>
    <span class="tabular-nums text-fg">{shown}</span>
  </span>
  <input
    class="range disabled:opacity-40"
    type="range"
    {min}
    {max}
    {step}
    {disabled}
    {value}
    style="--fill-frac: {fillFrac(value, min, max)}"
    aria-label={label}
    aria-valuetext={shown}
    oninput={(e) => onchange(Number(e.currentTarget.value))}
  />
</label>
