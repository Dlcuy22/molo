<script lang="ts">
  import { CaretDown, FolderOpen, Plus } from "phosphor-svelte";
  import { busy, commands, options } from "../store";

  let { decoderPref, backend }: { decoderPref: string; backend: string } = $props();

  function onCodec(e: Event) {
    commands.setCodec((e.currentTarget as HTMLSelectElement).value);
  }

  function onBackend(e: Event) {
    commands.setBackend((e.currentTarget as HTMLSelectElement).value);
  }
</script>

<!--
  Controls: the two engine choices (decoder, backend) and the two add actions.
  The visualizer shape lives in the Ctrl+I settings panel instead, so this row
  stays two selects wide and the panel stack on the right keeps a tidy column.

  The decoder note states the one timing fact the engine enforces: SwapDecoder
  reopens the current track in place, so the change is live but the position is
  re-read. Saying so beats letting the user think it silently did nothing.
-->
<div class="flex flex-col gap-2">
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
  </div>
</div>
