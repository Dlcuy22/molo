// Shortcuts: one keyboard listener for the whole app.
//
// The playback keys and the three modal chords share a single handler on
// purpose. Two listeners means two places that can each forget the gating, and
// the gating is the whole feature: a shortcut that fires while a text field has
// focus, or while a modal owns the keyboard, is worse than no shortcut. Keeping
// the matcher pure and DOM-free lets the keymap and the gates be tested without
// a browser.

export type ShortcutAction =
  | "volume-up"
  | "volume-down"
  | "next"
  | "prev"
  | "shuffle"
  | "visualizer"
  | "mute"
  | "repeat-all"
  | "repeat-one";

/** ShortcutKeyEvent is the slice of KeyboardEvent the matcher reads. A plain
 *  object works, so a test drives the keymap without constructing an event. */
export interface ShortcutKeyEvent {
  key: string;
  code: string;
  shiftKey: boolean;
}

/** ShortcutGateEvent adds the modifiers the gate reads. It is separate from the
 *  matcher's event so the matcher cannot accidentally start rejecting on a
 *  modifier: `*` is Shift+8 and `<` is Shift+comma, so a blanket shift check
 *  would eat two real keys. */
export interface ShortcutGateEvent extends ShortcutKeyEvent {
  ctrlKey: boolean;
  metaKey: boolean;
  altKey: boolean;
}

/**
 * matchShortcut maps a keydown to a playback action, or null.
 *
 * The numpad is checked by `e.code` and the symbols by `e.key`, both, because
 * neither alone is enough: a non-US layout puts a different symbol on the
 * numpad keys, and `e.code` only spells the physical key for the numpad and the
 * digit row. The digit-row codes are gated on `!e.shiftKey`, because Shift+9
 * and Shift+0 type `(` and `)`: those are not the bound keys, and without the
 * guard the code fallback would fire volume on a user reaching for a paren. The
 * numpad codes need no such guard (Shift does not change them), and `*` and `/`
 * still arrive through `e.key` on their own. A shifted key is otherwise never
 * rejected here: Shift+8 is `*`, Shift+comma is `<`, and `L` is Shift+L.
 */
export function matchShortcut(e: ShortcutKeyEvent): ShortcutAction | null {
  switch (e.key) {
    case "*":
    case "0":
      return "volume-up";
    case "/":
    case "9":
      return "volume-down";
    case ">":
    case ".":
      return "next";
    case "<":
    case ",":
      return "prev";
    case "L":
      // Uppercase is the shifted key. It is matched before the lowercase pass
      // below so `L` cannot be read as repeat-all.
      return "repeat-one";
  }

  switch (e.code) {
    case "NumpadMultiply":
      return "volume-up";
    case "NumpadDivide":
      return "volume-down";
    case "Digit0":
      if (!e.shiftKey) {
        return "volume-up";
      }

      return null;
    case "Digit9":
      if (!e.shiftKey) {
        return "volume-down";
      }

      return null;
  }

  switch (e.key.toLowerCase()) {
    case "n":
      return "next";
    case "s":
      return "shuffle";
    case "v":
      return "visualizer";
    case "m":
      return "mute";
    case "l":
      return "repeat-all";
  }

  return null;
}

// The input types that mean "the user is typing". Everything else an input can
// be (range, checkbox, radio, button, file) owns its own arrow keys but never
// takes a letter, so a bare shortcut over one should still fire. A range slider
// is the case that matters: it consumes the arrows and Home/End for its own
// value, and nothing types into it, so swallowing `s` or `m` over a focused
// slider would take a key the slider cannot use. An input with no type reads as
// text in the DOM, so absence is text-like here too.
const TEXT_INPUT_TYPES = new Set([
  "text",
  "search",
  "url",
  "email",
  "number",
  "password",
]);

/**
 * shouldIgnoreTarget reports whether a keydown on this element is typing rather
 * than a shortcut. It reads only the properties a target carries, so a test can
 * pass a plain object; the live caller passes document.activeElement.
 */
export function shouldIgnoreTarget(target: EventTarget | null): boolean {
  if (target === null) {
    return false;
  }
  const el = target as {
    tagName?: string;
    type?: string;
    isContentEditable?: boolean;
  };
  const tag = (el.tagName ?? "").toUpperCase();

  // A select or a textarea is either typing or arrow navigation, and neither
  // wants a playback key.
  if (tag === "SELECT" || tag === "TEXTAREA") {
    return true;
  }
  if (tag === "INPUT") {
    const type = (el.type ?? "text").toLowerCase();

    return type === "" || TEXT_INPUT_TYPES.has(type);
  }
  // isContentEditable is true on an editable node and on its descendants, so a
  // caret inside a rich field is caught without walking up the tree.
  return el.isContentEditable === true;
}

/**
 * ShortcutContext is everything the handler needs and nothing it owns. The
 * palette and settings stay in App.svelte; this module only decides whether a
 * key is a shortcut and calls back.
 */
export interface ShortcutContext {
  isModalOpen(): boolean;
  isPaletteOpen(): boolean;
  isEditableFocused(): boolean;
  openSettings(): void;
  openPalette(source: "queue" | "ytm", preview: boolean): void;
  closePaletteOrSwitch(source: "queue" | "ytm"): void;
  action(a: ShortcutAction): void;
}

/** FocusEnv is the window state the gate reads, lifted out so the gate is
 *  testable without a document. */
export interface FocusEnv {
  hasFocus: boolean;
  visible: boolean;
}
/**
 * shortcutFor applies every gate and returns the action a keydown should run,
 * or null. The order is the plan's: a modal owns the keyboard, then a held
 * modifier means a chord, then a typing target, then the window must be focused
 * and visible. hasFocus is the seatbelt for a future regression, not the main
 * guard: the webview only delivers keydown while its window is focused.
 */
export function shortcutFor(
  e: ShortcutGateEvent,
  ctx: ShortcutContext,
  env: FocusEnv,
): ShortcutAction | null {
  if (ctx.isModalOpen()) {
    return null;
  }
  if (e.ctrlKey || e.metaKey || e.altKey) {
    return null;
  }
  if (ctx.isEditableFocused()) {
    return null;
  }
  if (!env.hasFocus || !env.visible) {
    return null;
  }

  return matchShortcut(e);
}

/**
 * KeydownEventLike is the slice of KeyboardEvent the full handler reads: the
 * gate's fields plus the one method it calls. A plain object with a spy
 * preventDefault works, so the whole dispatch is testable without a DOM.
 */
export interface KeydownEventLike extends ShortcutGateEvent {
  preventDefault(): void;
}

/**
 * handleKeydown is the whole decision: modal chords first, then the playback
 * gate, and preventDefault only when something actually dispatched. Returning
 * whether it dispatched lets installKeyboard stay a thin attach, and lets a
 * test drive every branch with a plain event object. The modal chords are tried
 * before the gate on purpose: a chord must reach its modal even while a modal
 * owns the keyboard (that is how the modal is closed), so the gate cannot
 * precede them.
 */
export function handleKeydown(
  e: KeydownEventLike,
  ctx: ShortcutContext,
  env: FocusEnv,
): boolean {
  if (handleModalChord(e, ctx)) {
    return true;
  }
  const action = shortcutFor(e, ctx, env);
  if (action === null) {
    return false;
  }
  e.preventDefault();
  ctx.action(action);

  return true;
}

/**
 * handleModalChord runs the Ctrl/Cmd chords and reports whether it consumed the
 * event. It is the exact behaviour the inline App.svelte handler had, kept here
 * so there is one keyboard path: Ctrl+I toggles settings, Ctrl+Alt+P opens the
 * queue palette straight into preview (or is left to the open palette), Ctrl+F
 * opens the YouTube palette, and Ctrl+P opens the queue palette or closes it
 * when the queue source is already showing. ctrlKey and metaKey are the same
 * chord on purpose, so a mac user gets Command without a second table.
 */
function handleModalChord(e: KeydownEventLike, ctx: ShortcutContext): boolean {
  if (!e.ctrlKey && !e.metaKey) {
    return false;
  }
  const key = e.key.toLowerCase();

  if (!e.altKey && key === "i") {
    e.preventDefault();
    ctx.openSettings();

    return true;
  }
  if (e.altKey && key === "p") {
    e.preventDefault();
    if (ctx.isPaletteOpen()) {
      // Already open: the palette owns this chord and toggles its own preview
      // mode, so the app must not double-handle it.
      return true;
    }
    ctx.openPalette("queue", true);

    return true;
  }
  if (!e.altKey && key === "f") {
    e.preventDefault();
    ctx.openPalette("ytm", false);

    return true;
  }
  if (!e.altKey && key === "p") {
    e.preventDefault();
    // Ctrl+P always means the queue search: open it, or close it when that
    // source is already showing; from the YouTube source it is a switch back.
    ctx.closePaletteOrSwitch("queue");

    return true;
  }

  return false;
}

/**
 * installKeyboard attaches the single window keydown listener and returns its
 * teardown. It only supplies the live window state; the decision lives in
 * handleKeydown, which a test can run without a browser.
 */
export function installKeyboard(ctx: ShortcutContext): () => void {
  const onKey = (e: KeyboardEvent) => {
    handleKeydown(e, ctx, {
      hasFocus: document.hasFocus(),
      visible: document.visibilityState === "visible",
    });
  };
  window.addEventListener("keydown", onKey);

  return () => window.removeEventListener("keydown", onKey);
}
