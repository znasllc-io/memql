import { renderMemQLValue } from "./memqlValue.js";
import type { QueryClient } from "./query.js";
import type { Row } from "./types.js";

// Resolving a relationship pointer to the row it names (epic memql#4661, task
// memql#4671).
//
// ===========================================================================
// WHAT THIS IS NOT: A JOIN
// ===========================================================================
// Spec D2 rules out engine joins for this epic, and nothing here asks for
// one. Every read below is an ordinary authorized single-concept walk over a
// SET OF IDS -- exactly the read a person clicking through to a row already
// makes -- so row authz is untouched: a target row the caller may not read
// simply does not come back, and the cell renders the id.
//
// The whole feature is therefore a client-side batching problem, and the two
// things it has to get right are the two that make batching worth doing.
//
// ===========================================================================
// ONE READ PER (CONCEPT, ID SET), NOT ONE PER CELL
// ===========================================================================
// A table with a lookup column over 100 rows would issue 100 reads if each
// cell resolved itself. The resolver collects the ids a page needs, reads
// them in one call per concept, and answers every cell from the result.
//
// ===========================================================================
// COALESCING IS NOT AN OPTIMISATION HERE
// ===========================================================================
// Several cells ask for the same id in the same tick, and several COLUMNS may
// point at the same concept. Without coalescing, a render issues one read per
// asker for an answer already in flight -- so the in-flight map is what stops
// a page from stampeding its own cluster on mount, which is a correctness
// property of the page as much as a performance one.

// A parsed `ref:<as>.<field>` binding.
//
// `as` is the DOMAIN label of a relationship -- what the edge MEANS -- not the
// engine's structural `type` (memql#3652). A person composing a view picks
// "the plan's ownerAgent", and `as` is the word they picked.
export interface RefBinding {
  readonly as: string;
  readonly field: string;
}

export const REF_PREFIX = "ref:";

// parseRefBinding reads `ref:ownerAgent.name`. Returns undefined for anything
// that is not one, including a malformed one -- a binding that named a
// relationship and no field is not a partial lookup, it is a plain field name
// that happens to start with the prefix.
export function parseRefBinding(binding: string): RefBinding | undefined {
  if (!binding.startsWith(REF_PREFIX)) return undefined;
  const rest = binding.slice(REF_PREFIX.length);
  const dot = rest.indexOf(".");
  if (dot <= 0 || dot === rest.length - 1) return undefined;
  return { as: rest.slice(0, dot), field: rest.slice(dot + 1) };
}

export function isRefBinding(binding: string): boolean {
  return parseRefBinding(binding) !== undefined;
}

// LookupCache is the per-page store. A CLASS rather than a module-level map so
// its lifetime is the page's: a cache that outlived the page would serve a
// stale display name after somebody renamed the row it points at, and the
// staleness would be invisible because the cell looks the same either way.
export class LookupCache {
  // concept -> id -> row (or null for "read, and there is no such row").
  //
  // NULL IS A CACHED ANSWER. "The row is gone or unreadable" is a fact worth
  // remembering: without it, every render re-asks for a row that will never
  // arrive, which is the worst case rather than the rare one -- a deleted
  // target is exactly the row a page keeps pointing at.
  private readonly rows = new Map<string, Map<string, Row | null>>();
  // In-flight reads, keyed the same way, so two askers in one tick share one.
  private readonly inflight = new Map<string, Promise<void>>();
  // Insertion order per concept, for the eviction below.
  private readonly order = new Map<string, string[]>();

  constructor(private readonly limit = 500) {}

  // get returns the cached row, `null` for a known-absent one, and `undefined`
  // for "not asked yet". Three states, because a caller renders differently
  // for each: the value, the id, and nothing-yet.
  get(concept: string, id: string): Row | null | undefined {
    return this.rows.get(concept)?.get(id);
  }

  has(concept: string, id: string): boolean {
    return this.rows.get(concept)?.has(id) === true;
  }

  // resolve reads whatever is missing, once.
  //
  // Returns when every requested id has an answer -- a row or a cached null.
  // A read that FAILS caches nothing: a transport error is not evidence that a
  // row is absent, and caching it as one would turn a blip into a permanently
  // wrong cell.
  async resolve(
    query: QueryClient,
    concept: string,
    ids: readonly string[],
  ): Promise<void> {
    const wanted = [...new Set(ids)].filter((id) => id !== "" && !this.has(concept, id));
    if (wanted.length === 0) return;

    const key = `${concept} ${wanted.slice().sort().join(",")}`;
    const existing = this.inflight.get(key);
    if (existing !== undefined) return existing;

    const run = (async () => {
      try {
        const rows = await readByIds(query, concept, wanted);
        const byId = new Map<string, Row>();
        for (const row of rows) {
          const id = typeof row["id"] === "string" ? row["id"] : "";
          if (id !== "") byId.set(id, row);
        }
        for (const id of wanted) {
          // A row the read did not return is CACHED AS ABSENT. It may be
          // deleted, or it may be one the caller's row authz does not admit;
          // the two are indistinguishable here by design -- an authz-filtered
          // read returns fewer rows, not an error -- and the cell renders the
          // id either way, which is the correct answer to both.
          this.put(concept, id, byId.get(id) ?? null);
        }
      } finally {
        this.inflight.delete(key);
      }
    })();

    this.inflight.set(key, run);
    return run;
  }

  private put(concept: string, id: string, row: Row | null): void {
    let bucket = this.rows.get(concept);
    let order = this.order.get(concept);
    if (bucket === undefined || order === undefined) {
      bucket = new Map();
      order = [];
      this.rows.set(concept, bucket);
      this.order.set(concept, order);
    }
    if (!bucket.has(id)) order.push(id);
    bucket.set(id, row);

    // Oldest-first eviction rather than true LRU. A page resolves its ids in
    // waves and reads them all again on the next render, so recency barely
    // varies within a wave -- and true LRU costs a touch on every `get`, which
    // is the hot path here. The bound is what matters: a page that scrolled
    // through ten thousand rows must not hold ten thousand target rows.
    while (order.length > this.limit) {
      const evicted = order.shift();
      if (evicted !== undefined) bucket.delete(evicted);
    }
  }
}

// readByIds is ONE authorized walk over a set of ids.
//
// `id in [...]` rather than a chain of `id==` ors: the engine's membership
// operator is the form this compiles to a single indexed predicate, and the
// or-chain would grow the parsed expression linearly in the id count.
//
// Quoting goes through renderMemQLValue, never hand-interpolation -- the one
// rule every call-building path is held to.
async function readByIds(
  query: QueryClient,
  concept: string,
  ids: readonly string[],
): Promise<readonly Row[]> {
  if (ids.length === 0) return [];
  const list = ids.map((id) => renderMemQLValue(id)).join(", ");
  const result = await query.executeNamed(
    "lookupRows",
    `concept==${concept} && id in [${list}]`,
  );
  return result.rows();
}
