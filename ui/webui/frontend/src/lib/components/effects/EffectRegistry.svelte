<script lang="ts">
  import { MagnifyingGlass, Plus } from "phosphor-svelte";
  import type { EffectKind } from "../../effect-types";

  // The catalogue of effects that can be added. It is not the chain: adding one
  // emits the kind and impl and lets the caller place it.
  let {
    kinds,
    onadd,
  }: { kinds: EffectKind[]; onadd: (kind: string, impl: string) => void } = $props();

  let query = $state("");

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

  // Group order follows the catalogue's own order, so a stable sort of the
  // source keeps the engine's declared priority.
  const groups = $derived.by(() => {
    const order: string[] = [];
    const byKind = new Map<string, EffectKind[]>();
    for (const k of filtered) {
      let list = byKind.get(k.kind);
      if (!list) {
        list = [];
        byKind.set(k.kind, list);
        order.push(k.kind);
      }
      list.push(k);
    }

    return order.map((kind) => ({ kind, entries: byKind.get(kind) ?? [] }));
  });
</script>

<div class="flex h-full flex-col gap-2">
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
        {#each groups as group (group.kind)}
          <section class="flex flex-col gap-1 py-1">
            <h3 class="px-2 text-[11px] font-medium uppercase tracking-wide text-muted">
              {group.kind}
            </h3>
            {#each group.entries as k (k.kind + k.impl)}
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
          </section>
        {/each}
      </div>
    {/if}
  {/if}
</div>
