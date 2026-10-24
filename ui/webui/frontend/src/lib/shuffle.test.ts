import { describe, expect, it } from "vitest";
import { nextShuffleTarget } from "./shuffle";

describe("nextShuffleTarget", () => {
  it("turns shuffle on from a server value of false", () => {
    expect(nextShuffleTarget(null, false)).toBe(true);
  });

  it("turns shuffle off from a server value of true", () => {
    expect(nextShuffleTarget(null, true)).toBe(false);
  });

  it("flips the pending intent, so a fast double press lands on then off", () => {
    // The server has not moved yet: both presses see shrunk state where the
    // snapshot still says false. The intent is what lets the second one flip.
    const server = false;
    const first = nextShuffleTarget(null, server);
    expect(first).toBe(true);

    const second = nextShuffleTarget(first, server);
    expect(second).toBe(false);
  });

  it("keeps flipping while the intent is ahead of the server", () => {
    let intent: boolean | null = null;
    const server = true;
    const seq: boolean[] = [];
    for (let i = 0; i < 3; i++) {
      intent = nextShuffleTarget(intent, server);
      seq.push(intent);
    }
    expect(seq).toEqual([false, true, false]);
  });
});
