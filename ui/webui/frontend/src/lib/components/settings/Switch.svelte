<script lang="ts">
  // Switch: a two-state control. It is the same markup the Discord and
  // visualizer switches already use, extracted so a settings row and those
  // panels cannot drift into two looks. The native checkbox is kept (and
  // visually hidden under the track) so keyboard focus, Space and the accessible
  // role are the browser's own.
  let {
    checked,
    label,
    onchange,
  }: { checked: boolean; label: string; onchange: (value: boolean) => void } = $props();
</script>

<label class="relative inline-flex shrink-0 items-center">
  <input
    class="peer h-4 w-7 cursor-pointer appearance-none rounded-full bg-track transition-colors checked:bg-accent"
    type="checkbox"
    {checked}
    aria-label={label}
    onchange={(e) => onchange(e.currentTarget.checked)}
  />
  <span
    class="pointer-events-none absolute left-0.5 size-3 rounded-full bg-fg transition-transform peer-checked:translate-x-3"
    aria-hidden="true"
  ></span>
</label>
