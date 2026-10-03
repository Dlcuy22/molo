<script lang="ts">
  import { CaretDown, FolderOpen, Plus } from "phosphor-svelte";
  import { busy, commands, options, spectrumConfig, spectrumSchema } from "../store";
  import { fillFrac } from "../format";
import { fftLabel } from "../spectrum-fft";
  import type { Param } from "../../../bindings/github.com/dlcuy22/molo/ui/webui/internal/spectrum/models";
  import type { CodecOption } from "../../../bindings/github.com/dlcuy22/molo/ui/webui/models";

  let { decoderPref, backend }: { decoderPref: string; backend: string } = $props();

  let open = $state<"codec" | "visualizer" | null>(null);

  function onCodec(e: Event) {
    commands.setCodec((e.currentTarget as HTMLSelectElement).value);
  }

  function onBackend(e: Event) {
    commands.setBackend((e.currentTarget as HTMLSelectElement).value);
  }

  function spectrumValue(p: Param): number {
    const cfg = $spectrumConfig;
    if (p.key === "bars") return cfg.bars;
    if (p.key === "fft") return cfg.fft;
    if (p.key === "minHz") return cfg.minHz;
    if (p.key === "maxHz") return cfg.maxHz;

    return p.default;
  }

  function onSpectrum(p: Param, e: Event) {
    const raw = Number((e.currentTarget as HTMLInputElement).value);
    const next = { ...$spectrumConfig };
    if (p.key === "bars") next.bars = raw;
    if (p.key === "fft") next.fft = raw;
    if (p.key === "minHz") next.minHz = raw;
    if (p.key === "maxHz") next.maxHz = raw;
    // Optimistic: the store drives both thumb and fill, and the engine only
    // confirms on the next snapshot tick, so without this the thumb fights
    // the pointer and the fill lags the drag. These knobs cannot fail
    // server-side (low cut tops out below high cut's floor, and the transform
    // size falls back to the default if it is not on the list), so there is no
    // rollback path to maintain.
    spectrumConfig.set(next);
    commands.configureSpectrum(next);
  }

  function onSpectrumChoice(p: Param, e: Event) {
    const raw = Number((e.currentTarget as HTMLSelectElement).value);
    const next = { ...$spectrumConfig };
    if (p.key === "fft") next.fft = raw;
    spectrumConfig.set(next);
    commands.configureSpectrum(next);
  }

  // fftLabel names a transform size the way the choice is chosen: the resolution
  // it buys is what distinguishes them, not the sample count.
</script>

<!--
  Controls: the two engine choices (decoder, backend) and the display-only
  visualizer shape. They are grouped behind one row rather than spread across
  the window, because none of them is the primary action.

  The decoder note states the one timing fact the engine enforces: SwapDecoder
  reopens the current track in place, so the change is live but the position is
  re-read. Saying so beats letting the user think it silently did nothing.
-->
<div class="flex flex-col gap-2">
  <div class="flex flex-wrap items-center gap-2">
    <label class="flex items-center gap-2 text-xs text-muted">
      Decoder
      <span class="relative">
        <select
          class="appearance-none rounded-[4px] border border-line bg-surface py-1.5 pl-2.5 pr-7 text-xs text-fg transition-colors hover:bg-hover"
          onchange={onCodec}
        >
          {#each $options.codecs as c (c.name)}
            <!-- selected per option rather than a select value binding: the
                 Auto option's value is the empty string, which a value binding
                 does not reliably match. -->
            <option value={c.name} selected={c.name === decoderPref} class="bg-surface text-fg">
              {c.label}
            </option>
          {/each}
        </select>
        <CaretDown
          size="12"
          class="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 text-muted"
        />
      </span>
    </label>

    <label class="flex items-center gap-2 text-xs text-muted">
      Output
      <span class="relative">
        <select
          class="appearance-none rounded-[4px] border border-line bg-surface py-1.5 pl-2.5 pr-7 text-xs text-fg transition-colors hover:bg-hover"
          onchange={onBackend}
        >
          {#each $options.backends as b (b)}
            <option value={b} selected={b === backend} class="bg-surface text-fg">
              {b === "" ? "Auto" : b}
            </option>
          {/each}
        </select>
        <CaretDown
          size="12"
          class="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 text-muted"
        />
      </span>
    </label>

    <button
      class="ml-auto flex items-center gap-1.5 rounded-[4px] border border-line bg-surface px-2.5 py-1.5 text-xs text-fg transition-colors hover:bg-hover"
      class:bg-selected={open === "visualizer"}
      aria-expanded={open === "visualizer"}
      onclick={() => (open = open === "visualizer" ? null : "visualizer")}
    >
      Visualizer
      <CaretDown size="12" class="text-muted" />
    </button>
  </div>

  {#if open === "visualizer"}
    <!-- The panel is built from the schema the engine reports, so a new knob is
         one Go entry and no frontend change. -->
    <div class="flex flex-wrap items-end gap-x-5 gap-y-3 rounded-[6px] border border-line bg-surface p-3">
      {#each $spectrumSchema as p (p.key)}
        {#if p.kind === "choice"}
          <!-- A choice has no continuous range, so the panel renders the set the
               schema names instead of a slider over a span it cannot take. -->
          <label class="flex w-40 flex-col gap-1 text-xs text-muted">
            {p.label}
            <span class="relative">
              <select
                class="appearance-none w-full rounded-[4px] border border-line bg-surface py-1.5 pl-2.5 pr-7 text-xs text-fg transition-colors hover:bg-hover"
                value={spectrumValue(p)}
                onchange={(e) => onSpectrumChoice(p, e)}
              >
                {#each p.choices ?? [] as c (c)}
                  <option value={c} class="bg-surface text-fg">{fftLabel(c)}</option>
                {/each}
              </select>
              <CaretDown
                size="12"
                class="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 text-muted"
              />
            </span>
          </label>
        {:else}
          <label class="flex w-40 flex-col gap-1 text-xs text-muted">
            <span class="flex items-center justify-between">
              {p.label}
              <span class="tabular-nums text-fg">{spectrumValue(p)}</span>
            </span>
            <input
              class="range"
              type="range"
              min={p.min}
              max={p.max}
              step={p.step}
              value={spectrumValue(p)}
              style="--fill-frac: {fillFrac(spectrumValue(p), p.min, p.max)}"
              oninput={(e) => onSpectrum(p, e)}
            />
          </label>
        {/if}
      {/each}
      <p class="w-full text-[11px] text-muted">
        Display only. The transform never touches playback.
      </p>
    </div>
  {/if}

  <div class="flex flex-wrap items-center gap-2">
    <button
      class="flex items-center gap-1.5 rounded-[4px] bg-surface px-3 py-1.5 text-xs text-fg transition-colors hover:bg-hover disabled:text-disabled"
      disabled={$busy}
      onclick={() => commands.openFiles()}
    >
      <Plus size="14" weight="bold" />
      Add files
    </button>
    <button
      class="flex items-center gap-1.5 rounded-[4px] bg-surface px-3 py-1.5 text-xs text-fg transition-colors hover:bg-hover disabled:text-disabled"
      disabled={$busy}
      onclick={() => commands.openFolder()}
    >
      <FolderOpen size="14" />
      Add folder
    </button>
    {#if $busy}
      <span class="text-xs text-muted" role="status">Reading…</span>
    {/if}
  </div>
</div>
