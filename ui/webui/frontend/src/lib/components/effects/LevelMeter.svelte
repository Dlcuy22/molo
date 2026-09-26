<script lang="ts">
  import { meterFraction, meterText, clamp } from "./effects-helpers";

  // A level meter for one reading. It draws a value, never a live stream: the
  // snapshot carries the number at the bridge's own rate. The unit travels with
  // the reading, so a gain reduction or a scalar is not printed as dBFS.
  let {
    db,
    label,
    min = -60,
    max = 0,
    unit = "dB",
  }: { db: number | null; label: string; min?: number; max?: number; unit?: string } = $props();

  // A null reading means the effect does not meter, which must draw as an inert
  // track, not a zero bar, so silence and "no meter" stay distinguishable.
  const meters = $derived(db !== null && Number.isFinite(db));
  const frac = $derived(meterFraction(db, min, max));
  const fillPct = $derived(clamp(frac * 100, 0, 100));
</script>

<!--
  The meter is a fill on the track colour, with the accent as the fill. The
  number is in text next to it, so the bar is repetition for the eye rather than
  the only carrier of the value, and it is hidden from a screen reader.
  The label cell has a minimum width and can grow, so a longer name such as
  "Gain Reduction" is printed in full while the short In/Out rows keep their
  width.
-->
<div class="flex items-center gap-2">
  <span class="min-w-7 shrink-0 text-[11px] text-muted">{label}</span>
  <div class="relative h-2 min-w-0 flex-1 overflow-hidden rounded-[2px] bg-track" aria-hidden="true">
    {#if meters}
      <div class="h-full rounded-[2px] bg-accent transition-[width] duration-100 ease-linear" style="width: {fillPct}%">
      </div>
    {/if}
  </div>
  <span class="w-14 shrink-0 text-right text-[11px] tabular-nums text-muted">
    {meters ? meterText(db, unit) : ""}
  </span>
  <span class="sr-only">{label} {meterText(db, unit)}</span>
</div>
