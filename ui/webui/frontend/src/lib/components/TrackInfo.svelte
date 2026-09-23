<script lang="ts">
  import { MusicNotes } from "phosphor-svelte";
  import { displayTitle, subtitle } from "../format";
  import type { Snapshot } from "../../../bindings/github.com/dlcuy22/player/ui/webui/models";

  let { snap }: { snap: Snapshot } = $props();

  const title = $derived(displayTitle(snap.title, snap.path));
  const sub = $derived(subtitle(snap.artist, snap.album));

  // The one-line technical identity, shown only when the engine has reported
  // it: codec, container and the canonical rate. These are the facts a listener
  // checks when choosing a decoder, so they sit next to the choice.
  const facts = $derived(
    [
      snap.codec,
      snap.container,
      snap.sampleRate > 0 ? `${(snap.sampleRate / 1000).toFixed(1)} kHz` : "",
      snap.channels > 0 ? (snap.channels === 1 ? "mono" : "stereo") : "",
    ].filter((s) => s !== ""),
  );
</script>

<!--
  Now playing: the current track's identity. The focal point is the title;
  everything else recedes to the muted token so the hierarchy survives a long
  filename.
-->
<div class="flex min-w-0 items-center gap-3">
  <div
    class="grid size-12 shrink-0 place-items-center rounded-[6px] border border-line bg-surface"
    aria-hidden="true"
  >
    <MusicNotes size="22" class="text-muted" />
  </div>

  <div class="min-w-0 flex-1">
    {#if title === ""}
      <p class="truncate text-base text-muted">Nothing playing</p>
      <p class="truncate text-xs text-muted">Add files to get started</p>
    {:else}
      <p class="truncate text-base font-medium text-fg" title={snap.path}>
        {title}
      </p>
      {#if sub !== ""}
        <p class="truncate text-xs text-muted">{sub}</p>
      {/if}
      {#if facts.length > 0}
        <p class="mt-1 flex flex-wrap gap-x-2 truncate text-[11px] text-muted">
          {#each facts as fact, i (fact + i)}
            {#if i > 0}<span aria-hidden="true">·</span>{/if}<span>{fact}</span>
          {/each}
        </p>
      {/if}
    {/if}
  </div>
</div>
