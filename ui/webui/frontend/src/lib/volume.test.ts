import { beforeEach, describe, expect, it, vi } from "vitest";
import { get } from "svelte/store";
import {
  initVolume,
  muted,
  syncVolumeFromServer,
  toggleMute,
  volume,
  volumeBy,
  volumeOverride,
} from "./volume";

// volume.ts imports nothing from the store, so a fake engine call is injected
// and every assertion reads the module state directly.

const calls: number[] = [];

beforeEach(() => {
  calls.length = 0;
  initVolume((v) => calls.push(v));
  muted.set(false);
  volumeOverride.set(null);
});

describe("volumeBy", () => {
  it("steps by the delta and leads the engine", () => {
    syncVolumeFromServer(0.5);
    volumeBy(0.05);
    expect(volume()).toBeCloseTo(0.55, 10);
    expect(calls.at(-1)).toBeCloseTo(0.55, 10);
  });

  it("clamps at the top and still fires the engine call", () => {
    syncVolumeFromServer(0.98);
    volumeBy(0.05);
    expect(volume()).toBe(1);
    expect(calls.at(-1)).toBe(1);
  });

  it("clamps at the bottom and still fires the engine call", () => {
    syncVolumeFromServer(0.02);
    volumeBy(-0.05);
    expect(volume()).toBe(0);
    expect(calls.at(-1)).toBe(0);
  });

  it("unmutes when the volume rises from a mute", () => {
    syncVolumeFromServer(0.5);
    toggleMute();
    expect(get(muted)).toBe(true);

    volumeBy(0.05);
    expect(get(muted)).toBe(false);
  });
});

describe("toggleMute", () => {
  it("remembers the level and restores it on unmute", () => {
    syncVolumeFromServer(0.7);
    toggleMute();
    expect(get(muted)).toBe(true);
    expect(calls.at(-1)).toBe(0);

    toggleMute();
    expect(get(muted)).toBe(false);
    expect(calls.at(-1)).toBe(0.7);
  });

  it("never restores to 0", () => {
    syncVolumeFromServer(0);
    toggleMute();
    toggleMute();
    expect(calls.at(-1)).toBe(1);
  });
});

describe("syncVolumeFromServer", () => {
  it("drops the override when the server value changes", () => {
    syncVolumeFromServer(0.5);
    volumeBy(0.05);
    expect(get(volumeOverride)).toBeCloseTo(0.55, 10);

    // The engine confirms the new value.
    syncVolumeFromServer(0.55);
    expect(get(volumeOverride)).toBeNull();
    expect(volume()).toBe(0.55);
  });

  it("keeps the override while the server value is unchanged", () => {
    syncVolumeFromServer(0.5);
    volumeBy(0.05);
    syncVolumeFromServer(0.5);
    expect(get(volumeOverride)).toBeCloseTo(0.55, 10);
  });

  it("does not set a spy expectation failure on a stale engine call", () => {
    // initVolume is the only seam; a fresh injection replaces the fake cleanly.
    const local = vi.fn();
    initVolume(local);
    syncVolumeFromServer(0.3);
    volumeBy(0.05);
    expect(local).toHaveBeenCalledWith(0.35);
  });
});
