<script lang="ts">
  import { appSettings, commands, discordStatus, spectrumConfig, spectrumSchema } from "../../store";
  import type { CrossfadeSettings, SpectrumConfig } from "../../../../bindings/github.com/dlcuy22/molo/ui/webui/models";
  import type { Param } from "../../../../bindings/github.com/dlcuy22/molo/ui/webui/internal/spectrum/models";
  import { fftLabel } from "../../spectrum-fft";
  import DiscordCard from "../DiscordCard.svelte";
  import Switch from "./Switch.svelte";
  import Slider from "./Slider.svelte";
  import Picker from "./Picker.svelte";
  import SettingRow from "./SettingRow.svelte";

  // The settings panel: a modal overlay opened with Ctrl+I, with a tab per
  // category. It matches the command palette's frame (backdrop, a centred
  // surface, Escape to close) but is a square box: the panel is a utility
  // surface, not a floating card, so it carries no radius.
  //
  // Playback carries Crossfade; General carries the visualizer, which is a
  // display choice rather than a playback one; Integration carries Discord Rich
  // Presence. Stream is present as the category the panel is meant to hold and
  // says so rather than inventing controls. Every row is the same shape (label,
  // note, control), so a new setting is one row and no new layout.
  let { open, onclose }: { open: boolean; onclose: () => void } = $props();

  type Tab = "general" | "playback" | "integration" | "stream";
  let tab = $state<Tab>("playback");
  let panelEl = $state<HTMLDivElement | null>(null);

  const crossfade = $derived($appSettings.playback.crossfade);

  // The Discord status line and toggle read the store the same way the panel
  // read them when they lived in the main window's right column.
  const discordStatusLine = $derived(
    !$discordStatus.enabled
      ? "Off"
      : $discordStatus.connected
        ? "Connected to Discord"
        : "Waiting for Discord",
  );

  function toggleDiscord() {
    commands.setDiscordEnabled(!$discordStatus.enabled);
  }

  // spectrumValue reads one visualizer knob. The panel is built from the schema
  // the engine reports, so a new knob is one Go entry and no frontend change.
  function spectrumValue(p: Param): number {
    const cfg = $spectrumConfig;
    if (p.key === "bars") return cfg.bars;
    if (p.key === "fft") return cfg.fft;
    if (p.key === "minHz") return cfg.minHz;
    if (p.key === "maxHz") return cfg.maxHz;

    return p.default;
  }

  // The spectrum setters are optimistic like the crossfade ones: the store
  // drives the control, and the engine confirms on the next snapshot tick.
  // These knobs cannot fail server-side (low cut tops out below high cut's
  // floor, and the transform size falls back to the default if it is not on
  // the list), so there is no rollback path to maintain.
  function setSpectrum(patch: Partial<SpectrumConfig>) {
    const next = { ...$spectrumConfig, ...patch };
    spectrumConfig.set(next);
    commands.configureSpectrum(next);
  }

  // onSpectrumParam writes one schema knob back. A knob the engine adds needs a
  // matching line here, the same as spectrumValue above.
  function onSpectrumParam(p: Param, value: number) {
    const patch: Partial<SpectrumConfig> = {};
    if (p.key === "bars") patch.bars = value;
    if (p.key === "fft") patch.fft = value;
    if (p.key === "minHz") patch.minHz = value;
    if (p.key === "maxHz") patch.maxHz = value;
    setSpectrum(patch);
  }

  // Reset to the default tab each time the panel opens, so it does not reopen
  // on a tab the user left it on.
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
        {#each [["general", "General"], ["playback", "Playback"], ["integration", "Integration"], ["stream", "Stream"]] as const as [id, label] (id)}
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
        {:else if tab === "integration"}
          <div class="divide-y divide-line">
            <SettingRow
              label="Discord Rich Presence"
              description={discordStatusLine}
            >
              {#snippet control()}
                <Switch
                  checked={$discordStatus.enabled}
                  label="Discord Rich Presence"
                  onchange={toggleDiscord}
                />
              {/snippet}
            </SettingRow>

            <div class="py-2.5">
              <DiscordCard />
            </div>
          </div>
        {:else if tab === "stream"}
          <p class="px-1 py-6 text-center text-xs text-muted">
            No stream settings yet. YouTube Music and network options will live here.
          </p>
        {:else}
          <div class="divide-y divide-line">
            <SettingRow
              label="Visualizer"
              description="Draw the spectrum under the track title."
            >
              {#snippet control()}
                <Switch
                  checked={$spectrumConfig.enabled}
                  label="Visualizer"
                  onchange={(enabled) => setSpectrum({ enabled })}
                />
              {/snippet}
            </SettingRow>

            <!-- The shape knobs are one group, dimmed together when the
                 visualizer is off, because none of them changes anything the
                 user can see while it is hidden. -->
            <div class="grid grid-cols-2 gap-x-4 gap-y-3 py-2.5" class:opacity-40={!$spectrumConfig.enabled}>
              {#each $spectrumSchema as p (p.key)}
                {#if p.kind === "choice"}
                  <!-- A choice has no continuous range, so the set the schema
                       names is rendered instead of a slider over a span it
                       cannot take. -->
                  <Picker
                    label={p.label}
                    value={String(spectrumValue(p))}
                    disabled={!$spectrumConfig.enabled}
                    options={(p.choices ?? []).map((c) => ({ value: String(c), label: fftLabel(c) }))}
                    onchange={(v) => onSpectrumParam(p, Number(v))}
                  />
                {:else}
                  <Slider
                    label={p.label}
                    value={spectrumValue(p)}
                    min={p.min}
                    max={p.max}
                    step={p.step}
                    disabled={!$spectrumConfig.enabled}
                    format={(v) => (p.key === "minHz" || p.key === "maxHz" ? `${v} Hz` : String(v))}
                    onchange={(v) => onSpectrumParam(p, v)}
                  />
                {/if}
              {/each}
            </div>
          </div>
        {/if}
      </div>

      <div class="flex shrink-0 items-center gap-3 border-t border-line px-4 py-2 text-[11px] text-muted">
        <span>Settings apply as you change them.</span>
        <span class="ml-auto"><kbd class="font-sans text-fg">Esc</kbd> to close</span>
      </div>
    </div>
  </div>
{/if}
