// search.ts holds the quick-open matching. It is pure so the palette's ranking
// is unit-testable without a DOM, and so the same rule can back a future list
// view. The queue rows carry resolved tags, so matching never crosses the
// bridge per keystroke.

import type { QueueRow } from "../../bindings/github.com/dlcuy22/molo/ui/webui/models";
import type { YTMResult } from "../../bindings/github.com/dlcuy22/molo/ui/webui/models";
import { baseName, subtitle } from "./format";

/**
 * searchText is what a row is matched against. Tags are included when the
 * track has them, and the file name is always included too, so a tagged track
 * is findable by either its metadata or its file name. A track with no tags
 * still matches on its name alone.
 */
export function searchText(row: QueueRow): string {
  const parts = [row.title, row.artist, row.album].filter((s) => s.trim() !== "");
  parts.push(row.name || baseName(row.path));

  return parts.join(" ").toLowerCase();
}

/** tokenize splits a query on whitespace and lowercases it, dropping empties so
 *  a stray space does not match nothing. */
export function tokenize(query: string): string[] {
  return query
    .toLowerCase()
    .split(/\s+/)
    .filter((t) => t !== "");
}

/** hasTags reports whether a row carries any embedded tag to search or show. */
export function hasTags(row: QueueRow): boolean {
  return (
    row.title.trim() !== "" || row.artist.trim() !== "" || row.album.trim() !== ""
  );
}

/**
 * ytmResultRow projects a YouTube Music search result onto the row shape the
 * palette already draws, so a remote hit and a queued track share one renderer
 * and one keyboard model. The reference is the provider scheme the engine
 * resolves, which is also what identifies the row for its artwork.
 *
 * A non-playable hit (an album, artist or playlist) carries no reference,
 * because selecting it cannot play anything and pretending otherwise would
 * offer a control that does nothing.
 */
export function ytmResultRow(result: YTMResult, index: number): QueueRow {
  const playable = result.playable && result.videoId !== "";

  return {
    index,
    path: playable ? `ytm:${result.videoId}` : "",
    name: result.title,
    title: result.title,
    artist: result.artist,
    album: result.album,
    coverId: playable ? `ytm:${result.videoId}` : "",
    active: false,
  } as QueueRow;
}

/** ytmSubtitle is the row's second line: the credits, then the kind for a hit
 *  that is not a song. A song with no credited artist still says what it is
 *  rather than leaving the line blank. */
export function ytmSubtitle(result: YTMResult): string {
  const credits = subtitle(result.artist, result.album);
  if (credits !== "") {
    return credits;
  }
  if (result.kind !== "" && result.kind !== "Song") {
    return result.kind;
  }

  return "";
}

/** ytmKindLabel names the kind of a hit the way a listener reads it: a plain
 *  "Song" needs no label, anything else does. */
export function ytmKindLabel(result: YTMResult): string {
  return result.kind === "Song" ? "" : result.kind;
}

/** isPlaylistKind reports whether a hit is a playlist or album, which the
 *  palette can add to the queue as a whole rather than play as one track. */
export function isPlaylistKind(result: YTMResult): boolean {
  return result.kind === "Playlist" || result.kind === "Album";
}

/**
 * playlistIDFromQuery reads a YouTube Music playlist out of what the user typed.
 * A share URL carries the id as its list= parameter; a bare id is accepted only
 * with a known prefix, so an ordinary search word is never mistaken for one.
 * It returns "" when the text is a normal query.
 */
export function playlistIDFromQuery(query: string): string {
  const q = query.trim();
  if (q === "") {
    return "";
  }
  const fromURL = q.match(/[?&]list=([A-Za-z0-9_-]+)/);
  if (fromURL) {
    return fromURL[1];
  }
  if (/^(VL|MPREb_|PL|RD|OLAK5uy_)[A-Za-z0-9_-]+$/.test(q)) {
    return q;
  }

  return "";
}

/**
 * ytmPlaylistResult builds the hit that represents adding a whole playlist,
 * either one the user pasted as a URL or a playlist row from a search. It is
 * marked not playable because it is not a track; the palette acts on its kind
 * instead.
 */
export function ytmPlaylistResult(id: string, title = "Add YouTube Music playlist"): YTMResult {
  return {
    videoId: id,
    title,
    artist: "",
    album: "",
    durationMs: 0,
    kind: "Playlist",
    explicit: false,
    thumbnail: "",
    artistId: "",
    playable: false,
  };
}

/**
 * matchQueue returns the rows matching every token of the query, in a useful
 * order. An empty query returns the whole queue untouched, so opening the
 * palette lists what is there. Ranking is deliberately shallow: a row whose
 * text starts with the query sorts above one that merely contains it, and
 * ties keep queue order.
 */
export function matchQueue(rows: QueueRow[], query: string): QueueRow[] {
  const tokens = tokenize(query);
  if (tokens.length === 0) {
    return rows;
  }

  const scored: { row: QueueRow; score: number; order: number }[] = [];
  for (let i = 0; i < rows.length; i++) {
    const text = searchText(rows[i]);
    if (!tokens.every((t) => text.includes(t))) {
      continue;
    }
    scored.push({ row: rows[i], score: rank(text, query.toLowerCase().trim()), order: i });
  }

  scored.sort((a, b) => (a.score !== b.score ? a.score - b.score : a.order - b.order));

  return scored.map((s) => s.row);
}

/**
 * rank scores one match: 0 when the text starts with the query, 1 when the
 * query starts at a word boundary, 2 otherwise. Lower is better.
 */
function rank(text: string, query: string): number {
  if (text.startsWith(query)) {
    return 0;
  }
  if (text.includes(" " + query) || text.includes("·" + query)) {
    return 1;
  }

  return 2;
}
