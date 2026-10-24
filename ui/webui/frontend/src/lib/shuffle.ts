// shuffle.ts: the shuffle toggle's target value.
//
// Shuffle has no optimistic store of its own, and the snapshot's `shuffled`
// lags the round trip. Reading it directly makes two keypresses inside one
// snapshot window compute the same target, so a fast double press never turns
// shuffle back off. This keeps the pattern pure and testable: the caller holds
// the pending intent (null when none is in flight) and this decides the target.

/**
 * nextShuffleTarget returns the value the next shuffle press should send: the
 * opposite of the pending intent when one is in flight, otherwise the opposite
 * of the server value. The caller clears the intent once the snapshot agrees.
 */
export function nextShuffleTarget(intent: boolean | null, server: boolean): boolean {
  return !(intent ?? server);
}
