<script lang="ts">
  import { onMount } from "svelte";
  import { Warning } from "phosphor-svelte";
  import { connect, player, ready } from "./lib/store";
  import { displayTitle } from "./lib/format";
  import Transport from "./lib/components/Transport.svelte";
  import TrackInfo from "./lib/components/TrackInfo.svelte";
  import Queue from "./lib/components/Queue.svelte";
  import Spectrum from "./lib/components/Spectrum.svelte";
  import Controls from "./lib/components/Controls.svelte";

  // The one subscription to the Go push stream. connect returns its own
  // teardown, so a hot reload does not leave a second listener behind.
  onMount(connect);

  const snap = $derived($player);
  const hasQueue = $derived(snap.queue.length > 0);
</script>

<!--
  Layout: the spectrum spans the full width because it is the widest thing the
  app draws, then the queue and the controls share the row below it, and the
  transport anchors the bottom. One focal point per band of the window: the
  track title, then the spectrum, then the play button.
-->
<div class="flex h-full flex-col bg-bg text-fg">
  <main class="flex min-h-0 flex-1 flex-col gap-3 p-4">
    <TrackInfo {snap} />

    <Spectrum />

    <!-- min-h-0 lets the queue scroll inside the flex row instead of growing
         the page. -->
    <div class="flex min-h-0 flex-1 flex-col gap-3 lg:flex-row">
      <section class="flex min-h-0 flex-1 flex-col rounded-[6px] border border-line bg-surface">
        <div class="flex items-center justify-between px-3 pb-1 pt-3">
          <h2 class="text-xs font-medium text-muted">Queue</h2>
          {#if hasQueue}
            <span class="text-[11px] tabular-nums text-muted">
              {snap.queueIdx + 1} / {snap.queue.length}
            </span>
          {/if}
        </div>
        <div class="min-h-0 flex-1">
          <Queue queue={snap.queue} queueIdx={snap.queueIdx} />
        </div>
      </section>

      <section class="flex w-full flex-col gap-3 lg:w-[26rem] lg:shrink-0">
        <div class="rounded-[6px] border border-line bg-surface p-3">
          <Controls decoderPref={snap.decoderPref} backend={snap.backend} />
        </div>
      </section>
    </div>
  </main>

  <!-- Status and transport share the bottom bar. An error replaces the status
       line rather than stacking, because only one of them is ever the reason
       the user should look down here. -->
  <footer class="flex flex-col gap-2 border-t border-line bg-surface px-4 py-3">
    {#if snap.error}
      <!-- The error colour is only ~3:1 on this surface, so it is the icon's
           job here and the message itself uses the readable foreground. The
           colour still carries the state; the words stay legible. -->
      <p class="flex items-center gap-2 text-xs text-fg" role="alert">
        <Warning size="15" weight="fill" class="shrink-0 text-danger" />
        <span class="min-w-0 flex-1 truncate" title={snap.error}>{snap.error}</span>
      </p>
    {:else if !$ready}
      <p class="text-xs text-muted" role="status">Starting the player…</p>
    {:else}
      <p class="text-xs text-muted">
        {#if snap.state === "playing"}
          Playing {displayTitle(snap.title, snap.path)}
        {:else if snap.state === "paused"}
          Paused
        {:else if snap.state === "stopped"}
          Stopped
        {:else}
          Ready
        {/if}
      </p>
    {/if}

    <Transport {snap} />
  </footer>
</div>
