// fftLabel names a transform size by what it resolves rather than by its sample
// count: two sizes differ only in bin width, so that is the number a reader
// needs to choose between them. Kept beside the store helpers so the dropdown
// and its test use one implementation.
export function fftLabel(size: number, sampleRate = 48000): string {
  const hzPerBin = sampleRate / size;
  // One decimal below 1 kHz, whole numbers above: finer detail than that does
  // not distinguish the entries a reader is choosing between.
  return `${size} (${hzPerBin.toFixed(size <= 1024 ? 1 : 0)} Hz/bin)`;
}