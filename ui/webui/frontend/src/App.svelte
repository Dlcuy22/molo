<script lang="ts">
  import { onMount } from "svelte";
  import { get } from "svelte/store";
  import {
    Warning,
    MagnifyingGlass,
    Repeat,
    RepeatOnce,
    Shuffle,
    Eye,
    EyeSlash,
    SpeakerHigh,
    SpeakerSlash,
    SkipForward,
    SkipBack,
  } from "phosphor-svelte";
  import { connect, commands, player, ready, appSettings, spectrumConfig } from "./lib/store";
  import { displayTitle, percent } from "./lib/format";
  import { installKeyboard, shouldIgnoreTarget, type ShortcutAction } from "./lib/shortcuts";
  import { showToast } from "./lib/toast";
  import { muted, toggleMute, volume, volumeBy, VOLUME_STEP } from "./lib/volume";
  import { nextShuffleTarget } from "./lib/shuffle";
  import Transport from "./lib/components/Transport.svelte";
  import TrackInfo from "./lib/components/TrackInfo.svelte";
  import Queue from "./lib/components/Queue.svelte";
  import Spectrum from "./lib/components/Spectrum.svelte";
  import Controls from "./lib/components/Controls.svelte";
  import CommandPalette from "./lib/components/CommandPalette.svelte";
  import SettingsPanel from "./lib/components/settings/SettingsPanel.svelte";
  import Toaster from "./lib/components/Toaster.svelte";

  // The one subscription to the Go push stream. connect returns its own
  // teardown, so a hot reload does not leave a second listener behind.
  onMount(connect);

  const snap = $derived($player);
  // The generated binding types the queue as nullable; the store's EMPTY is
  // never null, but narrowing once here keeps every consumer non-null.
  const queue = $derived(snap.queue ?? []);
  const hasQueue = $derived(queue.length > 0);

  // Shuffle is optimistic like the visualizer. Two presses inside one snapshot
  // window must land as on then off, so the target is computed from this local
  // intent, not from snap.shuffled, which lags the round trip by a whole tick.
  // The intent clears once the snapshot agrees with it, at which point the
  // server value is the same thing.
  let shuffleIntent = $state<boolean | null>(null);
  $effect(() => {
    if (shuffleIntent !== null && snap.shuffled === shuffleIntent) {
      shuffleIntent = null;
    }
  });

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
  // The settings panel is opened with Ctrl+I (Cmd+I on macOS) and is a modal
  // overlay of its own, independent of the palette.
  let settingsOpen = $state(false);

  function openPalette(source: "queue" | "ytm", preview: boolean) {
    paletteSource = source;
    previewOnOpen = preview;
    paletteOpen = true;
  }

  function openYTMPalette() {
    openPalette("ytm", false);
  }

  function closePalette() {
    paletteOpen = false;
    previewOnOpen = false;
    // Closing the palette ends any audition, so the main track resumes and the
    // next open starts fresh.
    commands.previewStop();
  }

  // closePaletteOrSwitch is the Ctrl+P rule: close only when the queue source
  // is already showing, otherwise open (or switch) to it.
  function closePaletteOrSwitch(source: "queue" | "ytm") {
    if (paletteOpen && paletteSource === source) {
      closePalette();

      return;
    }
    openPalette(source, false);
  }

  // The volume bar is trailing-debounced 400ms. The engine call is not: it
  // fires on every press so the gain tracks the key without lag, and only the
  // toast waits, so holding the key shows one bar that reads its final value
  // instead of a stream of cards. The same key replaces in place, so the timer
  // resets on each press.
  let volumeToastTimer: ReturnType<typeof setTimeout> | null = null;
  function showVolumeToast() {
    if (volumeToastTimer !== null) {
      clearTimeout(volumeToastTimer);
    }
    volumeToastTimer = setTimeout(() => {
      volumeToastTimer = null;
      // volume() is the same derivation the transport draws, so the bar and the
      // slider cannot read different numbers.
      const v = volume();
      showToast({
        key: "volume",
        kind: "bar",
        label: "Volume",
        value: v,
        text: `${percent(v)}%`,
      });
    }, 400);
  }

  function showMuteToast() {
    const m = get(muted);
    showToast({
      key: "mute",
      kind: "tile",
      label: m ? "Muted" : "Unmuted",
      icon: m ? SpeakerSlash : SpeakerHigh,
    });
  }

  function showTrackToast() {
    // With nothing queued there is no track to advance to, so an art card would
    // claim a change that did not happen. The cover and title otherwise come
    // live from the Toaster; this only needs the key and the shape.
    if (queue.length === 0) {
      return;
    }
    showToast({ key: "track", kind: "art", label: "Track" });
  }

  function persistRepeat(mode: "off" | "all" | "one") {
    commands.setRepeat(mode);
    // The file is written through the settings document, since repeat outlives
    // a restart and the engine has no store of its own.
    commands.setAppSettings({
      ...$appSettings,
      playback: { ...$appSettings.playback, repeat: mode },
    });
  }

  function toggleVisualizer() {
    const on = !$appSettings.general.visualizerEnabled;
    commands.setAppSettings({
      ...$appSettings,
      general: { ...$appSettings.general, visualizerEnabled: on },
    });
    const next = { ...$spectrumConfig, enabled: on };
    spectrumConfig.set(next);
    commands.configureSpectrum(next);
  }

  // action maps a matched key to its effect and its feedback. It is the only
  // place a shortcut turns into a command, so the shortcut table stays data and
  // this stays behaviour.
  function action(a: ShortcutAction) {
    switch (a) {
      case "volume-up":
        volumeBy(VOLUME_STEP);
        showVolumeToast();

        return;
      case "volume-down":
        volumeBy(-VOLUME_STEP);
        showVolumeToast();

        return;
      case "mute":
        // The flag flips synchronously, so the toast reads the state the user
        // just toggled, not the one the engine will echo back a tick later.
        toggleMute();
        showMuteToast();

        return;
      case "next":
        commands.next();
        showTrackToast();

        return;
      case "prev":
        commands.prev();
        showTrackToast();

        return;
      case "shuffle": {
        const on = nextShuffleTarget(shuffleIntent, snap.shuffled);
        shuffleIntent = on;
        commands.shuffle(on);
        showToast({
          key: "shuffle",
          kind: "tile",
          label: on ? "Shuffle on" : "Shuffle off",
          icon: Shuffle,
        });

        return;
      }
      case "visualizer": {
        const on = !$appSettings.general.visualizerEnabled;
        toggleVisualizer();
        showToast({
          key: "visualizer",
          kind: "tile",
          label: on ? "Visualizer on" : "Visualizer off",
          icon: on ? Eye : EyeSlash,
        });

        return;
      }
      case "repeat-all":
        persistRepeat("all");
        showToast({ key: "repeat", kind: "tile", label: "Repeat all", icon: Repeat });

        return;
      case "repeat-one":
        persistRepeat("one");
        showToast({ key: "repeat", kind: "tile", label: "Repeat one", icon: RepeatOnce });

        return;
    }
  }

  onMount(() =>
    installKeyboard({
      isModalOpen: () => paletteOpen || settingsOpen,
      isPaletteOpen: () => paletteOpen,
      // A focused field typing is not a shortcut target. The rule for range and
      // other non-text inputs lives in shouldIgnoreTarget.
      isEditableFocused: () => shouldIgnoreTarget(document.activeElement),
      openSettings: () => (settingsOpen = !settingsOpen),
      openPalette,
      closePaletteOrSwitch,
      action,
    }),
  );
</script>

<!--
  Layout: the spectrum spans the full width because it is the widest thing the
  app draws, then the queue and the controls share the row below it, and the
  transport anchors the bottom. One focal point per band of the window: the
  track title, then the spectrum, then the play button.
-->
<div class="flex h-full flex-col bg-bg text-fg">
  <!-- main scrolls only in the stacked (narrow) layout, where the columns
       cannot fit side by side and one page scroll is the honest answer. Side by
       side it cannot scroll: the queue and the right column each scroll inside
       their own panel, so the window never grows a second scrollbar next to the
       queue's. -->
  <main class="scroll-thin flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4 lg:overflow-y-visible">
    <TrackInfo {snap} />

    {#if $spectrumConfig.enabled}
      <Spectrum />
    {/if}

    <!-- Side by side, the row is clamped to the space the window has and each
         column scrolls inside itself. Stacked, it keeps a floor so the queue
         stays usable and main takes the overflow. -->
    <div class="flex min-h-56 flex-1 flex-col gap-3 lg:min-h-0 lg:flex-row">
      <section
        class="flex min-h-56 flex-1 flex-col rounded-[6px] border border-line bg-surface lg:min-h-0"
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
              onclick={() => openPalette("queue", false)}
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

      <!-- Side by side, the panel stack scrolls inside itself rather than
           stretching the row. Stacked, it takes its natural height and main
           carries the scroll, so the two scrollbars never nest. -->
      <section
        class="scroll-thin flex w-full flex-col gap-3 lg:min-h-0 lg:w-[26rem] lg:shrink-0 lg:overflow-y-auto"
      >
        <div class="rounded-[6px] border border-line bg-surface p-3">
          <Controls decoderPref={snap.decoderPref} backend={snap.backend} />
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

  <SettingsPanel open={settingsOpen} onclose={() => (settingsOpen = false)} />

  <!-- One toast host for the whole app. It stays mounted and renders nothing
       until a toast arrives, so it costs a subscription and no layout. -->
  <Toaster />
</div>
