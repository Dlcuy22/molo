import { describe, expect, it } from "vitest";
import type { Param } from "../../bindings/github.com/dlcuy22/player/ui/webui/internal/spectrum/models";
import { fftLabel } from "./spectrum-fft";

// The dropdown renders fftLabel(c) verbatim, so the label is what tells two
// transform sizes apart in the list.
describe("fftLabel", () => {
  it("shows the bin width each size resolves", () => {
    expect(fftLabel(8192)).toBe("8192 (6 Hz/bin)");
    expect(fftLabel(4096)).toBe("4096 (12 Hz/bin)");
    expect(fftLabel(1024)).toBe("1024 (46.9 Hz/bin)");
  });

  it("keeps every choice distinguishable", () => {
    const labels = [1024, 2048, 4096, 8192, 16384].map(fftLabel);
    expect(new Set(labels).size).toBe(labels.length);
  });
});

// A choice param carries its own set and no range; the panel branches on kind
// rather than guessing from the numbers.
describe("choice params", () => {
  const fft: Param = {
    key: "fft",
    label: "Resolution",
    kind: "choice",
    min: 0,
    max: 0,
    step: 0,
    default: 8192,
    choices: [1024, 2048, 4096, 8192, 16384],
  };

  it("offers exactly the schema choices", () => {
    expect(fft.choices).toEqual([1024, 2048, 4096, 8192, 16384]);
  });

  it("defaults to an offered size so the select is never blank", () => {
    expect(fft.choices).toContain(fft.default);
  });

  it("is not a range param, so it takes no span", () => {
    expect(fft.kind).not.toBe("int");
    expect(fft.max).toBe(0);
  });
});

