import { renderMemQLValue, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

// The wire vocabulary of the composer.
//
// A composed view is stored in `v1:portalviews:view`, read through the
// `composedViews` / `composedViewById` queries and written through the
// `createComposedView` / `updateComposedView` / `archiveComposedView`
// mutations (dsl/portalviews/). All five are NAMED CALLS dispatched through
// QueryClient.executeNamed -- the same seam the concept browser, the
// integrations surface and the generated SDKs ride. No new gRPC message was
// needed and none was added.
//
// WHY THE CALL STRINGS ARE BUILT HERE. `make sdk-gen` emits typed builders for
// Go only (the Makefile passes an empty --ts-out), so there is no generated
// TypeScript counterpart to import. The alternative is call strings scattered
// through the composer's components, which is worse in the one way that
// matters: argument quoting. Every value goes through the SDK's
// renderMemQLValue, which mirrors the engine's literal grammar, so a view name
// containing a quote or a newline -- or an arrangement whose title does --
// cannot break the statement around it.
//
// This deliberately MIRRORS src/integrations/calls.ts rather than importing
// it. The two surfaces are being built in parallel and neither should be able
// to break the other by refining its own helper; if a generated TypeScript
// emitter ever lands, it replaces both.

// callArgs renders `k: v, k: v`, dropping undefined / null and empty strings so
// an optional argument is OMITTED rather than sent as "".
//
// Omission is not tidiness. A `when(args.x) { ... }` guard in a filter is
// dropped when its argument is ABSENT and honoured when it is present, so
// sending `status: ""` to composedViews would filter for views whose status is
// the empty string -- which is none of them.
//
// An empty ARRAY is kept. `arrangements: []` is a meaningful value (a view with
// no sections yet), and it is not the same statement as omitting the argument.
function callArgs(args: Record<string, unknown>): string {
  const parts: string[] = [];
  for (const key of Object.keys(args)) {
    const value = args[key];
    if (value === undefined || value === null) continue;
    if (typeof value === "string" && value === "") continue;
    parts.push(`${key}: ${renderMemQLValue(value)}`);
  }
  return parts.join(", ");
}

export function buildCall(
  kind: "query" | "mutation",
  name: string,
  args: Record<string, unknown> = {},
): string {
  return `${kind} ${name}(${callArgs(args)})`;
}

export async function runQuery(
  query: QueryClient,
  name: string,
  args: Record<string, unknown> = {},
): Promise<Row[]> {
  const result = await query.executeNamed(name, buildCall("query", name, args));
  return result.rows();
}

export async function runMutation(
  query: QueryClient,
  name: string,
  args: Record<string, unknown> = {},
): Promise<void> {
  await query.executeNamed(name, buildCall("mutation", name, args));
}
