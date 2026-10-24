import { describe, expect, it } from "vitest";
import {
  handleKeydown,
  matchShortcut,
  shortcutFor,
  shouldIgnoreTarget,
  type FocusEnv,
  type KeydownEventLike,
  type ShortcutContext,
  type ShortcutGateEvent,
} from "./shortcuts";

// matchShortcut reads only key, code and shiftKey, so a plain object drives it
// without a browser. The default event is a bare keypress with nothing held.
function key(over: Partial<ShortcutGateEvent>): ShortcutGateEvent {
  return {
    key: "",
    code: "",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    altKey: false,
    ...over,
  };
}

describe("matchShortcut", () => {
  it("maps the symbol and digit forms on the main row", () => {
    expect(matchShortcut(key({ key: "*" }))).toBe("volume-up");
    expect(matchShortcut(key({ key: "0" }))).toBe("volume-up");
    expect(matchShortcut(key({ key: "/" }))).toBe("volume-down");
    expect(matchShortcut(key({ key: "9" }))).toBe("volume-down");
  });

  it("recovers the numpad keys from e.code on a non-US layout", () => {
    // The key itself is the layout's symbol, so only the code gives the numpad
    // away; both forms must reach the same action.
    expect(matchShortcut(key({ key: "×", code: "NumpadMultiply" }))).toBe("volume-up");
    expect(matchShortcut(key({ key: "÷", code: "NumpadDivide" }))).toBe("volume-down");
  });

  it("maps the unshifted digit row through e.key and e.code", () => {
    expect(matchShortcut(key({ key: "0", code: "Digit0" }))).toBe("volume-up");
    expect(matchShortcut(key({ key: "9", code: "Digit9" }))).toBe("volume-down");
    // A layout that puts something else on the digit row still reaches the
    // action through the code.
    expect(matchShortcut(key({ key: ")", code: "Digit0" }))).toBe("volume-up");
    expect(matchShortcut(key({ key: "(", code: "Digit9" }))).toBe("volume-down");
  });

  it("does not fire volume on the shifted digit row", () => {
    // Shift+9 types "(" and Shift+0 types ")". Those are not bound keys, and
    // the code fallback must not turn them into volume.
    expect(matchShortcut(key({ key: "(", code: "Digit9", shiftKey: true }))).toBeNull();
    expect(matchShortcut(key({ key: ")", code: "Digit0", shiftKey: true }))).toBeNull();
    // The numpad and the explicitly bound symbols keep working while shifted.
    expect(matchShortcut(key({ key: "*", code: "NumpadMultiply", shiftKey: true }))).toBe(
      "volume-up",
    );
    expect(matchShortcut(key({ key: "/", code: "NumpadDivide", shiftKey: true }))).toBe(
      "volume-down",
    );
  });

  it("maps the track keys, shifted and plain", () => {
    expect(matchShortcut(key({ key: "n" }))).toBe("next");
    expect(matchShortcut(key({ key: ">" }))).toBe("next");
    expect(matchShortcut(key({ key: "." }))).toBe("next");
    expect(matchShortcut(key({ key: "<" }))).toBe("prev");
    expect(matchShortcut(key({ key: "," }))).toBe("prev");
  });

  it("maps the toggles", () => {
    expect(matchShortcut(key({ key: "s" }))).toBe("shuffle");
    expect(matchShortcut(key({ key: "v" }))).toBe("visualizer");
    expect(matchShortcut(key({ key: "m" }))).toBe("mute");
  });

  it("splits repeat by case: l is all, L is one", () => {
    expect(matchShortcut(key({ key: "l" }))).toBe("repeat-all");
    expect(matchShortcut(key({ key: "L", shiftKey: true }))).toBe("repeat-one");
  });

  it("does not reject the shifted forms", () => {
    // `*` arrives as Shift+8 and `<` as Shift+comma; neither is a chord.
    expect(matchShortcut(key({ key: "*", shiftKey: true }))).toBe("volume-up");
    expect(matchShortcut(key({ key: "<", shiftKey: true }))).toBe("prev");
  });

  it("is null for a key it does not own", () => {
    expect(matchShortcut(key({ key: "q" }))).toBeNull();
    expect(matchShortcut(key({ key: "Escape" }))).toBeNull();
    expect(matchShortcut(key({ key: "Enter" }))).toBeNull();
  });
});

// shouldIgnoreTarget reads only the properties a target carries, so a small
// object stands in for a DOM element.
function el(over: Record<string, unknown>): EventTarget {
  return over as unknown as EventTarget;
}

describe("shouldIgnoreTarget", () => {
  it("ignores text fields and textareas and selects", () => {
    expect(shouldIgnoreTarget(el({ tagName: "INPUT", type: "text" }))).toBe(true);
    expect(shouldIgnoreTarget(el({ tagName: "INPUT", type: "search" }))).toBe(true);
    expect(shouldIgnoreTarget(el({ tagName: "INPUT", type: "email" }))).toBe(true);
    expect(shouldIgnoreTarget(el({ tagName: "INPUT", type: "number" }))).toBe(true);
    expect(shouldIgnoreTarget(el({ tagName: "TEXTAREA" }))).toBe(true);
    expect(shouldIgnoreTarget(el({ tagName: "SELECT" }))).toBe(true);
  });

  it("treats a typeless input as a text field", () => {
    // document.activeElement for <input> with no type reads as text.
    expect(shouldIgnoreTarget(el({ tagName: "INPUT" }))).toBe(true);
    expect(shouldIgnoreTarget(el({ tagName: "INPUT", type: "" }))).toBe(true);
  });

  it("lets a range own its keys without swallowing letters", () => {
    // A slider consumes the arrows and Home/End itself and never takes a
    // letter, so a bare shortcut over one still fires.
    expect(shouldIgnoreTarget(el({ tagName: "INPUT", type: "range" }))).toBe(false);
    expect(shouldIgnoreTarget(el({ tagName: "INPUT", type: "checkbox" }))).toBe(false);
    expect(shouldIgnoreTarget(el({ tagName: "BUTTON" }))).toBe(false);
  });

  it("ignores a contenteditable node", () => {
    expect(shouldIgnoreTarget(el({ tagName: "DIV", isContentEditable: true }))).toBe(true);
    expect(shouldIgnoreTarget(el({ tagName: "DIV", isContentEditable: false }))).toBe(false);
  });

  it("is not an ignore when nothing is focused", () => {
    expect(shouldIgnoreTarget(null)).toBe(false);
  });
});

// shortcutFor is the whole playback gate. The context is a plain object of
// callbacks so each gate can be flipped on its own.
function context(over: Partial<ShortcutContext> = {}): ShortcutContext {
  return {
    isModalOpen: () => false,
    isPaletteOpen: () => false,
    isEditableFocused: () => false,
    openSettings: () => {},
    openPalette: () => {},
    closePaletteOrSwitch: () => {},
    action: () => {},
    ...over,
  };
}

const focused: FocusEnv = { hasFocus: true, visible: true };

describe("shortcutFor gating", () => {
  it("passes a bare key when nothing blocks it", () => {
    expect(shortcutFor(key({ key: "n" }), context(), focused)).toBe("next");
  });

  it("ignores every key while a modal is open", () => {
    const ctx = context({ isModalOpen: () => true });
    expect(shortcutFor(key({ key: "n" }), ctx, focused)).toBeNull();
    expect(shortcutFor(key({ key: "m" }), ctx, focused)).toBeNull();
  });

  it("ignores a held control, meta or alt modifier", () => {
    expect(shortcutFor(key({ key: "n", ctrlKey: true }), context(), focused)).toBeNull();
    expect(shortcutFor(key({ key: "n", metaKey: true }), context(), focused)).toBeNull();
    expect(shortcutFor(key({ key: "n", altKey: true }), context(), focused)).toBeNull();
  });

  it("ignores a key typed into an editable target", () => {
    const ctx = context({ isEditableFocused: () => true });
    expect(shortcutFor(key({ key: "l" }), ctx, focused)).toBeNull();
  });

  it("ignores a key when the window is unfocused or hidden", () => {
    expect(shortcutFor(key({ key: "n" }), context(), { hasFocus: false, visible: true })).toBeNull();
    expect(shortcutFor(key({ key: "n" }), context(), { hasFocus: true, visible: false })).toBeNull();
  });

  it("still matches a key its own table does not know", () => {
    expect(shortcutFor(key({ key: "q" }), context(), focused)).toBeNull();
  });
});

// handleKeydown is the full dispatch: the modal chords first, then the gate, and
// preventDefault only for a key it consumed. A plain event object with a spy
// preventDefault drives it without a DOM, and a recording context proves which
// callback fired.
function press(over: Partial<ShortcutGateEvent>): {
  e: KeydownEventLike;
  prevented: () => boolean;
} {
  let prevented = false;
  const e: KeydownEventLike = {
    ...key(over),
    preventDefault: () => {
      prevented = true;
    },
  };

  return { e, prevented: () => prevented };
}

interface Calls {
  ctx: ShortcutContext;
  settings: number;
  palettes: { source: string; preview: boolean }[];
  closes: string[];
  actions: string[];
}

function recording(over: Partial<ShortcutContext> = {}): Calls {
  const calls: Calls = {
    settings: 0,
    palettes: [],
    closes: [],
    actions: [],
    ctx: undefined as unknown as ShortcutContext,
  };
  calls.ctx = {
    isModalOpen: () => false,
    isPaletteOpen: () => false,
    isEditableFocused: () => false,
    openSettings: () => {
      calls.settings += 1;
    },
    openPalette: (source, preview) => {
      calls.palettes.push({ source, preview });
    },
    closePaletteOrSwitch: (source) => {
      calls.closes.push(source);
    },
    action: (a) => {
      calls.actions.push(a);
    },
    ...over,
  };

  return calls;
}

describe("handleKeydown dispatch", () => {
  it("dispatches a playback key and prevents the default", () => {
    const calls = recording();
    const { e, prevented } = press({ key: "n" });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(true);
    expect(calls.actions).toEqual(["next"]);
    expect(prevented()).toBe(true);
  });

  it("does nothing and does not prevent the default for an unbound key", () => {
    const calls = recording();
    const { e, prevented } = press({ key: "q" });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(false);
    expect(calls.actions).toEqual([]);
    expect(prevented()).toBe(false);
  });

  it("does not prevent the default when the gate blocks the key", () => {
    const calls = recording({ isModalOpen: () => true });
    const { e, prevented } = press({ key: "n" });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(false);
    expect(calls.actions).toEqual([]);
    expect(prevented()).toBe(false);
  });

  it("handles the modal chords before the playback gate", () => {
    // Ctrl+I is a chord, not a key the gate should see: the gate would ignore
    // it for the held modifier, but the chord must still open settings.
    const calls = recording({ isModalOpen: () => true });
    const { e, prevented } = press({ key: "i", ctrlKey: true });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(true);
    expect(calls.settings).toBe(1);
    expect(prevented()).toBe(true);
  });

  it("keeps a playback key inert while a modal is open", () => {
    const calls = recording({ isModalOpen: () => true });
    const { e, prevented } = press({ key: "s" });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(false);
    expect(calls.actions).toEqual([]);
    expect(prevented()).toBe(false);
  });

  it("opens the queue palette into preview on Ctrl+Alt+P", () => {
    const calls = recording();
    const { e } = press({ key: "p", ctrlKey: true, altKey: true });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(true);
    expect(calls.palettes).toEqual([{ source: "queue", preview: true }]);
  });

  it("leaves Ctrl+Alt+P to the palette when it is already open", () => {
    const calls = recording({ isPaletteOpen: () => true });
    const { e, prevented } = press({ key: "p", ctrlKey: true, altKey: true });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(true);
    // The palette owns this chord and toggles its own preview; the app must not
    // reopen it.
    expect(calls.palettes).toEqual([]);
    expect(prevented()).toBe(true);
  });

  it("opens the YouTube palette on Ctrl+F", () => {
    const calls = recording();
    const { e } = press({ key: "f", ctrlKey: true });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(true);
    expect(calls.palettes).toEqual([{ source: "ytm", preview: false }]);
  });

  it("routes Ctrl+P through close-or-switch", () => {
    const calls = recording();
    const { e } = press({ key: "p", ctrlKey: true });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(true);
    expect(calls.closes).toEqual(["queue"]);
    expect(calls.palettes).toEqual([]);
  });

  it("treats Command as the same chord as Control", () => {
    const calls = recording();
    const { e } = press({ key: "i", metaKey: true });

    expect(handleKeydown(e, calls.ctx, focused)).toBe(true);
    expect(calls.settings).toBe(1);
  });
});
