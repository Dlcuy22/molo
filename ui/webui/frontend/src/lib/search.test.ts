import { describe, expect, it } from "vitest";
import { hasTags, isPlaylistKind, matchQueue, playlistIDFromQuery, searchText, tokenize, ytmKindLabel, ytmPlaylistResult, ytmResultRow, ytmSubtitle } from "./search";
import type { QueueRow } from "../../bindings/github.com/dlcuy22/molo/ui/webui/models";
import type { YTMResult } from "../../bindings/github.com/dlcuy22/molo/ui/webui/models";

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

  it("does not offer a remote reference's identifier as a file name", () => {
    // A provider ref has no file name; "ytm:abc123" must never be matched as
    // if it were a title.
    const r = row({ index: 0, path: "ytm:abc123" });
    expect(searchText(r)).toBe("");
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

// hit builds a YouTube Music search result with only the fields the helpers
// read.
function hit(partial: Partial<YTMResult>): YTMResult {
  return {
    videoId: "",
    title: "",
    artist: "",
    album: "",
    durationMs: 0,
    kind: "Song",
    explicit: false,
    thumbnail: "",
    artistId: "",
    playable: true,
    ...partial,
  };
}

describe("ytmResultRow", () => {
  it("carries the provider reference for a playable song", () => {
    const r = ytmResultRow(hit({ videoId: "abc123", title: "A Song" }), 3);
    expect(r.path).toBe("ytm:abc123");
    expect(r.title).toBe("A Song");
    expect(r.coverId).toBe("ytm:abc123");
  });

  it("carries no reference for a hit that cannot be played", () => {
    const r = ytmResultRow(hit({ videoId: "MPREalbum", title: "An Album", kind: "Album", playable: false }), 0);
    expect(r.path).toBe("");
    expect(r.coverId).toBe("");
  });

  it("treats an empty id as not playable", () => {
    const r = ytmResultRow(hit({ title: "Nameless", playable: true }), 0);
    expect(r.path).toBe("");
  });
});

describe("ytmSubtitle", () => {
  it("joins the credits when there are any", () => {
    expect(ytmSubtitle(hit({ artist: "A", album: "B" }))).toBe("A · B");
  });

  it("falls back to the kind for a hit with no credits", () => {
    expect(ytmSubtitle(hit({ kind: "Album" }))).toBe("Album");
  });

  it("leaves a song with no credits blank", () => {
    expect(ytmSubtitle(hit({ kind: "Song" }))).toBe("");
  });
});

describe("ytmKindLabel", () => {
  it("labels everything but a plain song", () => {
    expect(ytmKindLabel(hit({ kind: "Song" }))).toBe("");
    expect(ytmKindLabel(hit({ kind: "Video" }))).toBe("Video");
    expect(ytmKindLabel(hit({ kind: "Artist" }))).toBe("Artist");
  });
});

describe("isPlaylistKind", () => {
  it("is true for playlists and albums only", () => {
    expect(isPlaylistKind(hit({ kind: "Playlist" }))).toBe(true);
    expect(isPlaylistKind(hit({ kind: "Album" }))).toBe(true);
    expect(isPlaylistKind(hit({ kind: "Song" }))).toBe(false);
    expect(isPlaylistKind(hit({ kind: "Artist" }))).toBe(false);
  });
});

describe("playlistIDFromQuery", () => {
  it("reads the id out of a share URL", () => {
    expect(
      playlistIDFromQuery("https://music.youtube.com/playlist?list=PLdirQjL5RyWE&si=DyvjQqw8h-cm07fx"),
    ).toBe("PLdirQjL5RyWE");
  });

  it("accepts a bare id with a known prefix", () => {
    expect(playlistIDFromQuery("PLdirQjL5RyWE")).toBe("PLdirQjL5RyWE");
    expect(playlistIDFromQuery("VLPLabc")).toBe("VLPLabc");
    expect(playlistIDFromQuery("MPREb_album")).toBe("MPREb_album");
  });

  it("does not mistake an ordinary query for a playlist", () => {
    expect(playlistIDFromQuery("kessoku band")).toBe("");
    expect(playlistIDFromQuery("")).toBe("");
  });
});

describe("ytmPlaylistResult", () => {
  it("marks the pasted playlist as an add action, not a track", () => {
    const r = ytmPlaylistResult("PLabc");
    expect(r.videoId).toBe("PLabc");
    expect(r.kind).toBe("Playlist");
    expect(r.playable).toBe(false);
    // It carries no track reference, so it never renders as playable art.
    expect(ytmResultRow(r, 0).path).toBe("");
  });
});
