import { renderMemQLValue, rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

// THE ROLLBACK IS THE HISTORY WALK, so both halves live in one module.
//
// "The graph's own history is the version list" (site-hosting.md, D10 /
// memql#2880): there is no one-query all-versions read, and no builtin that
// would be the prior art for one. So this module IS the version list --
// siteById re-issued under successive `asOf` timestamps, each set just before
// the previous result's createdAt -- and each entry it returns is a bundleRef
// updateSiteBundle can be pointed back at. Splitting the raw call away from
// the walk would leave two files that only ever make sense together.
//
// ===========================================================================
// WHY THE asOf WRAP IS A RAW CALL AND NOT A GENERATED TYPED METHOD
// ===========================================================================
//
// siteById deliberately declares no `asOf` clause of its own
// (dsl/platform/queries.memql): a query that declares `asOf args.asOf ??
// latest` refuses to be wrapped a second time ("multiple asOf() directives are
// not supported"), which is exactly the caller-chosen-point-in-time capability
// this walk needs. So the wrap lives at the CALL SITE --
// `asOf(<call>, "<rfc3339>")` -- proven against a live engine in
// component/memql/asof_reconstructability_1872_db_test.go and used the same way
// by component/deploycontrol/deploy.go's loadDeployment. Both send the BARE
// call form (no `query` keyword) as the wrapper's first argument, because that
// argument is parsed as an EXPRESSION and `query` is a top-level dispatch
// keyword with no place inside one. That is why the generated builders cannot
// be used here: they always prepend the kind keyword.
//
// Quoting still goes through renderMemQLValue, never hand-interpolated -- the
// one rule every call-building path in this portal is held to.
export async function fetchSiteAsOf(query: QueryClient, siteId: string, at: string): Promise<Row[]> {
  const call = `asOf(siteById(siteId: ${renderMemQLValue(siteId)}), ${renderMemQLValue(at)})`;
  // The `name` passed to executeNamed is only ever used to prefix an error
  // message (sdk/ts/src/client/query.ts) -- it is never sent on the wire -- so
  // a descriptive non-identifier string is fine here.
  const result = await query.executeNamed("siteById (asOf)", call);
  return result.rows();
}

export interface SiteVersion {
  bundleRef: string;
  status: string;
  createdAt: string;
  // The Library artifact this version was published from, when it was. Empty
  // for a version that arrived through POST /sites/{id}/bundles or that names
  // a file:// path baked into the edge image.
  artifactId: string;
}

// MAX_HISTORY_VERSIONS bounds the walk to "the last handful of versions". Each
// additional version is a full round trip, so this is a latency choice as much
// as a display one: enough to undo a bad deploy without turning the picker into
// a slow-loading timeline.
export const MAX_HISTORY_VERSIONS = 5;

function toVersion(row: Row): SiteVersion {
  return {
    bundleRef: rowString(row, "bundleRef"),
    status: rowString(row, "status"),
    createdAt: rowString(row, "createdAt"),
    artifactId: rowString(row, "artifactId"),
  };
}

// justBefore backs an RFC3339 instant off by one millisecond, so re-issuing
// siteById `asOf` that instant scans strictly BEFORE the row it came from
// instead of returning the same row again. Millisecond resolution -- the
// precision Date/toISOString actually carry -- which is coarser than Postgres's
// own timestamp column; two versions of the same deployable published within
// the same millisecond would collide. That is not a real risk for a human
// clicking Deploy, and no version anywhere in this DSL tree relies on
// sub-millisecond ordering. "" (unparseable) propagates as "" so the walk stops
// rather than loops.
export function justBefore(iso: string): string {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) return "";
  return new Date(ms - 1).toISOString();
}

// fetchSiteVersionHistory walks a deployable's row history newest-first, using
// ONLY the query surface. The current row comes from a plain siteById call;
// every version after it re-issues siteById wrapped in asOf() at a timestamp
// strictly before the previous result's createdAt, so each round trip reveals
// exactly the next-older row.
//
// Stops at `limit` versions, at the first asOf call that returns nothing (the
// walk reached the row's creation, or ran past it), or the first result whose
// createdAt does not strictly decrease (a defensive stop against turning a
// clock anomaly into an infinite loop -- it should never fire against a real
// engine, since createdAt is server-stamped and monotonic per id).
export async function fetchSiteVersionHistory(
  query: QueryClient,
  siteId: string,
  limit: number = MAX_HISTORY_VERSIONS,
): Promise<SiteVersion[]> {
  const versions: SiteVersion[] = [];
  if (siteId === "" || limit <= 0) return versions;

  const current = await query.siteById({ siteId });
  const first = current.rows()[0];
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
