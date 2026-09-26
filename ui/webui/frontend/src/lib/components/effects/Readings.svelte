<script lang="ts">
  import type { Reading } from "../../effect-types";
  import LevelMeter from "./LevelMeter.svelte";
  import { readingFraction, readingValue } from "./readings-helpers";

  let {
    readings,
    meters,
  }: { readings: Reading[]; meters: Record<string, number> | null } = $props();

  // A level or scalar draws its own value; a gain reduction draws its inverted
  // fraction, so 0 reduction reads full and the bar shrinks as gain is removed.
  function barValue(r: Reading): number | null {
    const value = readingValue(meters, r.key);
    if (r.kind !== "gain-reduction" || value === null) {
      return value;
    }
    const frac = readingFraction(value, r);

    return frac === null ? null : r.min + frac * (r.max - r.min);
  }
</script>

<!--
  A plain labelled list in normal flow, so every reading is reachable; nothing
  to activate, so it takes no tab stop.
-->
{#if readings.length > 0}
  <ul class="flex flex-col gap-2" aria-label="Readings">
    {#each readings as r (r.key)}
      <li>
        <LevelMeter db={barValue(r)} label={r.label} min={r.min} max={r.max} unit={r.unit} />
      </li>
    {/each}
  </ul>
{/if}
