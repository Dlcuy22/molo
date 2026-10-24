<script lang="ts">
  import {
    Pause,
    Play,
    Shuffle,
    SkipBack,
    SkipForward,
    Stop,
    SpeakerHigh,
    SpeakerSlash,
  } from "phosphor-svelte";
  import { commands } from "../store";
  import { muted, toggleMute, volumeBy, volumeOverride } from "../volume";
  import { formatTime, isActive, percent, fillFrac } from "../format";
  import type { Snapshot } from "../../../bindings/github.com/dlcuy22/molo/ui/webui/models";

  let { snap }: { snap: Snapshot } = $props();

  // The seek bar is optimistic: dragging updates the local value immediately so
  // the thumb tracks the pointer, and the engine confirmation overwrites it on
  // the next snapshot. While dragging, the pushed position is ignored, or the
  // thumb would fight the pointer.
  let scrubbing = $state(false);
  let scrubValue = $state(0);

  // The queue is nullable in the binding; the store never publishes null, but
  // narrowing once keeps the enabled checks plain.
  const queue = $derived(snap.queue ?? []);
  const live = $derived(isActive(snap.state));
  const canPrev = $derived(queue.length > 0);
  const position = $derived(scrubbing ? scrubValue : snap.position);
  const duration = $derived(snap.duration > 0 ? snap.duration : 0);
  const seekFill = $derived(fillFrac(position, 0, duration));

  // Volume comes from the shared store, so the slider and the keyboard cannot
  // disagree. The override leads the engine and is dropped when the server value
  // catches up (see volume.ts).
  const volumeLevel = $derived($volumeOverride ?? snap.volume);

  function onSeekInput(e: Event) {
    scrubbing = true;
    scrubValue = Number((e.currentTarget as HTMLInputElement).value);
  }

  function onSeekCommit(e: Event) {
    const value = Number((e.currentTarget as HTMLInputElement).value);
    scrubbing = false;
    commands.seek(value);
  }

  function onVolume(e: Event) {
    const v = Number((e.currentTarget as HTMLInputElement).value) / 100;
    // Route the slider drag through the same step helper the keys use, so the
    // two paths share their unmute and clamp behaviour.
    volumeBy(v - volumeLevel);
  }
</script>

<!--
  Transport: the one focal row of the app. The play button is the single round
  control and the only filled accent surface, so the eye lands on it first
  (DESIGN.md: "round play button only").
-->
<div class="flex flex-col gap-3">
  <div class="flex items-center gap-3">
    <span class="w-12 shrink-0 text-right text-xs tabular-nums text-muted">
      {formatTime(position)}
    </span>

    <input
      class="range flex-1 disabled:opacity-40"
      type="range"
      min="0"
      max={duration || 1}
      step="1000"
      value={position}
      style="--fill-frac: {seekFill}"
      disabled={duration === 0}
      aria-label="Seek"
      aria-valuetext="{formatTime(position)} of {formatTime(duration)}"
      oninput={onSeekInput}
      onchange={onSeekCommit}
    />

    <span class="w-12 shrink-0 text-xs tabular-nums text-muted">
      {formatTime(duration)}
    </span>
  </div>

  <div class="flex items-center justify-between gap-4">
    <div class="flex items-center gap-1">
      <button
        class="grid size-9 place-items-center rounded-[4px] text-fg transition-colors hover:bg-hover disabled:text-disabled disabled:hover:bg-transparent"
        disabled={!canPrev}
        aria-label="Previous track"
        title="Previous track"
        onclick={() => commands.prev()}
      >
        <SkipBack size="20" weight="fill" />
      </button>

      <button
        class="mx-1 grid place-items-center rounded-full bg-accent text-bg transition-colors hover:brightness-110 disabled:bg-disabled"
        style="width: 2.75rem; height: 2.75rem"
        disabled={queue.length === 0}
        aria-label={snap.state === "playing" ? "Pause" : "Play"}
        title={snap.state === "playing" ? "Pause" : "Play"}
        onclick={() => commands.togglePause()}
      >
        {#if snap.state === "playing"}
          <Pause size="22" weight="fill" />
        {:else}
          <Play size="22" weight="fill" />
        {/if}
      </button>

      <button
        class="grid size-9 place-items-center rounded-[4px] text-fg transition-colors hover:bg-hover disabled:text-disabled disabled:hover:bg-transparent"
        disabled={queue.length === 0}
        aria-label="Next track"
        title="Next track"
        onclick={() => commands.next()}
      >
        <SkipForward size="20" weight="fill" />
      </button>

      <button
        class="grid size-9 place-items-center rounded-[4px] text-muted transition-colors hover:bg-hover hover:text-fg disabled:text-disabled disabled:hover:bg-transparent"
        disabled={!live}
        aria-label="Stop"
        title="Stop"
        onclick={() => commands.stop()}
      >
        <Stop size="18" weight="fill" />
      </button>

      <!--
        Shuffle bakes a new queue order once rather than picking a random track
        on each advance, so the icon stays lit while that order is in force and
        the queue list shows exactly what will play next.
      -->
      <button
        class="grid size-9 place-items-center rounded-[4px] transition-colors hover:bg-hover disabled:text-disabled disabled:hover:bg-transparent"
        class:text-accent={snap.shuffled}
        class:text-muted={!snap.shuffled}
        disabled={!canPrev}
        aria-label={snap.shuffled ? "Shuffle on" : "Shuffle off"}
        aria-pressed={snap.shuffled}
        title={snap.shuffled ? "Shuffle on" : "Shuffle off"}
        onclick={() => commands.shuffle(!snap.shuffled)}
      >
        <Shuffle size="18" weight={snap.shuffled ? "fill" : "regular"} />
      </button>
    </div>

    <div class="flex items-center gap-2">
      <button
        class="grid size-8 place-items-center rounded-[4px] text-muted transition-colors hover:bg-hover hover:text-fg"
        aria-label={$muted ? "Unmute" : "Mute"}
        title={$muted ? "Unmute" : "Mute"}
        onclick={toggleMute}
      >
        {#if volumeLevel === 0 || $muted}
          <SpeakerSlash size="19" />
        {:else}
          <SpeakerHigh size="19" />
        {/if}
      </button>
      <input
        class="range w-28"
        type="range"
        min="0"
        max="100"
        step="1"
        value={percent(volumeLevel)}
        style="--fill-frac: {fillFrac(volumeLevel, 0, 1)}"
        aria-label="Volume"
        oninput={onVolume}
      />
      <span class="w-9 text-xs tabular-nums text-muted">{percent(volumeLevel)}%</span>
    </div>
  </div>
</div>
