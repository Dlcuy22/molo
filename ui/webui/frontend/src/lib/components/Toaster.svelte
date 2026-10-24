<script lang="ts">
  import { fade } from "svelte/transition";
  import { MusicNotes } from "phosphor-svelte";
  import { toast } from "../toast";
  import { cover, player } from "../store";
  import { displayTitle } from "../format";

  // Toaster: the fixed host for the one status card. It is centred so the
  // feedback is where the eye already is, above the transport but not over it,
  // and pointer-events-none so it can never intercept a click meant for the
  // surface behind it. z-40 sits below the modals' z-50: a toast is a note, not
  // a thing that covers a dialog.
  const t = $derived($toast);
  // The art card names the track from the live snapshot rather than from the
  // toast payload, so when the next track's snapshot lands the title and the
  // cover change in place while the card is still up.
  const nowPlaying = $derived(displayTitle($player.title, $player.path));

  // announcement is the text the live region reads out. The region is always in
  // the DOM (see the markup): a screen reader announces a change inside an
  // existing role="status" node, but many do not announce a node that enters
  // the DOM already populated, so the node must exist first and the text must
  // arrive second.
  const announcement = $derived.by(() => {
    const cur = t;
    if (cur === null) {
      return "";
    }
    if (cur.kind === "bar") {
      return `${cur.label} ${cur.text ?? ""}`.trim();
    }
    if (cur.kind === "art") {
      return nowPlaying || cur.label;
    }

    return cur.label;
  });
</script>

<!--
  The live region is rendered unconditionally so it is a stable node; the card
  inside it comes and goes. It stays out of the layout (fixed, zero size) and
  carries no visible text of its own: the announcement goes through the
  screen-reader-only span, so the visual card never doubles up in the a11y tree.
-->
<div
  class="pointer-events-none fixed left-1/2 top-1/2 z-40 -translate-x-1/2 -translate-y-1/2"
  role="status"
  aria-live="polite"
>
  <span class="sr-only">{announcement}</span>

  {#if t}
    <!--
      Motion (DESIGN.md MOTION 1): a one-shot 120ms fade, because the card is a
      response to a user action, not ambient motion, so it earns a transition a
      static page would not (R-19). It is opacity only: no transform, no scale
      overshoot and no pulse, which is also all a reduced-motion user needs, since
      there is no movement to remove (R-31).
    -->
    <div
      class="border border-line bg-surface"
      aria-hidden="true"
      transition:fade={{ duration: 120 }}
    >
      {#if t.kind === "bar"}
        <div class="flex w-64 flex-col gap-2 px-4 py-3">
          <div class="flex items-baseline justify-between gap-3">
            <span class="text-xs text-muted">{t.label}</span>
            <span class="text-xs tabular-nums text-fg">{t.text}</span>
          </div>
          <!-- No CSS transition on the fill: the value steps per keypress and a
               transition would leave the bar chasing the number it sits beside. -->
          <div class="h-1.5 w-full overflow-hidden rounded-full bg-track">
            <div
              class="h-full bg-accent"
              style="width: {Math.round((t.value ?? 0) * 100)}%"
            ></div>
          </div>
        </div>
      {:else if t.kind === "tile"}
        <div class="flex flex-col items-center gap-2 px-6 py-4">
          {#if t.icon}
            {@const Icon = t.icon}
            <Icon size="40" weight="fill" class="text-fg" aria-hidden="true" />
          {/if}
          <span class="text-xs text-muted">{t.label}</span>
        </div>
      {:else}
        <div class="flex flex-col items-center gap-2 px-4 py-3">
          <div class="grid size-24 place-items-center overflow-hidden bg-hover">
            {#if $cover}
              <img src={$cover} alt="" class="size-full object-cover" />
            {:else}
              <MusicNotes size="40" class="text-muted" aria-hidden="true" />
            {/if}
          </div>
          <span class="max-w-48 truncate text-xs text-fg">{nowPlaying || t.label}</span>
        </div>
      {/if}
    </div>
  {/if}
</div>
