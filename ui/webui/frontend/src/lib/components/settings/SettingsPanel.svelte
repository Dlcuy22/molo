<script lang="ts">
  import { appSettings, commands } from "../../store";
  import type { CrossfadeSettings } from "../../../../bindings/github.com/dlcuy22/molo/ui/webui/models";
  import Switch from "./Switch.svelte";
  import Slider from "./Slider.svelte";
  import SettingRow from "./SettingRow.svelte";

  // The settings panel: a modal overlay opened with Ctrl+I, with a tab per
  // category. It matches the command palette's frame (backdrop, a centred
  // surface, Escape to close) but is a square box: the panel is a utility
  // surface, not a floating card, so it carries no radius.
  //
  // Only the Playback tab carries a setting today: Crossfade. General and
  // Stream are present as the categories the panel is meant to hold, and say so
  // rather than inventing controls. Every row is the same shape (label, note,
  // control), so a new setting is one row and no new layout.
  let { open, onclose }: { open: boolean; onclose: () => void } = $props();

  type Tab = "general" | "playback" | "stream";
  let tab = $state<Tab>("playback");
  let panelEl = $state<HTMLDivElement | null>(null);

  const crossfade = $derived($appSettings.playback.crossfade);

  // Reset to the first tab with content each time the panel opens, so it does
  // not reopen on an empty tab the user left it on.
  let wasOpen = false;
  $effect(() => {
    if (open && !wasOpen) {
      tab = "playback";
      panelEl?.focus();
    }
    wasOpen = open;
  });

  // setCrossfade writes the whole settings document back with one field
  // changed. The panel holds no draft state: the store is the one place a
  // setting lives, and the Go side persists what it is given.
  function setCrossfade(patch: Partial<CrossfadeSettings>) {
    commands.setAppSettings({
      ...$appSettings,
      playback: {
        ...$appSettings.playback,
        crossfade: { ...$appSettings.playback.crossfade, ...patch },
      },
    });
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      e.preventDefault();
      onclose();
    }
  }
</script>

{#if open}
  <div
    class="fixed inset-0 z-50 flex justify-center bg-black/50 p-4"
    role="presentation"
    onclick={(e) => {
      if (e.target === e.currentTarget) onclose();
    }}
  >
    <div
      bind:this={panelEl}
      class="mt-[8vh] flex max-h-[70vh] w-full max-w-lg flex-col overflow-hidden border border-line bg-surface"
      role="dialog"
      aria-modal="true"
      aria-label="Settings"
      tabindex="-1"
      onkeydown={onKeydown}
    >
      <!-- Tabs across the top. A real tablist: each tab names a category, and
           the panel below shows only the selected one. -->
      <div
        class="flex shrink-0 items-center gap-1 border-b border-line px-2 py-1.5"
        role="tablist"
        aria-label="Settings categories"
      >
        {#each [["general", "General"], ["playback", "Playback"], ["stream", "Stream"]] as const as [id, label] (id)}
          <button
            class="rounded-[4px] px-2.5 py-1 text-xs transition-colors"
            class:bg-selected={tab === id}
            class:text-fg={tab === id}
            class:text-muted={tab !== id}
            role="tab"
            aria-selected={tab === id}
            aria-controls="settings-panel-body"
            onclick={() => (tab = id)}
          >
            {label}
          </button>
        {/each}
      </div>

      <div
        id="settings-panel-body"
        class="scroll-thin min-h-0 flex-1 overflow-y-auto px-4 py-2"
        role="tabpanel"
      >
        {#if tab === "playback"}
          <div class="divide-y divide-line">
            <SettingRow
              label="Crossfade"
              description="Fade the end of a track into the start of the next."
            >
              {#snippet control()}
                <Switch
                  checked={crossfade.enabled}
                  label="Crossfade"
                  onchange={(enabled) => setCrossfade({ enabled })}
                />
              {/snippet}
            </SettingRow>

            <div class="py-2.5" class:opacity-40={!crossfade.enabled}>
              <Slider
                label="Duration"
                value={crossfade.durationMs / 1000}
                min={1}
                max={12}
                step={1}
                unit="s"
                disabled={!crossfade.enabled}
                format={(v) => `${v} s`}
                onchange={(seconds) => setCrossfade({ durationMs: Math.round(seconds) * 1000 })}
              />
            </div>

            <SettingRow
              label="Skip for consecutive album tracks"
              description="Leave tracks from the same album unblended, the way the album was sequenced."
            >
              {#snippet control()}
                <Switch
                  checked={crossfade.skipSameAlbum}
                  label="Skip for consecutive album tracks"
                  onchange={(skipSameAlbum) => setCrossfade({ skipSameAlbum })}
                />
              {/snippet}
            </SettingRow>
          </div>
        {:else if tab === "stream"}
          <p class="px-1 py-6 text-center text-xs text-muted">
            No stream settings yet. YouTube Music and network options will live here.
          </p>
        {:else}
          <p class="px-1 py-6 text-center text-xs text-muted">
            No general settings yet.
          </p>
        {/if}
      </div>

      <div class="flex shrink-0 items-center gap-3 border-t border-line px-4 py-2 text-[11px] text-muted">
        <span>Settings apply as you change them.</span>
        <span class="ml-auto"><kbd class="font-sans text-fg">Esc</kbd> to close</span>
      </div>
    </div>
  </div>
{/if}
