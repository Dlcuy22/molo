<script lang="ts">
  import { Browser } from "@wailsio/runtime";
  import { DiscordLogo, MusicNotes } from "phosphor-svelte";
  import { commands, cover, discordStatus, player, artistAvatar } from "../store";
  import { displayTitle, formatTime, isRemoteRef, ytmVideoId } from "../format";

  // The preview mirrors the card Discord draws from the activity: the verb and
  // the artist on the header line, the cover with the artist avatar as a small
  // badge, the title and artist, the progress bar, and the two links. It reads
  // the same snapshot the rest of the window does, so what it shows is what the
  // presence will send.
  const snap = $derived($player);

  const title = $derived(displayTitle(snap.title, snap.path));
  const isRemote = $derived(isRemoteRef(snap.path));
  const listeningTo = $derived(snap.artist.trim() || "Unknown artist");

  // The listen link exists only for a remote track; a local file has no watch
  // page, so the button is not offered rather than pointing nowhere.
  const listenUrl = $derived(
    isRemote ? `https://music.youtube.com/watch?v=${ytmVideoId(snap.path)}` : "",
  );
  const projectUrl = "https://github.com/dlcuy22/molo";

  const progress = $derived(
    snap.duration > 0 ? Math.min(1, Math.max(0, snap.position / snap.duration)) : 0,
  );

  // The avatar is the one piece the snapshot does not carry as bytes. It is
  // fetched once per artist and cached, and it is only shown for a remote
  // track; a local file falls back to the application mark.
  let avatar = $state("");
  $effect(() => {
    const id = snap.artistId;
    if (id === "") {
      avatar = "";

      return;
    }
    let cancelled = false;
    artistAvatar(id).then((url) => {
      if (!cancelled) avatar = url;
    });

    return () => {
      cancelled = true;
    };
  });

  const statusLine = $derived(
    !$discordStatus.enabled
      ? "Off"
      : $discordStatus.connected
        ? "Connected to Discord"
        : "Waiting for Discord",
  );

  function toggle() {
    commands.setDiscordEnabled(!$discordStatus.enabled);
  }

  function open(url: string) {
    if (url !== "") {
      void Browser.OpenURL(url);
    }
  }
</script>

<!--
  Discord Rich Presence: the switch that turns it on, and a preview of the card
  it publishes. The preview is the point: Rich Presence is invisible from inside
  the app, so showing what Discord will draw is the only honest way to let a
  listener confirm it before opening their profile.
-->
<div class="flex flex-col gap-3">
  <div class="flex items-center justify-between gap-2">
    <div class="min-w-0">
      <h2 class="flex items-center gap-1.5 text-xs font-medium text-muted">
        <DiscordLogo size="14" aria-hidden="true" />
        Discord
      </h2>
      <p class="truncate text-[11px] text-muted">{statusLine}</p>
    </div>
    <label class="relative inline-flex shrink-0 items-center">
      <input
        class="peer h-4 w-7 cursor-pointer appearance-none rounded-full bg-track transition-colors checked:bg-accent"
        type="checkbox"
        checked={$discordStatus.enabled}
        aria-label="Show what is playing on Discord"
        onchange={toggle}
      />
      <span
        class="pointer-events-none absolute left-0.5 size-3 rounded-full bg-fg transition-transform peer-checked:translate-x-3"
        aria-hidden="true"
      ></span>
    </label>
  </div>

  <!-- The card is drawn dimmed while presence is off, so the preview reads as
       "this is what it will look like" rather than a live state. -->
  <div
    class="rounded-[6px] border border-line bg-bg/60 p-3 transition-opacity"
    class:opacity-50={!$discordStatus.enabled}
    aria-hidden={!$discordStatus.enabled}
  >
    {#if title === ""}
      <p class="py-6 text-center text-xs text-muted">
        Play a track to see the Discord card.
      </p>
    {:else}
      <p class="text-[11px] font-semibold uppercase tracking-wide text-muted">
        Listening to {listeningTo}
      </p>

      <div class="mt-2 flex items-start gap-3">
        <div class="relative size-[72px] shrink-0">
          {#if $cover}
            <img
              src={$cover}
              alt=""
              class="size-[72px] rounded-[4px] border border-line object-cover"
            />
          {:else}
            <div
              class="grid size-[72px] place-items-center rounded-[4px] border border-line bg-surface"
              aria-hidden="true"
            >
              <MusicNotes size="24" class="text-muted" />
            </div>
          {/if}
          {#if isRemote}
            <span
              class="absolute -bottom-1 -right-1 grid size-6 place-items-center rounded-full border-2 border-bg bg-surface"
            >
              {#if avatar}
                <img src={avatar} alt="" class="size-full rounded-full object-cover" />
              {:else}
                <DiscordLogo size="12" class="text-muted" aria-hidden="true" />
              {/if}
            </span>
          {/if}
        </div>

        <div class="min-w-0 flex-1 pt-0.5">
          <p class="truncate text-sm font-semibold text-fg" title={title}>{title}</p>
          <p class="truncate text-xs text-muted">{listeningTo}</p>
          {#if snap.album.trim() !== ""}
            <p class="truncate text-[11px] text-muted">{snap.album}</p>
          {/if}
          {#if snap.duration > 0}
            <p class="mt-1 text-[11px] tabular-nums text-muted">
              {formatTime(snap.position)} / {formatTime(snap.duration)}
            </p>
            <span class="mt-1 block h-[3px] w-full overflow-hidden rounded-full bg-track">
              <span
                class="block h-full rounded-full bg-accent transition-[width] duration-150 ease-linear"
                style="width: {progress * 100}%"
              ></span>
            </span>
          {/if}
        </div>
      </div>

      <div class="mt-3 flex flex-col gap-1.5">
        {#if listenUrl !== ""}
          <button
            class="w-full rounded-[4px] border border-line bg-surface px-2 py-1.5 text-xs text-fg transition-colors hover:bg-hover"
            onclick={() => open(listenUrl)}
          >
            Listen on YouTube Music
          </button>
        {/if}
        <button
          class="w-full rounded-[4px] border border-line bg-surface px-2 py-1.5 text-xs text-fg transition-colors hover:bg-hover"
          onclick={() => open(projectUrl)}
        >
          Visit molo
        </button>
      </div>
    {/if}
  </div>
</div>
