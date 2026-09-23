// format.ts holds the pure display helpers. They live together because every
// one of them is about turning an engine value into text, and a component that
// needs one usually needs two.

/** formatTime renders milliseconds as m:ss, or h:mm:ss past an hour. An unknown
 *  duration (0) renders as "--:--" rather than "0:00", so an unresolved probe
 *  is not shown as an empty track. */
export function formatTime(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) {
    return "--:--";
  }

  const total = Math.floor(ms / 1000);
  const s = total % 60;
  const m = Math.floor(total / 60) % 60;
  const h = Math.floor(total / 3600);

  const pad = (n: number) => String(n).padStart(2, "0");
  if (h > 0) {
    return `${h}:${pad(m)}:${pad(s)}`;
  }

  return `${m}:${pad(s)}`;
}

/**
 * displayTitle picks the best name for the current track: the tag if it has
 * one, otherwise the file name. It is the same precedence the TUI uses, so the
 * two front ends name a track identically.
 */
export function displayTitle(title: string, path: string): string {
  const tag = title.trim();
  if (tag !== "") {
    return tag;
  }
  if (path === "") {
    return "";
  }

  return baseName(path);
}

/** subtitle joins artist and album, omitting either when absent so a
 *  half-tagged file does not render a dangling separator. */
export function subtitle(artist: string, album: string): string {
  const a = artist.trim();
  const b = album.trim();
  if (a !== "" && b !== "") {
    return `${a} · ${b}`;
  }

  return a !== "" ? a : b;
}

/** baseName is path.basename for the browser: the last segment, POSIX or
 *  Windows separators both handled. */
export function baseName(path: string): string {
  const cut = Math.max(path.lastIndexOf("/"), path.lastIndexOf("\\"));

  return cut >= 0 ? path.slice(cut + 1) : path;
}

/** percent clamps a [0,1] value to a whole percentage for display. */
export function percent(v: number): number {
  if (!Number.isFinite(v)) {
    return 0;
  }

  return Math.round(Math.min(1, Math.max(0, v)) * 100);
}

/** fillFrac maps a slider value to the 0..1 fraction the range fill style
 *  wants. It clamps and guards a zero span, so a wayward value never pushes
 *  the fill past the thumb. */
export function fillFrac(v: number, min: number, max: number): number {
  if (
    !Number.isFinite(v) ||
    !Number.isFinite(min) ||
    !Number.isFinite(max) ||
    max <= min
  ) {
    return 0;
  }

  return Math.min(1, Math.max(0, (v - min) / (max - min)));
}

/** isActive reports whether a state should render as in-flight, which decides
 *  the transport buttons' enabled look. */
export function isActive(state: string): boolean {
  return state === "playing" || state === "paused";
}
