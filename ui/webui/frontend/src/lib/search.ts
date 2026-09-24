// search.ts holds the quick-open matching. It is pure so the palette's ranking
// is unit-testable without a DOM, and so the same rule can back a future list
// view. The queue rows carry resolved tags, so matching never crosses the
// bridge per keystroke.

import type { QueueRow } from "../../bindings/github.com/dlcuy22/player/ui/webui/models";
import { baseName } from "./format";

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
