// Race-safe state machine for the Concept panel's row list + row detail.
//
// Mirrors conceptsCache.ts's generation-counter guard against an
// out-of-order settle, but the panel has TWO independently-racing async
// operations instead of one: the paged row list (loadPage) and the single
// selected row's detail (resolveSelection). Each gets its own generation
// counter so selecting a row does not discard an in-flight "load more", and
// loading another page does not discard an in-flight row selection. A third
// source of staleness -- a response from a connection that is no longer the
// live one (a cluster switch, a reconnect) -- is handled the same way: the
// caller invokes reset() on every connection state change, which bumps BOTH
// counters, so any in-flight loadPage()/resolveSelection() tied to the old
// connection is discarded when it settles.
//
// loadPage() additionally guards a FOURTH case that is not a staleness
// (supersession) problem at all: CONCURRENCY. Two "Load more" clicks landing
// before the first response returns both capture the same generation and
// the same cursor -- the generation check alone lets both through, since
// neither is stale relative to the other, and the page gets appended twice.
// A generation counter answers "is this the newest call", not "is a call
// already running"; loadPage() tracks the latter separately (see
// loadInFlightGeneration) and drops a call outright when one for the same
// generation is already in flight, rather than issuing a duplicate fetch.
//
// Deliberately free of `vscode` imports so it is unit-testable with fake
// fetch functions instead of a real SDK QueryClient / gRPC round-trip.
// ConceptPanel owns one instance and adapts it to the webview; this module
// owns only the row/detail/error/generation state machine, with no
// rendering concerns.
export class ConceptPanelState<T> {
  private rows: T[] = [];
  private cursor = "";
  private selection: string | undefined;
  private rowDetail: T | null = null;
  private errorMessage = "";

  // Bumped only by reset() (an external invalidating event: reload, or the
  // connection changing). loadPage() captures it before the await and
  // discards its result on both the success and the rejection path if it
  // no longer matches -- the same discard-on-mismatch shape as
  // ConceptsCache.load().
  private listGeneration = 0;

  // Set to the generation a loadPage() call is fetching for, and cleared
  // once that same call settles. A second loadPage() call for the SAME
  // generation is dropped at entry rather than starting a concurrent fetch
  // -- this is the concurrency guard, distinct from listGeneration's
  // staleness guard. Left untouched by reset(): once reset() bumps
  // listGeneration, this field's old value no longer matches the new
  // listGeneration, so a loadPage() call issued right after a reload/
  // reconnect is never blocked by an old fetch that hasn't settled yet.
  private loadInFlightGeneration: number | undefined;

  // Bumped by EVERY beginSelection() call, not just reset(). Selecting row
  // B must supersede an in-flight fetch for row A even though nothing
  // "invalidated" the panel -- the newer click IS the invalidation.
  private selectionGeneration = 0;

  get nodes(): T[] {
    return this.rows;
  }

  get nextCursor(): string {
    return this.cursor;
  }

  get selectedRowId(): string | undefined {
    return this.selection;
  }

  get detail(): T | null {
    return this.rowDetail;
  }

  get error(): string {
    return this.errorMessage;
  }

  // reset clears the row list, cursor, selection, detail, and error, and
  // invalidates both generations. Call on an explicit reload and on every
  // connection state change -- a fresh cluster (or a reconnect to the same
  // one) must not let a stale response from the old connection paint this
  // panel, and must not go on showing that old connection's rows while a
  // fresh load is in flight.
  reset(): void {
    this.listGeneration++;
    this.selectionGeneration++;
    this.rows = [];
    this.cursor = "";
    this.selection = undefined;
    this.rowDetail = null;
    this.errorMessage = "";
  }

  // setConnectionError records a synchronous "not connected" condition. It
  // takes no generation, unlike the async paths below -- there is no await
  // for a fresher call to race against, since the caller detects "not
  // connected" and calls this before ever starting a fetch.
  setConnectionError(message: string): void {
    this.errorMessage = message;
  }

  // loadPage awaits `fetch()` and appends its page to the row list, unless:
  //   - a load for the current generation is ALREADY in flight (the
  //     concurrency guard: two "Load more" clicks before the first
  //     response lands must not both fetch and append the same page), or
  //   - reset() ran while `fetch()` was in flight (the staleness guard).
  // In either case the response is discarded -- neither the rows, the
  // cursor, nor the error are written. Returns whether state changed, so
  // the caller knows whether a render is warranted; a discard path returns
  // false and writes nothing.
  async loadPage(fetch: () => Promise<{ rows: T[]; nextCursor: string }>): Promise<boolean> {
    if (this.loadInFlightGeneration === this.listGeneration) {
      return false;
    }
    const gen = this.listGeneration;
    this.loadInFlightGeneration = gen;
    try {
      const page = await fetch();
      if (gen !== this.listGeneration) return false;
      this.rows = this.rows.concat(page.rows);
      this.cursor = page.nextCursor;
      this.errorMessage = "";
      return true;
    } catch (err) {
      if (gen !== this.listGeneration) return false;
      this.errorMessage = err instanceof Error ? err.message : String(err);
      return true;
    } finally {
      // Only clear the marker if it still belongs to THIS call. A reset()
      // followed by a fresh loadPage() may already have written a newer
      // generation into loadInFlightGeneration by the time this settles;
      // clearing unconditionally would let a duplicate concurrent fetch
      // start against that newer generation.
      if (this.loadInFlightGeneration === gen) this.loadInFlightGeneration = undefined;
    }
  }

  // beginSelection marks `rowId` as the selection immediately (so the row
  // highlights before the detail round-trip returns) and returns a token
  // for the matching resolveSelection() call. Calling this again before a
  // prior resolveSelection() settles supersedes it: the prior call's token
  // no longer matches once this one bumps the counter.
  beginSelection(rowId: string): number {
    this.selection = rowId;
    return ++this.selectionGeneration;
  }

  // resolveSelection awaits `fetch()` and writes the detail pane, unless
  // this token has been superseded by a later beginSelection() (or a
  // reset()) -- in which case the response is discarded (neither the
  // detail nor the error are written), so a slow fetch for a since-deselected
  // row can never paint over a newer selection's detail.
  async resolveSelection(token: number, fetch: () => Promise<T | null>): Promise<boolean> {
    try {
      const detail = await fetch();
      if (token !== this.selectionGeneration) return false;
      this.rowDetail = detail;
      this.errorMessage = "";
      return true;
    } catch (err) {
      if (token !== this.selectionGeneration) return false;
      this.errorMessage = err instanceof Error ? err.message : String(err);
      this.rowDetail = null;
      return true;
    }
  }
}
