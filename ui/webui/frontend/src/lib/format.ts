// format.ts holds the pure display helpers. They live together because every
// one of them is about turning an engine value into text, and a component that
// needs one usually needs two.

/** formatTime renders milliseconds as m:ss, or h:mm:ss past an hour. A negative
 *  value is an unknown duration and renders as "--:--"; zero is a real zero, so
 *  an elapsed time at the start of a track reads "0:00". */
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
 * two front ends name a track identically. A remote reference has no file name,
 * so it falls back to nothing rather than to its identifier.
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

/**
 * A provider reference is an identifier, not a file path. The scheme marks it
 * as remote: the UI knows one provider today ("ytm:"), and this is the single
 * place that has to learn about a second one.
 */
export function isRemoteRef(path: string): boolean {
  return path.startsWith("ytm:");
}

/** ytmVideoId reads the video id out of a remote reference, or "" when the path
 *  is not a YouTube Music reference. The prefix length lives here so the
 *  watch-link builder and any other consumer stay in step. */
export function ytmVideoId(path: string): string {
  return isRemoteRef(path) ? path.slice("ytm:".length) : "";
}

/** baseName is path.basename for the browser: the last segment, POSIX or
 *  Windows separators both handled. A remote reference has no file name, so it
 *  yields nothing rather than "ytm:abc123" shown as a title. */
export function baseName(path: string): string {
  if (isRemoteRef(path)) {
    return "";
  }

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
