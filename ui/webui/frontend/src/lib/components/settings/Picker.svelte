<script lang="ts">
  import { CaretDown } from "phosphor-svelte";

  // Picker: a labelled dropdown over a fixed set of choices. Each option is a
  // value plus the label shown for it; the value is what is emitted, so a
  // caller can carry an empty string (Auto) as a real choice, which a plain
  // value binding cannot express. The caret is decoration over a native select,
  // so keyboard, Escape and the platform menu are the browser's own.
  export interface PickerOption {
    value: string;
    label: string;
  }

  let {
    value,
    options,
    label,
    disabled = false,
    onchange,
  }: {
    value: string;
    options: PickerOption[];
    label: string;
    disabled?: boolean;
    onchange: (value: string) => void;
  } = $props();
</script>

<label class="flex flex-col gap-1 text-xs text-muted">
  <span class="truncate">{label}</span>
  <span class="relative">
    <select
      class="w-full appearance-none rounded-[4px] border border-line bg-surface py-1.5 pl-2.5 pr-7 text-xs text-fg transition-colors hover:bg-hover disabled:text-disabled"
      {disabled}
      aria-label={label}
      onchange={(e) => onchange(e.currentTarget.value)}
    >
      {#each options as o (o.value)}
        <option value={o.value} selected={o.value === value} class="bg-surface text-fg">
          {o.label}
        </option>
      {/each}
    </select>
    <CaretDown
      size="12"
      class="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 text-muted"
      aria-hidden="true"
    />
  </span>
</label>
