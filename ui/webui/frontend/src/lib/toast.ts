// toast.ts: the one-at-a-time status card for keyboard actions.
//
// A shortcut has no visible control to light up, so the toast is the only
// confirmation that a key did anything. The store is a single slot: a new toast
// always overwrites the current one, whether or not it shares a key, so there is
// no queue and no stacking. A held volume key shows one bar that keeps updating,
// not twenty. The key exists only to tag a toast for its expiry timer, so a
// timer that fires late cannot clear a newer toast it does not own.
//
// The timers are plain setTimeout, with no DOM, so the store can be driven by
// vitest fake timers.

import { writable, type Readable } from "svelte/store";
import type { Component } from "svelte";

export type ToastKind = "bar" | "tile" | "art";

export interface Toast {
  /** Identifies the toast to its expiry timer. It does not select a slot: the
   *  store holds one toast, and any show overwrites it. */
  key: string;
  kind: ToastKind;
  label: string;
  /** The icon component itself, not a name, so the host keeps no name table. */
  icon?: Component;
  /** Fill fraction 0..1, for kind "bar" only. */
  value?: number;
  /** The percentage or title text, when a kind shows one. */
  text?: string;
}

/** TOAST_TTL is per kind: a bar is a glance, an art card carries more to read,
 *  so each kind gets as long as its content needs and no longer. */
export const TOAST_TTL: Record<ToastKind, number> = {
  bar: 900,
  tile: 1200,
  art: 1500,
};

const store = writable<Toast | null>(null);

/** toast is the single visible card, or null. */
export const toast: Readable<Toast | null> = { subscribe: store.subscribe };

let timer: ReturnType<typeof setTimeout> | null = null;

/** showToast displays a toast, replacing whatever is showing. The previous
 *  timer is cleared so only the newest card counts down, and the callback
 *  re-checks the key so a late timer cannot clear a newer card. */
export function showToast(t: Toast): void {
  store.set(t);
  if (timer !== null) {
    clearTimeout(timer);
  }
  const key = t.key;
  timer = setTimeout(() => {
    timer = null;
    store.update((cur) => (cur && cur.key === key ? null : cur));
  }, TOAST_TTL[t.kind]);
}
