import type { QueryCallOptions, QueryClient } from "./query.js";
import type { Result } from "./types.js";

export interface QueryPageOptions extends QueryCallOptions {
  pageSize?: number;
  sort?: ReadonlyArray<readonly [field: string, direction: "asc" | "desc"]>;
}

/** Page an existing authorized named query using its generated builder.
 * The query keeps its argument checks, projection and access rules; only the
 * caller's result window changes. The named query must not already declare
 * paginate; optional sort is only valid when it declares no order. Existing
 * query order is preserved by default. Never pass raw user query text. */
export function queryPage(
  query: Pick<QueryClient, "executeNamed">,
  name: string,
  generatedCall: string,
  { pageSize = 10, sort = [], ...options }: QueryPageOptions = {},
): Promise<Result> {
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || !generatedCall.startsWith(`query ${name}(`) || !generatedCall.endsWith(")")) {
    throw new Error("queryPage requires the matching generated named-query call");
  }
  if (!Number.isSafeInteger(pageSize) || pageSize < 1 || pageSize > 500) throw new Error("queryPage pageSize must be an integer from 1 to 500");
  if (sort.some(([field, direction]) => !/^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$/.test(field) || (direction !== "asc" && direction !== "desc"))) {
    throw new Error("queryPage requires field paths and asc/desc directions");
  }
  const order = sort.flatMap(([field, direction]) => [JSON.stringify(field), JSON.stringify(direction)]).join(", ");
  const page = `paginate(${generatedCall}, ${pageSize})`;
  return query.executeNamed(name, sort.length ? `sort(${page}, ${order})` : page, options);
}
