import { renderMemQLValue, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

// The one raw-call form this feature needs beyond runQuery/runMutation
// (integrations/calls.ts, reused as-is for every ordinary named call): the
// asOf(...) wrapper the rollback picker walks a site's history with.
//
// WHY THIS IS NOT A NAMED CALL. siteById deliberately declares no `asOf`
// clause of its own (dsl/platform/queries.memql, memql#3717 / D10 / #2880):
// a query that declares `asOf args.asOf ?? latest` refuses to be wrapped a
// second time ("multiple asOf() directives are not supported"), which is
// exactly the caller-chosen-point-in-time capability the rollback picker
// needs. The wrap lives at the CALL SITE instead --
// `asOf(<call>, "<rfc3339>")` -- proven against a live engine in
// component/memql/asof_reconstructability_1872_db_test.go and used the same
// way by component/deploycontrol/deploy.go's loadDeployment. Both send the
// BARE call form (no `query` keyword) as the wrapper's first argument --
// `asOf(deploymentById(deploymentId:"x"), "...")` -- because that argument
// is parsed as an EXPRESSION, and `query` is a top-level dispatch keyword
// with no place inside one. This mirrors that exact shape rather than
// integrations/calls.ts's buildCall, which always prepends
// `query `/`mutation `/`builtin `.
//
// Quoting still goes through renderMemQLValue, never hand-interpolated --
// the one rule every call-building path in this portal is held to.
export async function fetchSiteAsOf(query: QueryClient, siteId: string, at: string): Promise<Row[]> {
  const call = `asOf(siteById(siteId: ${renderMemQLValue(siteId)}), ${renderMemQLValue(at)})`;
  // The `name` passed to executeNamed is only ever used to prefix an error
  // message (sdk/ts/src/client/query.ts) -- it is never sent on the wire --
  // so a descriptive non-identifier string is fine here.
  const result = await query.executeNamed("siteById (asOf)", call);
  return result.rows();
}
