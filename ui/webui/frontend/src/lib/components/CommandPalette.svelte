<script lang="ts">
  import { onDestroy } from "svelte";
  import { MagnifyingGlass, MusicNote, Pause } from "phosphor-svelte";
  import { commands, previewConfig, queueCover, searchYTM, ytmCover } from "../store";
  import { matchQueue, hasTags, ytmKindLabel, ytmResultRow, ytmSubtitle } from "../search";
  import { formatTime, isRemoteRef, subtitle } from "../format";
  import type {
    PreviewConfig,
    PreviewState,
    QueueRow,
    YTMResult,
  } from "../../../bindings/github.com/dlcuy22/player/ui/webui/models";

  // A palette row is a queued track, or a YouTube Music search hit projected
  // onto the same shape. The extra field carries the hit's own data, which the
  // queue row shape does not have: its duration, its kind, and whether
  // selecting it can play anything.
  type PaletteRow = QueueRow & { ytm?: YTMResult };

  let {
    open,
    source,
    queue,
    preview,
    startPreview,
    onclose,
  }: {
    open: boolean;
    source: "queue" | "ytm";
    queue: QueueRow[];
    preview: PreviewState;
    startPreview: boolean;
    onclose: () => void;
  } = $props();

  const isYTM = $derived(source === "ytm");

  let query = $state("");
  let cursor = $state(0);
  let inputEl = $state<HTMLInputElement | null>(null);
  let listEl = $state<HTMLUListElement | null>(null);

  // The YouTube Music side: the last results, and the request state. They live
  // beside the queue's own state because the palette shows one source at a
  // time; the inactive one is never drawn.
  let ytmResults = $state<YTMResult[]>([]);
  let ytmLoading = $state(false);
  let ytmError = $state("");
  // ytmSeq tags each request so a reply for an abandoned query cannot overwrite
  // the results of the query that replaced it.
  let ytmSeq = 0;

  // previewMode is the Ctrl+Alt+P session toggle. It is UI-local: the Go side
  // only knows about individual previews, so turning the mode off simply stops
  // the current one. It is a queue-mode concept: a remote hit has no local file
  // to audition, so the controls are not offered there at all.
  let previewMode = $state(false);
  let settingsOpen = $state(false);

  // covers maps a row's identity to its artwork data URL, filled in only for
  // the rows the observer has seen on screen. An identity with no entry draws
  // the music note placeholder, so an absent value and a track with no art look
  // alike, which is honest: neither has a cover to show yet.
  let covers = $state<Record<string, string>>({});

  // pending holds the identities whose artwork fetch is already in flight, so
  // an intersection that fires twice for one row does not start two calls.
  const pending = new Set<string>();

  // ytmRows projects the current hits onto the queue row shape the list draws.
  const ytmRows = $derived(
    ytmResults.map((r, i) => ({ ...ytmResultRow(r, i), ytm: r }) as PaletteRow),
  );

  // matches is the one result set the list renders, from whichever source this
  // palette was opened with. The queue side is recomputed from the store on
  // every keystroke, which is cheap because the tags already ride on the rows.
  const matches = $derived<PaletteRow[]>(
    isYTM ? ytmRows : matchQueue(queue, query).map((r) => r as PaletteRow),
  );

  // selectable is the indices the cursor may land on. A queue row always can be
  // played; a YouTube Music hit only when it is a song. The other kinds are
  // shown as what they are, but the keyboard never lands on something Enter
  // cannot act on.
  const selectable = $derived(
    matches.reduce<number[]>((acc, row, i) => {
      if (isSelectable(row)) acc.push(i);

      return acc;
    }, []),
  );

  // A query that matches nothing, a queue with no tracks, and a search not yet
  // run are different states with different next actions; keep them apart for
  // the empty view.
  const noQueue = $derived(!isYTM && queue.length === 0);
  const empty = $derived(!isYTM && matches.length === 0);
  const ytmIdle = $derived(isYTM && query.trim() === "");
  const ytmEmpty = $derived(
    isYTM && !ytmLoading && ytmError === "" && query.trim() !== "" && matches.length === 0,
  );

  // ytmCounts reports whether a search also matched things that cannot be
  // played, so the footer can say so once instead of repeating it per row.
  const ytmOtherKinds = $derived(
    isYTM ? matches.filter((r) => r.ytm && !r.ytm.playable).length : 0,
  );

  /** isSelectable reports whether Enter can act on a row. */
  function isSelectable(row: PaletteRow): boolean {
    if (row.ytm) {
      return row.ytm.playable && row.path !== "";
    }

    return true;
  }

  /** canPreview reports whether the audition can sound a row. It is local-only:
   *  a YouTube Music track, whether a search hit or already queued, has no file
   *  the preview player can open. */
  function canPreview(row: PaletteRow | undefined): boolean {
    if (!row || row.ytm) {
      return false;
    }

    return row.path !== "" && !isRemoteRef(row.path);
  }

  // previewingPath is the row the Go preview is currently sounding, or "".
  const previewingPath = $derived(preview.active ? preview.path : "");

  // previewAvailable is whether the highlighted row could be auditioned at all.
  // It drives the button's disabled state, so a remote row says why rather than
  // accepting a click that cannot sound.
  const previewAvailable = $derived(canPreview(matches[cursor]));
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
      ytmResults = [];
      ytmLoading = false;
      ytmError = "";
      ytmSeq++;
      settingsOpen = false;
      previewMode = source === "queue" && startPreview;
      // Adopt the source this open started in, so the source-change effect
      // below does not read the open itself as a switch and disarm the preview
      // we just armed (Ctrl+Alt+P after the palette was last used for YouTube).
      lastSource = source;
      inputEl?.focus();
    } else if (!open && wasOpen) {
      // Closing ends the mode; the Go side ends the session, so reopening
      // starts from a clean, unarmed state.
      previewMode = false;
      settingsOpen = false;
    }
    wasOpen = open;
  });

  // A YouTube Music search runs off the keystroke, not on it: the debounce
  // keeps a typed word from spending one request per letter. The timer is not
  // cleared from the effect's teardown for the same reason the preview timer is
  // not: a snapshot push re-runs the effect and a teardown would cancel the
  // request that is about to be made.
  let searchTimer: ReturnType<typeof setTimeout> | null = null;

  function clearSearchTimer() {
    if (searchTimer !== null) {
      clearTimeout(searchTimer);
      searchTimer = null;
    }
  }

  $effect(() => {
    const q = query.trim();
    if (!open || !isYTM) {
      clearSearchTimer();

      return;
    }
    if (q === "") {
      clearSearchTimer();
      ytmResults = [];
      ytmLoading = false;
      ytmError = "";

      return;
    }

    ytmLoading = true;
    ytmError = "";
    const seq = ++ytmSeq;
    clearSearchTimer();
    searchTimer = setTimeout(() => {
      searchTimer = null;
      searchYTM(q).then(
        (rows) => {
          if (seq !== ytmSeq) {
            return;
          }
          ytmResults = rows;
          ytmLoading = false;
        },
        (err) => {
          if (seq !== ytmSeq) {
            return;
          }
          ytmResults = [];
          ytmError = errText(err);
          ytmLoading = false;
        },
      );
    }, 250);
  });

  // The timer outlives the effect by design, so it is cleared on unmount here.
  onDestroy(clearSearchTimer);

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

    // Preview is local-only. A remote row has no file the preview player can
    // open, so attempting one would fail the audition and leave the main track
    // paused for a session that never sounded.
    if (!open || !previewMode || !canPreview(row)) {
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

  // Preview belongs to the queue source: a remote hit has no local file to
  // audition, and the preview player has no provider, so a "ytm:" reference
  // would fail the audition while leaving the main track paused for a session
  // that never ends. Switching source while the palette is open therefore ends
  // any audition and disarms the toggle, so returning to the queue starts
  // unarmed rather than showing a stale "Previewing".
  let lastSource: "queue" | "ytm" | null = null;
  $effect(() => {
    const now = source;
    if (lastSource === null) {
      lastSource = now;

      return;
    }
    if (now === lastSource) {
      return;
    }
    lastSource = now;
    previewMode = false;
    commands.previewStop();
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
          if (path === "" || pending.has(path)) {
            continue;
          }
          pending.add(path);
          // A queued track's art comes from its file, a search hit's from the
          // catalogue; the backend serves both as a data URL, but only the
          // local one needs the content id it was asked for.
          const req = isYTM ? ytmCover(path) : queueCover(id, path);
          req.then((url) => {
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
  // it past the end, and Enter would then do nothing. The YouTube Music side
  // also skips the hits that cannot be played, so the highlight never rests on
  // a row Enter cannot act on.
  $effect(() => {
    if (!selectable.includes(cursor)) {
      cursor = selectable.length > 0 ? selectable[0] : 0;
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
    // The keyboard never rests on a row Enter cannot act on, and neither does
    // the pointer: a hovered non-playable hit would be snapped away by the
    // cursor effect, which reads as a flicker.
    if (!selectable.includes(i)) {
      return;
    }
    cursor = i;
  }

  function move(delta: number) {
    const list = selectable;
    if (list.length === 0) {
      return;
    }
    const at = list.indexOf(cursor);
    // The highlight can sit off the list for a moment while an effect moves it;
    // stepping from there lands on the first entry rather than nowhere.
    const next = at < 0 ? 0 : (at + delta + list.length) % list.length;
    cursor = list[next];
  }

  function choose(row: PaletteRow, append = false) {
    // Enter commits the selection, so it ends any audition before playing.
    if (previewMode) {
      previewMode = false;
      commands.previewStop();
    }
    if (row.ytm) {
      if (!isSelectable(row)) {
        return;
      }
      // Enter queues the track to play next; Shift+Enter adds it to the end
      // without touching what is playing. Neither replaces the queue, so
      // starting a search does not throw away what is already there.
      if (append) {
        commands.appendYTM(row.ytm.videoId);
      } else {
        commands.insertNextYTM(row.ytm.videoId);
      }
    } else {
      commands.playIndex(row.index);
    }
    onclose();
  }

  function togglePreviewMode() {
    previewMode = !previewMode;
    const row = matches[cursor];
    if (previewMode && canPreview(row)) {
      commands.previewStart(row!.path);
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
          // Shift+Enter appends to the queue; plain Enter queues to play next.
          choose(matches[cursor], e.shiftKey);
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

  function rowLabel(row: PaletteRow): string {
    return row.title.trim() || row.name;
  }

  /** rowLine is a row's second line: the queue's credits, or the YouTube Music
   *  credits with the kind spelled out when the hit is not a song. */
  function rowLine(row: PaletteRow): string {
    if (row.ytm) {
      return ytmSubtitle(row.ytm);
    }

    return subtitle(row.artist, row.album);
  }

  /** errText unwraps a rejected bridge call, which rejects with a plain string
   *  rather than an Error. */
  function errText(err: unknown): string {
    if (err instanceof Error) {
      return err.message;
    }
    if (typeof err === "string" && err !== "") {
      return err;
    }

    return "The search failed.";
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
      aria-label={isYTM ? "Search YouTube Music" : "Search the queue"}
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
          placeholder={isYTM ? "Search YouTube Music" : "Search title, artist or album"}
          autocomplete="off"
          spellcheck="false"
          role="combobox"
          aria-expanded="true"
          aria-controls="palette-results"
          aria-activedescendant={matches[cursor] ? `palette-row-${cursor}` : undefined}
          aria-label={isYTM ? "Search YouTube Music" : "Search title, artist or album"}
        />
        <!-- Preview is a secondary action, so it sits at the end of the field
             rather than competing with the search. It only exists in the queue
             source; the YouTube source has no local file to audition. Within
             the queue, a row that is itself remote cannot be auditioned either,
             so the control is disabled with the reason rather than accepting a
             click that cannot sound. -->
        {#if !isYTM}
          <button
            class="flex shrink-0 items-center gap-1.5 rounded-[4px] px-2 py-1 text-[11px] transition-colors disabled:cursor-not-allowed disabled:opacity-40"
            class:bg-accent={previewMode}
            class:text-bg={previewMode}
            class:text-muted={!previewMode}
            class:hover:bg-hover={!previewMode && previewAvailable}
            aria-pressed={previewMode}
            disabled={!previewMode && !previewAvailable}
            title={previewAvailable || previewMode
              ? "Preview the highlighted track (Ctrl+Alt+P)"
              : "Only local tracks can be previewed"}
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
        {/if}
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
      {:else if ytmIdle}
        <p class="px-3 py-8 text-center text-sm text-muted">
          Type to search YouTube Music.
        </p>
      {:else if ytmLoading && matches.length === 0}
        <!-- Only when there is nothing to show yet: a re-search keeps the
             previous results on screen instead of blanking the list on every
             keystroke. -->
        <p class="px-3 py-8 text-center text-sm text-muted" role="status">
          Searching YouTube Music…
        </p>
      {:else if ytmError !== ""}
        <p class="px-3 py-8 text-center text-sm text-muted" role="alert">
          {ytmError}
          <span class="mt-1 block">Check your connection and try again.</span>
        </p>
      {:else if ytmEmpty}
        <p class="px-3 py-8 text-center text-sm text-muted">
          No results for “{query}”. Try a different spelling.
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
          {#each matches as row, i (isYTM ? `ytm-${row.ytm?.videoId || i}` : `${row.path}-${row.index}`)}
            <li
              id={`palette-row-${i}`}
              data-index={i}
              role="option"
              aria-selected={i === cursor}
              aria-disabled={isSelectable(row) ? undefined : true}
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
                {#if rowLine(row) !== ""}
                  <span class="block truncate text-xs text-muted">{rowLine(row)}</span>
                {:else if !row.ytm && row.title.trim() !== "" && row.name !== row.title.trim()}
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
              <!-- A YouTube Music hit shows its length, which a queue row does
                   not have until its probe lands. -->
              {#if row.ytm && row.ytm.durationMs > 0}
                <span class="shrink-0 text-[11px] tabular-nums text-muted">
                  {formatTime(row.ytm.durationMs)}
                </span>
              {/if}
              <!-- An explicit track is marked with a letter, never colour
                   alone. -->
              {#if row.ytm?.explicit}
                <span
                  class="shrink-0 rounded-[3px] border border-line px-1 text-[10px] text-muted"
                  title="Explicit"
                >E</span>
              {/if}
              <!-- Row state is always a word, never colour alone. Preview
                   takes precedence over Playing: while an audition is
                   sounding, that is what the row is doing. -->
              {#if previewMode && !row.ytm && isRemoteRef(row.path)}
                <span class="shrink-0 text-[11px] text-muted">No preview</span>
              {:else if row.path === previewingPath}
                <span class="flex shrink-0 items-center gap-1 text-[11px] text-fg">
                  {preview.loop ? "Preview loop" : "Preview"}
                </span>
              {:else if row.active}
                <span class="shrink-0 text-[11px] text-fg">Playing</span>
              {:else if isYTM}
                {#if ytmKindLabel(row.ytm!) !== ""}
                  <span class="shrink-0 text-[11px] text-muted">{ytmKindLabel(row.ytm!)}</span>
                {/if}
              {:else if !hasTags(row)}
                <span class="shrink-0 text-[11px] text-muted">No tags</span>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}

      <div class="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-line px-3 py-2 text-[11px] text-muted">
        <span><kbd class="font-sans text-fg">↑</kbd> <kbd class="font-sans text-fg">↓</kbd> to move</span>
        {#if isYTM}
          <!-- Enter queues the track after the current one rather than
               replacing the queue, so a search never throws away what is
               playing. Shift+Enter is the plain "queue it for later". -->
          <span><kbd class="font-sans text-fg">Enter</kbd> to play next</span>
          <span><kbd class="font-sans text-fg">Shift Enter</kbd> to add to queue</span>
        {:else}
          <span><kbd class="font-sans text-fg">Enter</kbd> to play</span>
          <span><kbd class="font-sans text-fg">Ctrl Alt P</kbd> preview</span>
          {#if previewMode}
            <span><kbd class="font-sans text-fg">Ctrl Alt L</kbd> loop</span>
          {/if}
        {/if}
        {#if isYTM && ytmOtherKinds > 0}
          <!-- Say once why some rows cannot be played, rather than repeating a
               reason on every one of them. -->
          <span>{ytmOtherKinds} not playable</span>
        {/if}
        <span><kbd class="font-sans text-fg">Esc</kbd> to close</span>
      </div>
    </div>
  </div>
{/if}
