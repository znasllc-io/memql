import {
  newShortId,
  renderMemQLValue,
  type QueryClient,
  type Result,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";
import type { Arrangement } from "@znasllc-io/memql-view-kit";

import { parseSavedView, type SavedView } from "../compose/savedViews";

// A page's VERSIONS: the override row, its history, and the walk (epic
// memql#4661, task memql#4668).
//
// ===========================================================================
// RESOLUTION ORDER, AND THE THING THAT IS NOT IN IT
// ===========================================================================
//
//   the caller's active override row for this page id
//     -> else the page's seed (the manifest)
//        -> and NEVER a model.
//
// The third line is the one worth stating. AI runs on ONE explicit action --
// the regenerate button -- and never at render (spec D3). A render path that
// asked a provider what a page should look like would cost money on every
// page view, change a page under somebody mid-read, and make the console
// unusable on a cluster with no provider. So the only thing that can produce
// a new arrangement is a write, and the only thing render does is read.
//
// ===========================================================================
// PER-USER, AND WHY THAT IS ONE CONJUNCT RATHER THAN A FEATURE
// ===========================================================================
// A regeneration is stored as a row the caller OWNS (spec D4). The engine's
// row-authz tier on v1:portalviews:view is `owner="ownerUserId"`, the query
// states `ownerUserId==actor.userId`, and the mutation stamps the field from
// actor.userId with @serverSet -- so there is no argument through which one
// person could write, or read, another person's arrangement of a page. "A
// regeneration never repaints somebody else's console" is that, and nothing
// else is required to be true for it to hold.
//
// ===========================================================================
// THE VERSION LIST IS THE ROW'S OWN HISTORY
// ===========================================================================
// A write in MemQL is an append onto one id, so a plain read of the override
// returns its NEWEST version and re-issuing that read under successive `asOf`
// timestamps walks back one version at a time. That is the deployables
// pattern (D10 / memql#2880) and it is why there is no parallel version table:
// there does not need to be one.
//
// ORIGINAL IS THE SEED AND HAS NO ROW. It is always present, always
// available, and costs nothing to keep -- which is what makes "nothing is ever
// destroyed" true rather than aspirational.

export interface PageVersion {
  // 0 is Original -- the seed. 1..n are the stored versions, oldest first, so
  // the numbering matches what a person reads: v1 was the first regeneration.
  readonly version: number;
  readonly label: string;
  // The arrangements this version renders. Empty for Original, whose
  // arrangements come from the manifest and are not carried here -- a copy of
  // the seed in this list would be a second place for it to drift from.
  readonly arrangements: readonly Arrangement[];
  // RFC3339. Empty for Original, which has no moment: it is what the page has
  // always looked like.
  readonly createdAt: string;
}

export const ORIGINAL_VERSION: PageVersion = {
  version: 0,
  label: "Original",
  arrangements: [],
  createdAt: "",
};

// MAX_VERSIONS bounds the walk. Each version back is a full round trip, so
// this is a latency choice as much as a display one: enough to undo a
// regeneration that went wrong without turning a page header into a timeline.
export const MAX_VERSIONS = 8;

// fetchPageOverride reads the newest version of a page's override, or nothing.
//
// NOTHING IS THE ANSWER, not an empty state: a person who has never
// regenerated this page has no row, and the caller renders the seed.
export async function fetchPageOverride(
  query: QueryClient,
  pageId: string,
): Promise<SavedView | undefined> {
  const result = await callPageOverride(query, pageId, "");
  return firstView(result);
}

// fetchPageVersions walks the override's history, newest first, and appends
// Original.
//
// The walk re-issues the SAME query under `asOf` set just before each
// result's createdAt, because there is no all-versions read and no builtin
// that would be the prior art for one. It stops on the first empty reply, on
// an unparseable timestamp, or at MAX_VERSIONS -- all three being "there is
// nothing more to show" rather than errors.
export async function fetchPageVersions(
  query: QueryClient,
  pageId: string,
): Promise<readonly PageVersion[]> {
  const rows: SavedView[] = [];
  let at = "";
  for (let i = 0; i < MAX_VERSIONS; i += 1) {
    const view = firstView(await callPageOverride(query, pageId, at));
    if (view === undefined) break;
    rows.push(view);
    at = justBefore(view.createdAt);
    if (at === "") break;
  }

  // Numbered from the OLDEST stored version, so v1 is the first regeneration
  // somebody made rather than the most recent -- which is how a person counts
  // them and the opposite of the order they arrive in.
  const oldestFirst = [...rows].reverse();
  const versions: PageVersion[] = oldestFirst.map((view, index) => ({
    version: index + 1,
    label: `v${index + 1}`,
    arrangements: view.arrangements,
    createdAt: view.createdAt,
  }));

  return [ORIGINAL_VERSION, ...versions];
}

// callPageOverride issues the read, optionally wrapped in `asOf`.
//
// A RAW CALL rather than the generated typed method, for the reason
// deployables/calls.ts states at length: `asOf(<call>, "<instant>")` parses
// its first argument as an EXPRESSION, and the generated builders always
// prepend the `query` keyword, which is a top-level dispatch keyword with no
// place inside one. Quoting still goes through renderMemQLValue -- the one
// rule every call-building path in this portal is held to.
async function callPageOverride(
  query: QueryClient,
  pageId: string,
  at: string,
): Promise<Result> {
  const call = `pageOverride(targetPageId: ${renderMemQLValue(pageId)})`;
  if (at === "") {
    return query.executeNamed("pageOverride", `query ${call}`);
  }
  return query.executeNamed(
    "pageOverride (asOf)",
    `asOf(${call}, ${renderMemQLValue(at)})`,
  );
}

function firstView(result: Result): SavedView | undefined {
  const rows = result.rows();
  const first = rows[0];
  return first === undefined ? undefined : parseSavedView(first as Row);
}

// justBefore backs an instant off by one millisecond so re-issuing the read
// `asOf` it scans strictly BEFORE the row it came from rather than returning
// the same row again.
//
// Millisecond resolution -- the precision Date carries -- which is coarser
// than Postgres's own timestamp column. Two regenerations of one page inside
// the same millisecond would collide, which is not a risk for a person
// pressing a button. "" propagates as "" so the walk stops rather than loops.
export function justBefore(iso: string): string {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) return "";
  return new Date(ms - 1).toISOString();
}

// newOverrideId mints the row id for a page's FIRST regeneration.
//
// Minted client-side like every other MemQL write, so a create is idempotent
// under a retry. Subsequent regenerations reuse the existing row's id -- a
// write is an append onto it, which is what makes the history a history
// rather than a pile of unrelated rows.
export function newOverrideId(): string {
  return newShortId();
}
