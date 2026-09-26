<script lang="ts">
  import { onMount } from "svelte";
  import { DotsSixVertical, Plus } from "phosphor-svelte";
  import { commands, connect, effectChain, effectKinds } from "../store";
  import EffectPanel from "./effects/EffectPanel.svelte";
  import EffectSidebar from "./effects/EffectSidebar.svelte";
  import EffectRegistry from "./effects/EffectRegistry.svelte";

  // The effect window's shell. The chain lives in the sidebar, top to bottom in
  // processing order, because that order is the signal path the audio takes.
  // The main pane shows the selected stage's controls, and the tabs split the
  // editor from the catalogue of effects that can be added.
  onMount(connect);

  type Tab = "controls" | "registry" | "script";
  let tab = $state<Tab>("controls");
  let selectedId = $state("");

  const stages = $derived($effectChain);
  const kinds = $derived($effectKinds);
  // The scripted effects are the registry entries whose kind is the script
  // slot. They are listed separately because a script is a file on disk, not a
  // built-in, and a listener wants to see which ones loaded.
  const scripted = $derived(kinds.filter((k) => k.scripted));

  // Keep the selection valid as the chain changes: a removed stage must not
  // leave the pane pointed at nothing, and the first add should select itself.
  $effect(() => {
    const list = stages;
    if (list.length === 0) {
      selectedId = "";
    } else if (!list.some((s) => s.id === selectedId)) {
      selectedId = list[0].id;
    }
  });

  const selected = $derived(stages.find((s) => s.id === selectedId) ?? null);

  function add(kind: string, impl: string) {
    void commands.addEffect(kind, impl);
    tab = "controls";
  }

  // Right-click on the header offers "open in new window". The window is a
  // singleton the Go side finds by name, so this is idempotent: a second click
  // focuses the window already open rather than stacking another.
  let menuOpen = $state(false);
  let menuX = $state(0);
  let menuY = $state(0);

  function openHeaderMenu(e: MouseEvent) {
    e.preventDefault();
    menuOpen = true;
    menuX = e.clientX;
    menuY = e.clientY;
  }

  function closeMenu() {
    menuOpen = false;
  }

  function openInNewWindow() {
    menuOpen = false;
    void commands.openEffectWindow();
  }

  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        menuOpen = false;
      }
    };
    const onClick = () => {
      if (menuOpen) {
        menuOpen = false;
      }
    };
    window.addEventListener("keydown", onKey);
    window.addEventListener("click", onClick);

    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("click", onClick);
    };
  });
</script>

<!--
  Two columns: the chain as the signal path on the left, the editor on the
  right. The tabs stay inside this window; the header is what detaches.
-->
<div class="flex h-full flex-col bg-bg text-fg">
  <div
    class="flex shrink-0 items-center justify-between gap-3 border-b border-line bg-surface px-4 py-2.5"
    role="banner"
    oncontextmenu={openHeaderMenu}
    title="Right-click for window options"
  >
    <div class="flex items-center gap-2">
      <DotsSixVertical size="16" class="text-muted" aria-hidden="true" />
      <h1 class="text-sm font-medium">Effects</h1>
    </div>
    <nav class="flex items-center gap-1" aria-label="Effect window sections">
      <button
        class="rounded-[4px] px-2.5 py-1 text-xs transition-colors"
        class:bg-selected={tab === "controls"}
        class:text-fg={tab === "controls"}
        class:text-muted={tab !== "controls"}
        aria-current={tab === "controls" ? "page" : undefined}
        onclick={() => (tab = "controls")}
      >
        Controls
      </button>
      <button
        class="rounded-[4px] px-2.5 py-1 text-xs transition-colors"
        class:bg-selected={tab === "registry"}
        class:text-fg={tab === "registry"}
        class:text-muted={tab !== "registry"}
        aria-current={tab === "registry" ? "page" : undefined}
        onclick={() => (tab = "registry")}
      >
        Registry
      </button>
      <button
        class="rounded-[4px] px-2.5 py-1 text-xs transition-colors"
        class:bg-selected={tab === "script"}
        class:text-fg={tab === "script"}
        class:text-muted={tab !== "script"}
        aria-current={tab === "script" ? "page" : undefined}
        onclick={() => (tab = "script")}
      >
        Script
      </button>
    </nav>
  </div>

  <div class="flex min-h-0 flex-1">
    <aside
      class="flex w-56 shrink-0 flex-col border-r border-line bg-surface"
      aria-label="Effect chain"
    >
      <div class="flex items-center justify-between gap-2 px-3 py-2">
        <h2 class="text-xs font-medium text-muted">Chain</h2>
        <button
          class="flex size-8 items-center justify-center rounded-[4px] text-muted transition-colors hover:bg-hover hover:text-fg focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
          aria-label="Add an effect"
          onclick={() => (tab = "registry")}
        >
          <Plus size="14" aria-hidden="true" />
        </button>
      </div>
      <div class="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
        <EffectSidebar
          {stages}
          {selectedId}
          onselect={(id) => {
            selectedId = id;
            tab = "controls";
          }}
          onmove={(id, to) => commands.moveEffect(id, to)}
          onremove={(id) => commands.removeEffect(id)}
        />
      </div>
    </aside>

    <main class="min-h-0 flex-1 overflow-y-auto p-4">
      {#if tab === "registry"}
        <EffectRegistry {kinds} onadd={add} />
      {:else if tab === "script"}
        <!-- The script tab lists the Lua effects the engine loaded, so a
             listener can see what is available and add one. Editing happens in
             the file itself; this window is not an editor. -->
        <div class="flex flex-col gap-3">
          <div>
            <h2 class="text-sm font-medium text-fg">Lua effects</h2>
            <p class="mt-1 text-xs text-muted">
              Effects loaded from Lua scripts. Each one appears in the registry
              like a built-in.
            </p>
          </div>
          {#if scripted.length === 0}
            <p class="text-xs text-muted">
              No Lua effects loaded. Add a .lua file to the effects directory and
              restart the player.
            </p>
          {:else}
            <ul class="flex flex-col gap-1">
              {#each scripted as s (s.kind + "/" + s.impl)}
                <li
                  class="flex items-center justify-between gap-2 rounded-[4px] border border-line bg-surface px-3 py-2"
                >
                  <span class="min-w-0 truncate text-xs text-fg">
                    {s.label.trim() || s.impl}
                  </span>
                  <button
                    class="shrink-0 rounded-[4px] border border-line px-2 py-1.5 text-[11px] text-fg transition-colors hover:bg-hover focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
                    onclick={() => add(s.kind, s.impl)}
                  >
                    Add
                  </button>
                </li>
              {/each}
            </ul>
          {/if}
        </div>
      {:else if selected}
        <EffectPanel
          stage={selected}
          onparam={(key, value) => commands.setEffectParam(selected.id, key, value)}
          onbypass={(bypassed) => commands.setEffectBypass(selected.id, bypassed)}
        />
      {:else}
        <div class="flex h-full flex-col items-center justify-center gap-2 text-center">
          <p class="text-sm text-muted">No effects in the chain yet.</p>
          <button
            class="rounded-[4px] border border-line bg-surface px-3 py-1.5 text-xs text-fg transition-colors hover:bg-hover"
            onclick={() => (tab = "registry")}
          >
            Add an effect
          </button>
        </div>
      {/if}
    </main>
  </div>
</div>

{#if menuOpen}
  <!-- A native-feeling context menu positioned at the pointer. It closes on the
       next click anywhere, so it never strands on screen. -->
  <div
    class="fixed z-50 min-w-44 rounded-[6px] border border-line bg-surface py-1 shadow-lg"
    style="left: {menuX}px; top: {menuY}px;"
    role="menu"
  >
    <button
      class="block w-full px-3 py-1.5 text-left text-xs text-fg transition-colors hover:bg-hover"
      role="menuitem"
      onclick={openInNewWindow}
    >
      Open in new window
    </button>
  </div>
{/if}
