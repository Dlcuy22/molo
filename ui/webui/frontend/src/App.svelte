<script lang="ts">
  import { onMount } from "svelte";
  import { Warning, MagnifyingGlass } from "phosphor-svelte";
  import { connect, commands, player, ready, spectrumConfig } from "./lib/store";
  import { displayTitle } from "./lib/format";
  import Transport from "./lib/components/Transport.svelte";
  import TrackInfo from "./lib/components/TrackInfo.svelte";
  import Queue from "./lib/components/Queue.svelte";
  import Spectrum from "./lib/components/Spectrum.svelte";
  import Controls from "./lib/components/Controls.svelte";
  import DiscordPanel from "./lib/components/DiscordPanel.svelte";
  import CommandPalette from "./lib/components/CommandPalette.svelte";

  // The one subscription to the Go push stream. connect returns its own
  // teardown, so a hot reload does not leave a second listener behind.
  onMount(connect);

  const snap = $derived($player);
  // The generated binding types the queue as nullable; the store's EMPTY is
  // never null, but narrowing once here keeps every consumer non-null.
  const queue = $derived(snap.queue ?? []);
  const hasQueue = $derived(queue.length > 0);

  // Ctrl+P (Cmd+P on macOS) opens the queue search, the way it opens a file
  // switcher in an editor. The browser's own Ctrl+P prints, so the default is
  // suppressed while the app has focus. Ctrl+Alt+P opens it straight into
  // preview mode, which is the one audition shortcut worth having outside the
  // palette. Ctrl+F opens the same palette against YouTube Music instead of the
  // queue; the webview's own find-in-page is suppressed the same way.
  let paletteOpen = $state(false);
  let paletteSource = $state<"queue" | "ytm">("queue");
  let previewOnOpen = $state(false);
  const isMac = /mac/i.test(navigator.platform || navigator.userAgent);
  const shortcutLabel = isMac ? "⌘P" : "Ctrl P";
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      const key = e.key.toLowerCase();
      if ((e.ctrlKey || e.metaKey) && e.altKey && key === "p") {
        e.preventDefault();
        if (paletteOpen) {
          // Already open: the palette owns this chord and toggles its own
          // preview mode, so the app must not double-handle it.
          return;
        }
        paletteSource = "queue";
        previewOnOpen = true;
        paletteOpen = true;

        return;
      }
      if ((e.ctrlKey || e.metaKey) && !e.altKey && key === "f") {
        e.preventDefault();
        paletteSource = "ytm";
        previewOnOpen = false;
        paletteOpen = true;

        return;
      }
      if ((e.ctrlKey || e.metaKey) && !e.altKey && key === "p") {
        e.preventDefault();
        // Ctrl+P always means the queue search. Open in that source, or close
        // when it is already the one showing; from the YouTube source it is a
        // switch back, not a close.
        if (paletteOpen && paletteSource === "queue") {
          closePalette();

          return;
        }
        paletteSource = "queue";
        previewOnOpen = false;
        paletteOpen = true;
      }
    };
    window.addEventListener("keydown", onKey);

    return () => window.removeEventListener("keydown", onKey);
  });

  function openPalette(withPreview: boolean) {
    paletteSource = "queue";
    previewOnOpen = withPreview;
    paletteOpen = true;
  }

  function openYTMPalette() {
    paletteSource = "ytm";
    previewOnOpen = false;
    paletteOpen = true;
  }

  function closePalette() {
    paletteOpen = false;
    previewOnOpen = false;
    // Closing the palette ends any audition, so the main track resumes and the
    // next open starts fresh.
    commands.previewStop();
  }
</script>

<!--
  Layout: the spectrum spans the full width because it is the widest thing the
  app draws, then the queue and the controls share the row below it, and the
  transport anchors the bottom. One focal point per band of the window: the
  track title, then the spectrum, then the play button.
-->
<div class="flex h-full flex-col bg-bg text-fg">
  <!-- main is the window's scroll surface: the queue scrolls in its own panel,
       and this outer scroll keeps the panels below the fold reachable. -->
  <main class="scroll-thin flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4">
    <TrackInfo {snap} />

    {#if $spectrumConfig.enabled}
      <Spectrum />
    {/if}

    <!-- The row grows to its tallest column instead of being clamped to the
         viewport, so a short window scrolls rather than painting over the
         transport. -->
    <div class="flex flex-1 flex-col gap-3 lg:flex-row">
      <!-- min-h-56 keeps the queue's own scroll floor usable on a short window. -->
      <section
        class="flex min-h-56 flex-1 flex-col rounded-[6px] border border-line bg-surface"
      >
        <div class="flex items-center justify-between gap-2 px-3 pb-1 pt-3">
          <h2 class="text-xs font-medium text-muted">Queue</h2>
          <div class="flex items-center gap-2">
            {#if hasQueue}
              <span class="text-[11px] tabular-nums text-muted">
                {snap.queueIdx + 1} / {queue.length}
              </span>
            {/if}
            <!-- The shortcut is the fast path; these buttons are the
                 discoverable one, so neither search is reachable only by
                 knowing a key. The queue search keeps Ctrl+P; YouTube Music
                 gets its own entry rather than a hidden mode. -->
            <button
              class="flex items-center gap-1.5 rounded-[4px] border border-line bg-surface px-2 py-1 text-[11px] text-muted transition-colors hover:bg-hover hover:text-fg"
              onclick={() => openPalette(false)}
            >
              <MagnifyingGlass size="12" aria-hidden="true" />
              Search
              <kbd class="font-sans text-fg">{shortcutLabel}</kbd>
            </button>
            <button
              class="flex items-center gap-1.5 rounded-[4px] border border-line bg-surface px-2 py-1 text-[11px] text-muted transition-colors hover:bg-hover hover:text-fg"
              onclick={() => openYTMPalette()}
            >
              <MagnifyingGlass size="12" aria-hidden="true" />
              YouTube
              <kbd class="font-sans text-fg">{isMac ? "⌘F" : "Ctrl F"}</kbd>
            </button>
          </div>
        </div>
        <div class="min-h-0 flex-1">
          <Queue {queue} queueIdx={snap.queueIdx} />
        </div>
      </section>

      <section class="flex w-full flex-col gap-3 lg:w-[26rem] lg:shrink-0">
        <div class="rounded-[6px] border border-line bg-surface p-3">
          <Controls decoderPref={snap.decoderPref} backend={snap.backend} />
        </div>
        <div class="rounded-[6px] border border-line bg-surface p-3">
          <DiscordPanel />
        </div>
        <div class="rounded-[6px] border border-line bg-surface p-3">
          <div class="flex items-center justify-between gap-2">
            <div class="min-w-0">
              <h2 class="text-xs font-medium text-muted">Effects</h2>
              <p class="truncate text-[11px] text-muted">
                Build a chain of effects in its own window.
              </p>
            </div>
            <button
              class="shrink-0 rounded-[4px] border border-line bg-surface px-2.5 py-1 text-xs text-fg transition-colors hover:bg-hover focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
              onclick={() => commands.openEffectWindow()}
            >
              Open
            </button>
          </div>
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

  <CommandPalette
    open={paletteOpen}
    source={paletteSource}
    {queue}
    preview={snap.preview}
    startPreview={previewOnOpen}
    onclose={closePalette}
  />
</div>
