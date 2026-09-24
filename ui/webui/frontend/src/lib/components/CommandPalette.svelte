<script lang="ts">
  import { onDestroy } from "svelte";
  import { MagnifyingGlass, MusicNote, Pause } from "phosphor-svelte";
  import { commands, previewConfig, queueCover } from "../store";
  import { matchQueue, hasTags } from "../search";
  import { subtitle } from "../format";
  import type {
    PreviewConfig,
    PreviewState,
    QueueRow,
  } from "../../../bindings/github.com/dlcuy22/player/ui/webui/models";

  let {
    open,
    queue,
    preview,
    startPreview,
    onclose,
  }: {
    open: boolean;
    queue: QueueRow[];
    preview: PreviewState;
    startPreview: boolean;
    onclose: () => void;
  } = $props();

  let query = $state("");
  let cursor = $state(0);
  let inputEl = $state<HTMLInputElement | null>(null);
  let listEl = $state<HTMLUListElement | null>(null);

  // previewMode is the Ctrl+Alt+P session toggle. It is UI-local: the Go side
  // only knows about individual previews, so turning the mode off simply stops
  // the current one.
  let previewMode = $state(false);
  let settingsOpen = $state(false);

  // covers maps a row's path to its artwork data URL, filled in only for the
  // rows the observer has seen on screen. A path with no entry draws the music
  // note placeholder, so an absent value and a track with no art look alike,
  // which is honest: neither has a cover to show yet.
  let covers = $state<Record<string, string>>({});

  // pending holds the paths whose artwork fetch is already in flight, so an
  // intersection that fires twice for one row does not start two calls.
  const pending = new Set<string>();

  // matches is recomputed from the store's queue on every keystroke, which is
  // cheap because the tags already ride on the rows.
  const matches = $derived(matchQueue(queue, query));

  // A query that matches nothing, or a queue with no tracks, are different
  // states with different next actions; keep them apart for the empty view.
  const empty = $derived(matches.length === 0);
  const noQueue = $derived(queue.length === 0);

  // previewingPath is the row the Go preview is currently sounding, or "".
  const previewingPath = $derived(preview.active ? preview.path : "");
  const previewProgress = $derived(
    preview.duration > 0 ? Math.min(1, preview.position / preview.duration) : 0,
  );

  // Reset once per open, not on every dependency change: a snapshot push
  // rebuilds the queue array several times a second, and a reset keyed on the
  // props alone would still be fragile if a prop identity changed. Tracking the
  // previous open value makes the reset a true open edge.
  let wasOpen = false;
  $effect(() => {
    if (open && !wasOpen) {
      query = "";
      cursor = 0;
      covers = {};
      pending.clear();
      inputEl?.focus();
      if (startPreview) {
        previewMode = true;
      }
    } else if (!open && wasOpen) {
      // Closing ends the mode; the Go side ends the session, so reopening
      // starts from a clean, unarmed state.
      previewMode = false;
      settingsOpen = false;
    }
    wasOpen = open;
  });

  // Preview follows the highlight, but only while preview mode is on. A short
  // debounce keeps a held arrow key from starting a preview for every row it
  // passes through; only where the selection rests is auditioned.
  //
  // The dedupe key is the last path *requested*, not the Go-reported preview
  // path: with looping off, a window ends and the preview goes inactive, and
  // comparing against that state would restart the same preview forever.
  //
  // The timer is deliberately NOT cleared from the effect's teardown. Snapshot
  // pushes arrive 4x a second and rebuild the queue array, so this effect re-runs
  // on every push even when the highlight has not moved; a teardown that cleared
  // the timer would cancel a pending preview and never reschedule it, which made
  // the preview lag behind or stick on an earlier row.
  let previewTimer: ReturnType<typeof setTimeout> | null = null;
  let lastPreviewRequest = "";

  function clearPreviewTimer() {
    if (previewTimer !== null) {
      clearTimeout(previewTimer);
      previewTimer = null;
    }
  }

  $effect(() => {
    const row = matches[cursor];
    const path = row?.path ?? "";

    if (!open || !previewMode) {
      clearPreviewTimer();
      lastPreviewRequest = "";

      return;
    }
    if (path === "" || path === lastPreviewRequest) {
      // Same target: leave any pending timer alone so a snapshot push cannot
      // cancel the preview that is about to start.
      return;
    }
    lastPreviewRequest = path;

    clearPreviewTimer();
    previewTimer = setTimeout(() => {
      previewTimer = null;
      commands.previewStart(path);
    }, 180);
  });

  // The timer outlives the effect by design, so it is cleared on unmount here.
  onDestroy(clearPreviewTimer);

  // Leaving preview mode, or closing the palette, stops the audition so the
  // main track resumes. The Go side ends the session the same way.
  $effect(() => {
    if (!previewMode && preview.active) {
      commands.previewStop();
    }
  });

  // Art is fetched only for rows actually inside the list's viewport. An effect
  // (which runs after the DOM is updated) wires the observer, rather than a
  // per-row action: an action runs before this component's reset effect, so an
  // action-created observer would be torn down on open. The effect re-runs when
  // the result set changes, so new rows from a narrower query are watched too.
  // This is what keeps a queue of thousands from decoding art nobody scrolled
  // to.
  $effect(() => {
    // Track the exact result set, not just its length: a different query can
    // yield the same count with different rows.
    const key = matches.map((m) => m.path).join("|");
    if (!open || !listEl || key === "") {
      return;
    }

    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) {
            continue;
          }
          const el = entry.target as HTMLElement;
          const id = el.dataset.coverId ?? "";
          const path = el.dataset.coverPath ?? "";
          if (id === "" || pending.has(path)) {
            continue;
          }
          pending.add(path);
          queueCover(id, path).then((url) => {
            if (url !== "") {
              covers[path] = url;
            }
          });
        }
      },
      { root: listEl, rootMargin: "0px" },
    );
    for (const el of listEl.querySelectorAll<HTMLElement>("[data-cover-path]")) {
      observer.observe(el);
    }

    return () => observer.disconnect();
  });

  // Keep the cursor inside the current result set: a narrowing query can leave
  // it past the end, and Enter would then do nothing.
  $effect(() => {
    if (cursor >= matches.length) {
      cursor = Math.max(0, matches.length - 1);
    }
  });

  // Follow the cursor. The list scrolls independently of the input, so a
  // keyboard move past the visible rows has to bring the selected row into
  // view. The key is the selected row's identity, not the result array: a
  // snapshot push rebuilds `matches` several times a second, and scrolling on
  // every push would fight a manual scroll (and yank the list while the user
  // is reading it).
  const followKey = $derived(`${cursor}|${matches[cursor]?.path ?? ""}`);
  let followedKey = "";
  $effect(() => {
    const key = followKey;
    if (!listEl || key === followedKey) {
      return;
    }
    followedKey = key;
    listEl
      .querySelector<HTMLElement>(`[data-index="${cursor}"]`)
      ?.scrollIntoView({ block: "nearest" });
  });

  // hoverCursor moves the highlight to a row the pointer is over. It is driven
  // by pointer movement, not by a per-row mouseenter: a wheel scroll slides
  // rows under a stationary pointer, and mouseenter fires for whatever row
  // lands there, which dragged the highlight down when the user scrolled up.
  // Tracking the pointer's own motion is what keeps a scroll from stealing the
  // selection.
  let pointerX = -1;
  let pointerY = -1;
  function hoverCursor(e: MouseEvent, i: number) {
    if (e.clientX === pointerX && e.clientY === pointerY) {
      // The pointer did not move; a row scrolled under it. Leave the cursor.
      return;
    }
    pointerX = e.clientX;
    pointerY = e.clientY;
    cursor = i;
  }

  function move(delta: number) {
    if (matches.length === 0) {
      return;
    }
    cursor = (cursor + delta + matches.length) % matches.length;
  }

  function choose(row: QueueRow) {
    // Enter commits the selection, so it ends any audition before playing.
    if (previewMode) {
      previewMode = false;
      commands.previewStop();
    }
    commands.playIndex(row.index);
    onclose();
  }

  function togglePreviewMode() {
    previewMode = !previewMode;
    if (previewMode && matches[cursor]) {
      commands.previewStart(matches[cursor].path);
    }
  }

  function onKeydown(e: KeyboardEvent) {
    // Ctrl+Alt+P toggles the preview session; Ctrl+Alt+L toggles looping. They
    // are checked before the plain keys so a modifier chord is never read as a
    // bare arrow or letter.
    if ((e.ctrlKey || e.metaKey) && e.altKey && e.key.toLowerCase() === "p") {
      e.preventDefault();
      togglePreviewMode();

      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.altKey && e.key.toLowerCase() === "l") {
      e.preventDefault();
      commands.setPreviewConfig({ ...$previewConfig, loop: !$previewConfig.loop });

      return;
    }

    // Escape closes from anywhere in the dialog, including a focused button or
    // slider, so the modal can always be dismissed by keyboard.
    if (e.key === "Escape") {
      e.preventDefault();
      onclose();

      return;
    }

    // A range slider owns its own arrow keys; stealing them would make the
    // settings sliders unusable by keyboard. The list is driven by arrows only
    // when focus is not in a control that consumes them.
    const target = e.target as HTMLElement | null;
    const onSlider = target?.tagName === "INPUT" && (target as HTMLInputElement).type === "range";
    if (onSlider && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
      return;
    }

    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        move(1);
        break;
      case "ArrowUp":
        e.preventDefault();
        move(-1);
        break;
      case "Enter":
        e.preventDefault();
        if (matches[cursor]) {
          choose(matches[cursor]);
        }
        break;
      case "Tab":
        // The dialog is modal; keep Tab from walking into the window behind it.
        e.preventDefault();
        break;
    }
  }

  // refocus returns focus to the search field after a control is used, so the
  // list keys keep working without the user reaching for the mouse again.
  function refocus() {
    inputEl?.focus();
  }

  function setPreviewMs(key: keyof PreviewConfig, seconds: number) {
    const ms = Math.max(0, Math.round(seconds * 1000));
    commands.setPreviewConfig({ ...$previewConfig, [key]: ms });
  }

  function rowLabel(row: QueueRow): string {
    return row.title.trim() || row.name;
  }
</script>

<!--
  Quick open: Ctrl/Cmd+P over the queue. It is a modal because it takes over
  the keyboard while it is open; the player keeps running behind it. The panel
  is a plain surface with a hairline, matching the rest of the window.
-->
{#if open}
  <div
    class="fixed inset-0 z-50 flex justify-center bg-black/50 p-4"
    role="presentation"
    onclick={(e) => {
      if (e.target === e.currentTarget) onclose();
    }}
  >
    <div
      class="mt-[8vh] flex max-h-[70vh] w-full max-w-xl flex-col overflow-hidden rounded-[6px] border border-line bg-surface"
      role="dialog"
      aria-modal="true"
      aria-label="Search the queue"
      tabindex="-1"
      onkeydown={onKeydown}
    >
      <div class="flex items-center gap-2 border-b border-line px-3 focus-within:ring-2 focus-within:ring-inset focus-within:ring-focus">
        <MagnifyingGlass size="16" class="shrink-0 text-muted" aria-hidden="true" />
        <input
          bind:this={inputEl}
          bind:value={query}
          class="w-full bg-transparent py-3 text-sm text-fg outline-none! placeholder:text-muted"
          type="text"
          placeholder="Search title, artist or album"
          autocomplete="off"
          spellcheck="false"
          role="combobox"
          aria-expanded="true"
          aria-controls="palette-results"
          aria-activedescendant={matches[cursor] ? `palette-row-${matches[cursor].index}` : undefined}
          aria-label="Search title, artist or album"
        />
        <!-- Preview is a secondary action, so it sits at the end of the field
             rather than competing with the search. -->
        <button
          class="flex shrink-0 items-center gap-1.5 rounded-[4px] px-2 py-1 text-[11px] transition-colors"
          class:bg-accent={previewMode}
          class:text-bg={previewMode}
          class:text-muted={!previewMode}
          class:hover:bg-hover={!previewMode}
          aria-pressed={previewMode}
          title="Preview the highlighted track (Ctrl+Alt+P)"
          onclick={() => {
            togglePreviewMode();
            refocus();
          }}
        >
          {#if previewMode}
            <Pause size="12" weight="fill" aria-hidden="true" />
            Previewing
          {:else}
            <MusicNote size="12" aria-hidden="true" />
            Preview
          {/if}
        </button>
        <button
          class="shrink-0 rounded-[4px] px-1.5 py-1 text-[11px] text-muted transition-colors hover:bg-hover hover:text-fg"
          class:bg-selected={settingsOpen}
          aria-expanded={settingsOpen}
          title="Preview settings"
          onclick={() => {
            settingsOpen = !settingsOpen;
            refocus();
          }}
        >
          Settings
        </button>
      </div>

      {#if previewMode}
        <!-- The mode is a session, so its state is stated once here rather than
             on every row: what is auditioning, and whether it repeats. -->
        <div class="flex items-center gap-3 border-b border-line bg-selected/40 px-3 py-1.5 text-[11px] text-muted">
          <span class="flex items-center gap-1.5">
            {previewingPath ? "Previewing the highlighted track" : "Move to preview a track"}
          </span>
          <!-- Say what happened to the real track while an audition sounds, so
               a paused player is explained rather than looking broken. -->
          {#if preview.active}
            <span class="truncate">Main track paused</span>
          {/if}
          <button
            class="ml-auto rounded-[4px] px-1.5 py-0.5 transition-colors hover:bg-hover hover:text-fg"
            class:text-fg={$previewConfig.loop}
            aria-pressed={$previewConfig.loop}
            title="Loop the preview (Ctrl+Alt+L)"
            onclick={() =>
              commands.setPreviewConfig({ ...$previewConfig, loop: !$previewConfig.loop })}
          >
            {$previewConfig.loop ? "Loop on" : "Loop off"}
          </button>
        </div>
      {/if}

      {#if settingsOpen}
        <!-- Settings are a small panel, not a modal: they are edited while the
             queue stays visible, because the effect of a change is audible. -->
        <div class="grid grid-cols-2 gap-x-4 gap-y-3 border-b border-line bg-bg/40 px-3 py-3 text-[11px] text-muted">
          <label class="flex flex-col gap-1">
            <span class="flex justify-between">
              <span>Preview length</span>
              <span class="tabular-nums text-fg">{$previewConfig.lengthMs / 1000}s</span>
            </span>
            <input
              class="range"
              type="range"
              min="5"
              max="120"
              step="5"
              value={$previewConfig.lengthMs / 1000}
              oninput={(e) => setPreviewMs("lengthMs", Number(e.currentTarget.value))}
            />
          </label>
          <label class="flex flex-col gap-1">
            <span class="flex justify-between">
              <span>Start at</span>
              <span class="tabular-nums text-fg">{$previewConfig.startMs / 1000}s</span>
            </span>
            <input
              class="range"
              type="range"
              min="0"
              max="300"
              step="5"
              value={$previewConfig.startMs / 1000}
              oninput={(e) => setPreviewMs("startMs", Number(e.currentTarget.value))}
            />
          </label>
          <label class="flex flex-col gap-1">
            <span class="flex justify-between">
              <span>Fade</span>
              <span class="tabular-nums text-fg">{$previewConfig.fadeMs}ms</span>
            </span>
            <input
              class="range"
              type="range"
              min="0"
              max="2000"
              step="50"
              value={$previewConfig.fadeMs}
              oninput={(e) =>
                commands.setPreviewConfig({
                  ...$previewConfig,
                  fadeMs: Number(e.currentTarget.value),
                })}
            />
          </label>
          <label class="flex flex-col gap-1">
            <span class="flex justify-between">
              <span>Preview volume</span>
              <span class="tabular-nums text-fg">{Math.round($previewConfig.volume * 100)}%</span>
            </span>
            <input
              class="range"
              type="range"
              min="0"
              max="100"
              step="5"
              value={$previewConfig.volume * 100}
              oninput={(e) =>
                commands.setPreviewConfig({
                  ...$previewConfig,
                  volume: Number(e.currentTarget.value) / 100,
                })}
            />
          </label>
          <p class="col-span-2 text-[11px] text-muted">
            Previews play on a second output. The current track pauses for the session and
            resumes when you leave preview mode.
          </p>
        </div>
      {/if}

      {#if noQueue}
        <p class="px-3 py-8 text-center text-sm text-muted">
          No tracks queued. Add files or a folder to build a queue.
        </p>
      {:else if empty}
        <p class="px-3 py-8 text-center text-sm text-muted">
          No track matches “{query}”.
        </p>
      {:else}
        <ul
          bind:this={listEl}
          id="palette-results"
          class="scroll-thin min-h-0 flex-1 overflow-y-auto py-1"
          role="listbox"
        >
          {#each matches as row, i (row.path + row.index)}
            <li
              id={`palette-row-${row.index}`}
              data-index={i}
              role="option"
              aria-selected={i === cursor}
              class="flex cursor-pointer items-center gap-2 px-3 py-2"
              class:bg-selected={i === cursor}
              data-cover-id={row.coverId}
              data-cover-path={row.path}
              onmousedown={(e: MouseEvent) => {
                // mousedown, not click: the input keeps focus so the list does
                // not blur before the choice lands.
                e.preventDefault();
                choose(row);
              }}
              onmousemove={(e: MouseEvent) => hoverCursor(e, i)}
            >
              <span
                class="h-5 w-[3px] shrink-0 rounded-full"
                class:bg-accent={row.active}
                aria-hidden="true"
              ></span>
              <!-- Art when the track has it, the note otherwise. The slot is
                   one fixed size so rows do not jump when a cover arrives. -->
              {#if covers[row.path]}
                <img
                  src={covers[row.path]}
                  alt=""
                  class="size-8 shrink-0 rounded-[4px] border border-line object-cover"
                />
              {:else}
                <span class="grid size-8 shrink-0 place-items-center" aria-hidden="true">
                  <MusicNote size="15" weight={row.active ? "fill" : "regular"} class="text-muted" />
                </span>
              {/if}
              <span class="min-w-0 flex-1">
                <span class="block truncate text-sm text-fg">{rowLabel(row)}</span>
                {#if subtitle(row.artist, row.album) !== ""}
                  <span class="block truncate text-xs text-muted">
                    {subtitle(row.artist, row.album)}
                  </span>
                {:else if row.title.trim() !== "" && row.name !== row.title.trim()}
                  <span class="block truncate text-xs text-muted">{row.name}</span>
                {/if}
                <!-- The previewing row carries its own progress line, drawn
                     from the snapshot the Go side pushes, so it reads as an
                     audition distinct from the real "Playing" row. -->
                {#if row.path === previewingPath}
                  <span class="mt-1 block h-[2px] w-full overflow-hidden rounded-full bg-track">
                    <span
                      class="block h-full rounded-full bg-accent transition-[width] duration-100 ease-linear"
                      style="width: {previewProgress * 100}%"
                    ></span>
                  </span>
                {/if}
              </span>
              <!-- Row state is always a word, never colour alone. Preview
                   takes precedence over Playing: while an audition is
                   sounding, that is what the row is doing. -->
              {#if row.path === previewingPath}
                <span class="flex shrink-0 items-center gap-1 text-[11px] text-fg">
                  {preview.loop ? "Preview loop" : "Preview"}
                </span>
              {:else if row.active}
                <span class="shrink-0 text-[11px] text-fg">Playing</span>
              {:else if !hasTags(row)}
                <span class="shrink-0 text-[11px] text-muted">No tags</span>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}

      <div class="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-line px-3 py-2 text-[11px] text-muted">
        <span><kbd class="font-sans text-fg">↑</kbd> <kbd class="font-sans text-fg">↓</kbd> to move</span>
        <span><kbd class="font-sans text-fg">Enter</kbd> to play</span>
        <span><kbd class="font-sans text-fg">Ctrl Alt P</kbd> preview</span>
        {#if previewMode}
          <span><kbd class="font-sans text-fg">Ctrl Alt L</kbd> loop</span>
        {/if}
        <span><kbd class="font-sans text-fg">Esc</kbd> to close</span>
      </div>
    </div>
  </div>
{/if}
