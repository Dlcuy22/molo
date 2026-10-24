import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { get } from "svelte/store";
import { showToast, TOAST_TTL, toast, type ToastKind } from "./toast";

// The toast store owns its timer with plain setTimeout, so vitest fake timers
// drive the TTL without a DOM.

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

// slotCount is 0 or 1: the store is a single slot, never more.
function slotCount(): number {
  return get(toast) === null ? 0 : 1;
}

describe("toast single slot", () => {
  it("keeps one toast when the same key is shown again", () => {
    showToast({ key: "volume", kind: "bar", label: "Volume", value: 0.2, text: "20%" });
    showToast({ key: "volume", kind: "bar", label: "Volume", value: 0.4, text: "40%" });
    showToast({ key: "volume", kind: "bar", label: "Volume", value: 0.6, text: "60%" });

    expect(slotCount()).toBe(1);
    expect(get(toast)?.text).toBe("60%");
  });

  it("holds only the newest toast, whatever its key", () => {
    // The store does not distinguish keys for storage: a second show always
    // overwrites the slot, so the newest toast is the only one present and
    // nothing is queued behind it.
    showToast({ key: "volume", kind: "bar", label: "Volume", value: 0.4, text: "40%" });
    showToast({ key: "mute", kind: "tile", label: "Muted" });

    expect(slotCount()).toBe(1);
    expect(get(toast)?.key).toBe("mute");
    expect(get(toast)?.kind).toBe("tile");
  });
});

describe("toast expiry", () => {
  const cases: [ToastKind, number][] = [
    ["bar", TOAST_TTL.bar],
    ["tile", TOAST_TTL.tile],
    ["art", TOAST_TTL.art],
  ];

  for (const [kind, ttl] of cases) {
    it(`expires a ${kind} on its own TTL`, () => {
      showToast({ key: kind, kind, label: kind });
      expect(get(toast)?.key).toBe(kind);

      vi.advanceTimersByTime(ttl - 1);
      expect(get(toast)?.key).toBe(kind);

      vi.advanceTimersByTime(1);
      expect(get(toast)).toBeNull();
    });
  }

  it("resets the timer when the same key is shown again", () => {
    showToast({ key: "volume", kind: "bar", label: "Volume", value: 0.2, text: "20%" });
    vi.advanceTimersByTime(TOAST_TTL.bar - 1);
    // A second press under the TTL replaces the toast and restarts the clock.
    showToast({ key: "volume", kind: "bar", label: "Volume", value: 0.4, text: "40%" });

    vi.advanceTimersByTime(TOAST_TTL.bar - 1);
    expect(get(toast)?.text).toBe("40%");

    vi.advanceTimersByTime(1);
    expect(get(toast)).toBeNull();
  });
});
