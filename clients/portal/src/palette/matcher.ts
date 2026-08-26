// The palette's ranking.
//
// ===========================================================================
// WHY RANK AT ALL
// ===========================================================================
// A palette over a hundred-odd entries where "us" matches `Users`,
// `Sessions and tokens` and `v1:cluster:node` equally is a list you have to
// read -- which is the thing the palette exists to avoid. What makes it feel
// instant is that the entry you meant is FIRST, and that is a ranking
// question, not a matching one.
//
// FOUR TIERS, strongest first, and the order is the whole design:
//
//   prefix         you typed the start of the thing        ("us" -> Users)
//   word start     you typed the start of a WORD in it     ("tok" -> Sessions
//                                                           and tokens)
//   substring      you typed something inside it           ("rig" -> origins)
//   subsequence    your letters appear in order, spread    ("dor" -> Data
//                                                           origins)
//
// Subsequence is last and deliberately weak. It is what makes "cs" find
// "Concepts", and it is also what would let almost anything match almost
// anything if it outranked a real prefix hit.
//
// Ties break on the SHORTER text, because a query that is most of a short
// label is a better guess than the same query inside a long one.

export const NO_MATCH = -1;

export function scoreMatch(query: string, text: string): number {
  const q = query.trim().toLowerCase();
  if (q === "") return 0;
  const t = text.toLowerCase();

  // Length is folded in as a small penalty rather than a tier of its own, so
  // it only decides ties within a tier and never promotes across one.
  const brevity = Math.max(0, 40 - t.length);

  if (t.startsWith(q)) return 4000 + brevity;

  // A word start: the character before it is a separator. Cheaper and more
  // predictable than splitting -- and it treats ":" as a separator, which is
  // what makes "node" find v1:cluster:node.
  for (let i = 1; i < t.length; i += 1) {
    if (!isBoundary(t[i - 1] ?? "")) continue;
    if (t.startsWith(q, i)) return 3000 + brevity - i;
  }

  const at = t.indexOf(q);
  if (at !== -1) return 2000 + brevity - at;

  const span = subsequenceSpan(q, t);
  if (span === NO_MATCH) return NO_MATCH;
  // Tighter runs rank higher: "dor" over "Data origins" beats the same
  // letters scattered across a paragraph.
  return 1000 + brevity - Math.min(span, 60);
}

function isBoundary(ch: string): boolean {
  return !/[a-z0-9]/.test(ch);
}

// The number of characters between the first and last matched letter, or
// NO_MATCH when the letters do not appear in order at all.
function subsequenceSpan(q: string, t: string): number {
  let first = -1;
  let cursor = 0;
  for (const ch of q) {
    const found = t.indexOf(ch, cursor);
    if (found === NO_MATCH) return NO_MATCH;
    if (first === -1) first = found;
    cursor = found + 1;
  }
  return cursor - first;
}

export interface Rankable {
  readonly label: string;
  // Extra text a query may match -- a path, a concept's domain. Scored at a
  // discount so a hit in the LABEL always wins: what a person types is
  // overwhelmingly the name of the thing.
  readonly hint?: string;
}

export function rank<T extends Rankable>(query: string, items: readonly T[]): T[] {
  const scored: { item: T; score: number }[] = [];
  for (const item of items) {
    const label = scoreMatch(query, item.label);
    const hint = item.hint === undefined ? NO_MATCH : scoreMatch(query, item.hint);
    const score = Math.max(label, hint === NO_MATCH ? NO_MATCH : hint - 1500);
    if (score !== NO_MATCH) scored.push({ item, score });
  }
  // A STABLE sort over the source order, so an empty query leaves the entries
  // in the order the sources built them -- destinations, then tabs, then
  // views. A palette that reshuffled its own list on every keystroke would be
  // unusable at the exact moment it is most useful.
  scored.sort((a, b) => b.score - a.score);
  return scored.map((entry) => entry.item);
}
