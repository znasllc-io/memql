import { rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

import { runQuery } from "../integrations/calls";
import { fetchSiteAsOf } from "./calls";

// The rollback picker's version list (memql#3717, ruling 2). "The graph's
// own history is the version list" (D10) -- there is no one-query
// all-versions read (D10 / #2880), so this module IS the walk: siteById
// under successive `asOf` timestamps, each one just before the previous
// result's createdAt.

export interface SiteVersion {
  bundleRef: string;
  status: string;
  createdAt: string;
}

// MAX_HISTORY_VERSIONS bounds the walk to "the last handful of versions"
// (ruling 2). Each additional version is a full round trip, so this is a
// latency choice as much as a display one: enough to undo a bad publish
// without turning the picker into a slow-loading timeline.
export const MAX_HISTORY_VERSIONS = 5;

function toVersion(row: Row): SiteVersion {
  return {
    bundleRef: rowString(row, "bundleRef"),
    status: rowString(row, "status"),
    createdAt: rowString(row, "createdAt"),
  };
}

// justBefore backs an RFC3339 instant off by one millisecond, so re-issuing
// siteById `asOf` that instant scans strictly BEFORE the row it came from
// instead of returning the same row again. Millisecond resolution -- the
// precision Date/toISOString actually carry -- which is coarser than
// Postgres's own timestamp column; two versions of the same site published
// within the same millisecond would collide. That is not a real risk for a
// human clicking Publish, and no version anywhere in this DSL tree relies on
// sub-millisecond ordering. "" (unparseable) propagates as "" so the walk
// stops rather than loops.
export function justBefore(iso: string): string {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) return "";
  return new Date(ms - 1).toISOString();
}

// fetchSiteVersionHistory walks a site's row history newest-first, using
// ONLY the query surface -- no builtin, no timeline mode, because none
// exists (D10 / #2880; the prior art for a one-query timeline read would be
// a builtin, and none has been built). The current row comes from a plain
// siteById call; every version after it re-issues siteById wrapped in
// asOf() at a timestamp strictly before the previous result's createdAt, so
// each round trip reveals exactly the next-older row.
//
// Stops at `limit` versions, at the first asOf call that returns nothing
// (the walk reached the row's creation, or ran past it), or the first
// result whose createdAt does not strictly decrease (a defensive stop
// against turning a clock anomaly into an infinite loop -- it should never
// fire against a real engine, since createdAt is server-stamped and
// monotonic per id).
export async function fetchSiteVersionHistory(
  query: QueryClient,
  siteId: string,
  limit: number = MAX_HISTORY_VERSIONS,
): Promise<SiteVersion[]> {
  const versions: SiteVersion[] = [];
  if (siteId === "" || limit <= 0) return versions;

  const current = await runQuery(query, "siteById", { siteId });
  const first = current[0];
  if (first === undefined) return versions;
  versions.push(toVersion(first));
  let cursor = rowString(first, "createdAt");

  while (versions.length < limit && cursor !== "") {
    const at = justBefore(cursor);
    if (at === "") break;
    const rows = await fetchSiteAsOf(query, siteId, at);
    const row = rows[0];
    if (row === undefined) break;
    const createdAt = rowString(row, "createdAt");
    if (createdAt === "" || createdAt >= cursor) break;
    versions.push(toVersion(row));
    cursor = createdAt;
  }

  return versions;
}
