<script lang="ts">
  import { DotsSixVertical, Trash } from "phosphor-svelte";
  import type { EffectStage } from "../../effect-types";
  import { finalIndex, indexOfStage } from "./effects-helpers";

  // The chain, in processing order. It is two things at once: a list of stages
  // to select, and the signal path the audio actually takes, so the order shown
  // is the order heard.
  let {
    stages,
    selectedId,
    onselect,
    onmove,
    onremove,
  }: {
    stages: EffectStage[];
    selectedId: string;
    onselect: (id: string) => void;
    onmove: (id: string, to: number) => void;
    onremove: (id: string) => void;
  } = $props();

  // dragID is the id being dragged; dragOver is the index a drop would land
  // on. The indicator only draws while a drag is in flight, so a settled list
  // has no insertion marker.
  let dragID = $state<string | null>(null);
  let dragOver = $state<number | null>(null);

  function labelOf(s: EffectStage): string {
    return s.label.trim() || s.impl.trim() || s.kind;
  }

  function onDragStart(e: DragEvent, s: EffectStage) {
    dragID = s.id;
    e.dataTransfer?.setData("text/plain", s.id);
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = "move";
    }
  }

  function onDragOver(e: DragEvent, i: number) {
    if (dragID === null) {
      return;
    }
    e.preventDefault();
    if (e.dataTransfer) {
      e.dataTransfer.dropEffect = "move";
    }
    // The bottom half of a row means "insert after it", so a drag to the end
    // of the list is reachable without a dedicated drop zone.
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    dragOver = e.clientY > rect.top + rect.height / 2 ? i + 1 : i;
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    if (dragID !== null && dragOver !== null) {
      // dragOver is a gap in the current list; the engine wants a final index.
      const from = indexOfStage(stages, dragID);
      if (from >= 0) {
        onmove(dragID, finalIndex(from, dragOver, stages.length));
      }
    }
    dragID = null;
    dragOver = null;
  }

  function onDragEnd() {
    dragID = null;
    dragOver = null;
  }

  // The keyboard path, so reorder and remove are not mouse-only. Alt plus a
  // vertical arrow swaps the stage with its neighbour, and Delete or Backspace
  // removes it. A move past either end is a no-op, not an error the caller has
  // to reject. The accessible name on each row states the keys.
  function onRowKey(e: KeyboardEvent, s: EffectStage, i: number) {
    if (e.altKey && e.key === "ArrowUp") {
      e.preventDefault();
      if (i > 0) {
        onmove(s.id, i - 1);
      }
    } else if (e.altKey && e.key === "ArrowDown") {
      e.preventDefault();
      if (i < stages.length - 1) {
        onmove(s.id, i + 1);
      }
    } else if (e.key === "Delete" || e.key === "Backspace") {
      e.preventDefault();
      onremove(s.id);
    }
  }
</script>

<div class="flex h-full flex-col">
  {#if stages.length === 0}
    <p class="px-3 py-6 text-sm text-muted">
      The chain is empty. Add an effect from the catalogue to start processing.
    </p>
  {:else}
    <ul class="flex flex-col gap-px py-1" ondrop={onDrop} ondragover={(e) => e.preventDefault()}>
      {#each stages as s, i (s.id)}
        {#if dragOver === i && dragID !== null}
          <li class="h-[3px] rounded-full bg-accent" aria-hidden="true"></li>
        {/if}
        <li
          draggable="true"
          class="group relative"
          ondragstart={(e) => onDragStart(e, s)}
          ondragover={(e) => onDragOver(e, i)}
          ondrop={onDrop}
          ondragend={onDragEnd}
        >
          <div
            class="flex w-full items-center gap-2 rounded-[4px] py-2 pl-2 pr-10 text-left transition-colors"
            class:bg-selected={s.id === selectedId}
            class:hover:bg-hover={s.id !== selectedId}
          >
            <span
              class="grid size-6 shrink-0 cursor-grab place-items-center rounded-[4px] text-muted"
              title="Drag to reorder"
              aria-hidden="true"
            >
              <DotsSixVertical size="14" />
            </span>
            <button
              type="button"
              class="min-w-0 flex-1 truncate text-left text-sm"
              class:text-fg={s.id === selectedId}
              class:text-muted={s.id !== selectedId}
              class:line-through={s.bypassed}
              aria-current={s.id === selectedId ? "true" : undefined}
              aria-label="{labelOf(s)}, position {i + 1} of {stages.length}. Select, or use Alt+ArrowUp and Alt+ArrowDown to reorder, Delete to remove."
              onclick={() => onselect(s.id)}
              onkeydown={(e) => onRowKey(e, s, i)}
            >
              {labelOf(s)}
            </button>
            {#if s.bypassed}
              <span class="shrink-0 text-[11px] text-muted">Disabled</span>
            {/if}
            <button
              type="button"
              class="absolute right-0.5 top-1/2 grid size-8 -translate-y-1/2 place-items-center rounded-[4px] text-muted opacity-0 transition-opacity hover:bg-hover hover:text-fg focus-visible:opacity-100 group-hover:opacity-100"
              aria-label="Remove {labelOf(s)}"
              title="Remove {labelOf(s)}"
              onclick={() => onremove(s.id)}
            >
              <Trash size="14" />
            </button>
          </div>
        </li>
      {/each}
      {#if dragOver === stages.length && dragID !== null}
        <li class="h-[3px] rounded-full bg-accent" aria-hidden="true"></li>
      {/if}
    </ul>
    <p class="px-3 py-2 text-[11px] text-muted">
      Top of the list is first in the signal path.
    </p>
  {/if}
</div>
