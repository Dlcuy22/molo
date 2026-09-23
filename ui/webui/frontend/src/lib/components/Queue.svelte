<script lang="ts">
  import { MusicNote } from "phosphor-svelte";
  import { commands } from "../store";
  import { baseName } from "../format";
  import type { QueueRow } from "../../../bindings/github.com/dlcuy22/player/ui/webui/models";

  let { queue, queueIdx }: { queue: QueueRow[]; queueIdx: number } = $props();

  // The active row is scrolled into view when the track changes, so a queue
  // longer than the panel follows along without the user hunting for it.
  // Snapshots arrive several times a second with a fresh queue array each
  // time, so the scroll only happens when the followed track actually
  // changes; otherwise it would yank the list back while the user browses.
  let listEl: HTMLDivElement;
  const followKey = $derived(`${queueIdx}|${queue.length}|${queue[queueIdx]?.path ?? ""}`);
  let followedKey = "";
  $effect(() => {
    const key = followKey;
    if (!listEl || queueIdx < 0 || key === followedKey) {
      return;
    }
    followedKey = key;
    listEl
      .querySelector<HTMLElement>(`[data-row="${queueIdx}"]`)
      ?.scrollIntoView({ block: "nearest" });
  });
</script>

<!--
  Queue: one row per track. The active track is marked with an accent bar and
  accent text, the only place the accent appears in the list, so "where am I"
  is answerable at a glance without a second colour.
-->
<div bind:this={listEl} class="scroll-thin h-full overflow-y-auto">
  {#if queue.length === 0}
    <p class="px-3 py-6 text-sm text-muted">
      No tracks queued yet. Add files or a folder to build a queue.
    </p>
  {:else}
    <ul class="flex flex-col gap-px py-1">
      {#each queue as row (row.path + row.index)}
        <li>
          <button
            data-row={row.index}
            class="group flex w-full items-center gap-2 rounded-[4px] py-2 pr-3 text-left transition-colors"
            class:bg-selected={row.active}
            class:hover:bg-hover={!row.active}
            aria-current={row.active ? "true" : undefined}
            title={row.path}
            onclick={() => commands.playIndex(row.index)}
          >
            <!-- The accent bar is the active marker; the reserved width keeps
                 every row's text aligned whether or not the bar is present. -->
            <span
              class="h-5 w-[3px] shrink-0 rounded-full"
              class:bg-accent={row.active}
              aria-hidden="true"
            ></span>
            <MusicNote
              size="15"
              weight={row.active ? "fill" : "regular"}
              class={row.active ? "text-accent" : "text-muted"}
            />
            <span
              class="min-w-0 flex-1 truncate text-sm"
              class:text-accent={row.active}
              class:text-fg={!row.active}
            >
              {row.name || baseName(row.path)}
            </span>
          </button>
        </li>
      {/each}
    </ul>
  {/if}
</div>
