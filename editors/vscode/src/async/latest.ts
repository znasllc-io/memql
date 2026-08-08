// The one guard against a superseded async result landing on current state.
//
// Every async round-trip in this extension races the world moving on beneath
// it: a dial the user superseded by picking another cluster, a listConcepts()
// against the cluster we just left, a row-detail fetch for a row the user has
// already clicked away from. The fix is always the same shape -- capture a
// token before the await, compare it after, discard on mismatch -- and B1
// shipped four independent hand-rolled copies of it (ConnectionManager's
// `generation`, ConceptsCache's `generation`, ConceptPanelState's
// `listGeneration` and `selectionGeneration`).
//
// The cost of four copies was never the duplication. It was that "is this
// module guarded?" became a per-module question with a per-module answer, so
// the one place that needed the guard and lacked it -- ConnectionManager
// observing Connection.done() -- was invisible precisely because there was no
// shared thing to be missing from. That gap shipped as a real bug: after a
// server-side drop every view still reported "Connected" and `get query()`
// kept handing out a client over a dead socket. One named type turns "which
// async paths are guarded" into a grep.
//
// Deliberately free of `vscode` imports -- this module is pure logic and is
// tested under bare `node --test`. See test/latest.test.ts.

// The phantom `Kind` is what makes `Latest<T>` carry a T at all: the guard
// stores no values, only tokens, so the type parameter's whole job is to keep
// two guards living on the SAME object from being confused for one another.
// ConceptPanelState holds both a "list" guard and a "selection" guard, and
// handing a list token to the selection guard would compile fine against a
// bare `number` while silently comparing unrelated counters. Branding the
// token with the guard's kind makes that a type error.
declare const latestTokenBrand: unique symbol;

// LatestToken is an opaque handle: callers pass it back to isCurrent() and
// otherwise never inspect it. It is a branded number rather than an object so
// begin() allocates nothing on a hot path (every row click mints one).
export type LatestToken<Kind extends string = string> = number & {
  readonly [latestTokenBrand]: Kind;
};

export class Latest<Kind extends string = string> {
  // Monotonic. Never reset -- a wrapped or reused token would make a stale
  // result test as current, which is the exact failure the guard exists to
  // prevent. Number.MAX_SAFE_INTEGER is not reachable by a UI event loop.
  private counter = 0;

  // current captures the token in force RIGHT NOW without superseding
  // anything. This is the read for an operation that is not itself an
  // invalidation -- a page load, a concepts fetch -- where two calls landing
  // in the same generation are peers, not rivals, and the caller has its own
  // story for that (ConceptsCache memoizes the in-flight promise;
  // ConceptPanelState.loadPage drops the duplicate).
  //
  // It is the member the issue's three-method sketch did not have, and the
  // one every non-invalidating caller needs: begin() everywhere would make
  // each ordinary load supersede the load before it, which would break
  // loadPage's in-flight marker outright (the marker is compared against the
  // current token, so a bump per call means it never matches).
  get current(): LatestToken<Kind> {
    return this.counter as LatestToken<Kind>;
  }

  // begin supersedes every outstanding token and returns the new current one.
  // This is the read for an operation that IS the invalidation -- connecting
  // to a cluster, selecting a row -- where starting the new call is precisely
  // what makes the old one stale.
  begin(): LatestToken<Kind> {
    return ++this.counter as LatestToken<Kind>;
  }

  // isCurrent reports whether `token` still names the newest generation. A
  // false answer means the caller's result is superseded and must be
  // discarded rather than written.
  isCurrent(token: LatestToken<Kind>): boolean {
    return token === this.counter;
  }

  // invalidate supersedes every outstanding token without minting one. This
  // is the read for an external event that makes in-flight work stale without
  // starting any of its own -- a disconnect, a Reload, a cluster switch.
  invalidate(): void {
    this.counter++;
  }
}
