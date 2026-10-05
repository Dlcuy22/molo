<script lang="ts">
  import { MagnifyingGlass, Plus } from "phosphor-svelte";
  import type { EffectKind } from "../../effect-types";
  import type { PresetImportResult } from "../../../../bindings/github.com/dlcuy22/molo/ui/webui/models";

  // The catalogue of effects that can be added. It is not the chain: adding one
  // emits the kind and impl and lets the caller place it.
  let {
    kinds,
    onadd,
    onimport,
  }: {
    kinds: EffectKind[];
    onadd: (kind: string, impl: string) => void;
    onimport: () => Promise<PresetImportResult | null>;
  } = $props();

  let query = $state("");

  // The import is a slow command (a native dialog and a chain rebuild), so it
  // carries its own busy flag and its own result, separate from the catalogue.
  let importing = $state(false);
  let imported = $state<PresetImportResult | null>(null);

  async function runImport() {
    importing = true;
    imported = null;
    try {
      imported = await onimport();
    } finally {
      importing = false;
    }
  }

  const warnings = $derived(imported?.warnings ?? []);

  const filtered = $derived(
    kinds.filter((k) => {
      const q = query.trim().toLowerCase();
      if (q === "") {
        return true;
      }

      return (
        k.label.toLowerCase().includes(q) ||
        k.kind.toLowerCase().includes(q) ||
        k.impl.toLowerCase().includes(q)
      );
    }),
  );
</script>

<div class="flex h-full flex-col gap-2">
  <div class="flex items-center justify-between gap-2 border-b border-line pb-2">
    <p class="text-xs text-muted">Import an EasyEffects preset to replace the chain.</p>
    <button
      type="button"
      class="shrink-0 rounded-[4px] border border-line px-2.5 py-1.5 text-[11px] text-fg transition-colors hover:bg-hover disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
      disabled={importing}
      onclick={runImport}
    >
      {importing ? "Importing…" : "Import preset"}
    </button>
  </div>

  {#if imported}
    <div class="rounded-[4px] border border-line bg-surface px-3 py-2 text-[11px] text-fg">
      <p>
        Imported {imported.stages}
        {imported.stages === 1 ? "stage" : "stages"}.
      </p>
      {#if warnings.length > 0}
        <details class="mt-1">
          <summary class="cursor-pointer text-muted">
            {warnings.length} setting{warnings.length === 1 ? "" : "s"} skipped
          </summary>
          <ul class="mt-1 flex flex-col gap-0.5 text-muted">
            {#each warnings as w}
              <li>{w}</li>
            {/each}
          </ul>
        </details>
      {/if}
    </div>
  {/if}

  {#if kinds.length === 0}
    <p class="px-3 py-6 text-sm text-muted">
      No effects are available. The engine reported an empty catalogue.
    </p>
  {:else}
    <label class="flex items-center gap-2 rounded-[4px] border border-line bg-bg px-2 focus-within:ring-2 focus-within:ring-inset focus-within:ring-focus">
      <MagnifyingGlass size="13" class="shrink-0 text-muted" aria-hidden="true" />
      <input
        bind:value={query}
        class="w-full bg-transparent py-1.5 text-xs text-fg outline-none! placeholder:text-muted"
        type="text"
        placeholder="Filter effects"
        autocomplete="off"
        spellcheck="false"
        aria-label="Filter effects"
      />
    </label>

    {#if filtered.length === 0}
      <p class="px-3 py-6 text-sm text-muted">No effect matches “{query}”.</p>
    {:else}
      <div class="scroll-thin min-h-0 flex-1 overflow-y-auto">
        {#each filtered as k (k.kind + k.impl)}
          <div class="flex items-center gap-2 rounded-[4px] px-2 py-1 hover:bg-hover">
            <div class="flex min-w-0 flex-1 flex-col">
              <span class="truncate text-sm text-fg">{k.label.trim() || k.kind}</span>
              <span class="truncate text-[11px] text-muted">
                {k.impl}
                {#if k.scripted}<span class="ml-1 text-fg">Scripted</span>{/if}
              </span>
            </div>
            <button
              type="button"
              class="grid size-8 shrink-0 place-items-center rounded-[4px] text-muted transition-colors hover:bg-selected hover:text-fg"
              aria-label="Add {k.label.trim() || k.kind}"
              title="Add {k.label.trim() || k.kind}"
              onclick={() => onadd(k.kind, k.impl)}
            >
              <Plus size="14" weight="bold" />
            </button>
          </div>
        {/each}
      </div>
    {/if}
  {/if}
</div>
