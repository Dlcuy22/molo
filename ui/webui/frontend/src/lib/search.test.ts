import { describe, expect, it } from "vitest";
import { hasTags, matchQueue, searchText, tokenize } from "./search";
import type { QueueRow } from "../../bindings/github.com/dlcuy22/player/ui/webui/models";

// row builds a QueueRow with only the fields the search reads.
function row(partial: Partial<QueueRow> & { index: number; path: string }): QueueRow {
  return {
    name: "",
    title: "",
    artist: "",
    album: "",
    active: false,
    ...partial,
  } as QueueRow;
}

describe("searchText", () => {
  it("includes the tags and the file name", () => {
    const r = row({
      index: 0,
      path: "/m/a.opus",
      name: "a.opus",
      title: "Karma",
      artist: "Kessoku Band",
      album: "The First",
    });
    expect(searchText(r)).toBe("karma kessoku band the first a.opus");
  });

  it("still uses the file name when a track has no tags", () => {
    const r = row({ index: 0, path: "/m/Me and the colors.opus", name: "Me and the colors.opus" });
    expect(searchText(r)).toBe("me and the colors.opus");
  });

  it("ignores a blank tag and still uses the others", () => {
    const r = row({ index: 0, path: "/m/a.opus", name: "a.opus", title: "  ", artist: "Solo" });
    expect(searchText(r)).toBe("solo a.opus");
  });
});

describe("tokenize", () => {
  it("splits on whitespace, lowercases, and drops empties", () => {
    expect(tokenize("  Kessoku   BAND ")).toEqual(["kessoku", "band"]);
  });

  it("returns nothing for a blank query", () => {
    expect(tokenize("   ")).toEqual([]);
  });
});

describe("hasTags", () => {
  it("is true only when some tag is present", () => {
    expect(hasTags(row({ index: 0, path: "/a.opus" }))).toBe(false);
    expect(hasTags(row({ index: 0, path: "/a.opus", album: "X" }))).toBe(true);
  });
});

describe("matchQueue", () => {
  const queue = [
    row({ index: 0, path: "/m/1.opus", name: "1.opus", title: "Karma", artist: "Kessoku Band" }),
    row({ index: 1, path: "/m/2.opus", name: "2.opus", title: "Never Forget", artist: "Kessoku Band" }),
    row({ index: 2, path: "/m/untagged song.opus", name: "untagged song.opus" }),
  ];

  it("returns the whole queue for an empty query", () => {
    expect(matchQueue(queue, "")).toEqual(queue);
  });

  it("matches on title, artist and album", () => {
    expect(matchQueue(queue, "karma").map((r) => r.index)).toEqual([0]);
    expect(matchQueue(queue, "kessoku").map((r) => r.index)).toEqual([0, 1]);
  });

  it("matches an untagged track by file name only", () => {
    expect(matchQueue(queue, "untagged").map((r) => r.index)).toEqual([2]);
  });

  it("matches a tagged track by its file name too", () => {
    // The filename is always a search param, even when tags exist.
    const rows = [
      row({ index: 0, path: "/m/Mimpi itu.flac", name: "Mimpi itu.flac", title: "あの夢をなぞって", artist: "YOASOBI" }),
    ];
    expect(matchQueue(rows, "mimpi").map((r) => r.index)).toEqual([0]);
    expect(matchQueue(rows, "yoasobi").map((r) => r.index)).toEqual([0]);
  });

  it("requires every token to match", () => {
    expect(matchQueue(queue, "kessoku karma").map((r) => r.index)).toEqual([0]);
    expect(matchQueue(queue, "kessoku untagged")).toEqual([]);
  });

  it("ranks a prefix match above a mid-string match", () => {
    const rows = [
      row({ index: 0, path: "/m/a.opus", name: "a.opus", title: "My Karma" }),
      row({ index: 1, path: "/m/b.opus", name: "b.opus", title: "Karma Police" }),
    ];
    expect(matchQueue(rows, "karma").map((r) => r.index)).toEqual([1, 0]);
  });

  it("keeps queue order for equal matches", () => {
    expect(matchQueue(queue, "band").map((r) => r.index)).toEqual([0, 1]);
  });
});
