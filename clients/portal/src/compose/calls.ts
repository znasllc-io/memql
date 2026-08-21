import { renderMemQLValue, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

// The wire vocabulary of the composer.
//
// The saved-view lifecycle -- `composedViews` / `composedViewById` /
// `createComposedView` / `updateComposedView` / `archiveComposedView` --
// moved onto the GENERATED typed methods when the TS emitter landed
// (memql#4232); see useSavedViews. What stays here is the one path the
// generator structurally cannot type: the composer runs constructs the USER
// picked at runtime, so the construct name is a value, not a compile-time
// method. That is the same generic-by-design posture as the concept
// browser, and quoting still goes through the SDK's renderMemQLValue, so a
// name containing a quote or a newline cannot break the statement around
// it.
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
