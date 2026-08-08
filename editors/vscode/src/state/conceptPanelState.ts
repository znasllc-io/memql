// Race-safe state machine for the Concept panel's row list + row detail.
//
// Uses the same shared supersession guard as conceptsCache.ts and
// connection/manager.ts (Latest, src/async/latest.ts), but the panel has TWO
// independently-racing async operations instead of one: the paged row list
// (loadPage) and the single selected row's detail (resolveSelection). Each
// gets its OWN guard so selecting a row does not discard an in-flight "load
// more", and loading another page does not discard an in-flight row
// selection -- and the guards' phantom kinds ("list" / "selection") make
// handing one guard's token to the other a compile error rather than a
// silent comparison of two unrelated counters. A third source of staleness --
// a response from a connection that is no longer the live one (a cluster
// switch, a reconnect) -- is handled the same way: the caller invokes reset()
// on every connection state change, which invalidates BOTH guards, so any
// in-flight loadPage()/resolveSelection() tied to the old connection is
// discarded when it settles.
//
// loadPage() additionally guards a FOURTH case that is not a staleness
// (supersession) problem at all: CONCURRENCY. Two "Load more" clicks landing
// before the first response returns both capture the same token and the same
// cursor -- the staleness check alone lets both through, since neither is
// stale relative to the other, and the page gets appended twice. Latest
// answers "is this the newest call", not "is a call already running";
// loadPage() tracks the latter separately (see loadInFlight) and drops a call
// outright when one for the same token is already in flight, rather than
// issuing a duplicate fetch.
//
// Lives under src/state/ (not src/webview/) because it holds no VS Code types
// at all: src/views/ and src/webview/ are the adapter layers allowed to
// import `vscode`, and everything carrying logic stays out of them so it is
// unit-testable with fake fetch functions instead of a real SDK QueryClient /
// gRPC round-trip. The rule is enforced mechanically -- see
// cmd/memql-lsp/vscodeimportrule_test.go. ConceptPanel owns one instance and
// adapts it to the webview; this module owns only the row/detail/error/
// staleness state machine, with no rendering concerns.
import { Latest, type LatestToken } from "../async/latest.js";

// SelectionToken is the handle beginSelection() hands out and
// resolveSelection() takes back. Exported so ConceptPanel can name it when it
// holds one across the detail round-trip.
export type SelectionToken = LatestToken<"selection">;

export class ConceptPanelState<T> {
  private rows: T[] = [];
  private cursor = "";
  private selection: string | undefined;
  private rowDetail: T | null = null;
  private errorMessage = "";

  // Persistent "live updates are off" notice -- deliberately a SEPARATE
  // field from errorMessage, not a reuse of it. errorMessage is transient:
  // loadPage()/resolveSelection() clear it on every success, because a
  // fresh good row-list load supersedes a stale data-fetch error. The
  // live-updates notice must survive exactly that -- a CDC subscription
  // failing does not stop ordinary queries from succeeding on the same
  // healthy connection, so the very next successful loadPage() would wipe
  // an errorMessage-based notice within moments of it appearing, leaving
  // the panel looking fully live when it silently is not. This field is
  // touched by exactly two call sites: setLiveUpdatesDegraded() (the
  // subscribe attempt threw) and clearLiveUpdatesDegraded() (a later
  // subscribe attempt succeeded). Nothing else -- not reset(), not
  // loadPage(), not resolveSelection() -- writes it.
  private liveUpdatesDegradedMessage = "";

  // Invalidated only by reset() (an external invalidating event: reload, or
  // the connection changing). loadPage() captures the current token with
  // `.current` -- NOT `.begin()`, since one page load does not supersede
  // another -- before the await, and discards its result on both the success
  // and the rejection path if the token is no longer current. Same
  // discard-on-mismatch shape as ConceptsCache.load().
  private readonly listLatest = new Latest<"list">();

  // Set to the token a loadPage() call is fetching for, and cleared once that
  // same call settles. A second loadPage() call for the SAME token is dropped
  // at entry rather than starting a concurrent fetch -- this is the
  // concurrency guard, distinct from listLatest's staleness guard. Left
  // untouched by reset(): once reset() invalidates listLatest, this field's
  // old value no longer matches the new current token, so a loadPage() call
  // issued right after a reload/reconnect is never blocked by an old fetch
  // that hasn't settled yet.
  private loadInFlight: LatestToken<"list"> | undefined;

  // Invalidated by EVERY beginSelection() call, not just reset() -- hence
  // begin() rather than current(). Selecting row B must supersede an
  // in-flight fetch for row A even though nothing "invalidated" the panel:
  // the newer click IS the invalidation.
  private readonly selectionLatest = new Latest<"selection">();

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

  get liveUpdatesError(): string {
    return this.liveUpdatesDegradedMessage;
  }

  // reset clears the row list, cursor, selection, detail, and error, and
  // invalidates both guards. Call on an explicit reload and on every
  // connection state change -- a fresh cluster (or a reconnect to the same
  // one) must not let a stale response from the old connection paint this
  // panel, and must not go on showing that old connection's rows while a
  // fresh load is in flight.
  //
  // Deliberately does NOT touch liveUpdatesDegradedMessage. reset() fires on
  // every connection state change, including transient ones ("connecting")
  // that are not yet a resolved subscribe attempt -- clearing the notice
  // here would flash it away before the caller even knows whether the new
  // connection's subscribe will succeed. Only setLiveUpdatesDegraded() /
  // clearLiveUpdatesDegraded() touch that field, so it survives exactly
  // until a subscribe attempt actually resolves.
  reset(): void {
    this.listLatest.invalidate();
    this.selectionLatest.invalidate();
    this.rows = [];
    this.cursor = "";
    this.selection = undefined;
    this.rowDetail = null;
    this.errorMessage = "";
  }

  // setLiveUpdatesDegraded / clearLiveUpdatesDegraded record whether the CDC
  // subscription behind live refresh is currently up. Unlike errorMessage,
  // nothing else in this class writes liveUpdatesDegradedMessage -- a
  // successful loadPage() or resolveSelection() must NOT silently erase a
  // "live updates are off" notice just because an unrelated ordinary query
  // happened to succeed on the same connection.
  setLiveUpdatesDegraded(message: string): void {
    this.liveUpdatesDegradedMessage = message;
  }

  clearLiveUpdatesDegraded(): void {
    this.liveUpdatesDegradedMessage = "";
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
    const token = this.listLatest.current;
    if (this.loadInFlight === token) {
      return false;
    }
    this.loadInFlight = token;
    try {
      const page = await fetch();
      if (!this.listLatest.isCurrent(token)) return false;
      this.rows = this.rows.concat(page.rows);
      this.cursor = page.nextCursor;
      this.errorMessage = "";
      return true;
    } catch (err) {
      if (!this.listLatest.isCurrent(token)) return false;
      this.errorMessage = err instanceof Error ? err.message : String(err);
      return true;
    } finally {
      // Only clear the marker if it still belongs to THIS call. A reset()
      // followed by a fresh loadPage() may already have written a newer
      // token into loadInFlight by the time this settles; clearing
      // unconditionally would let a duplicate concurrent fetch start against
      // that newer token.
      if (this.loadInFlight === token) this.loadInFlight = undefined;
    }
  }

  // beginSelection marks `rowId` as the selection immediately (so the row
  // highlights before the detail round-trip returns) and returns a token
  // for the matching resolveSelection() call. Calling this again before a
  // prior resolveSelection() settles supersedes it: the prior call's token
  // is no longer current once this one begins a new generation.
  beginSelection(rowId: string): SelectionToken {
    this.selection = rowId;
    return this.selectionLatest.begin();
  }

  // resolveSelection awaits `fetch()` and writes the detail pane, unless
  // this token has been superseded by a later beginSelection() (or a
  // reset()) -- in which case the response is discarded (neither the
  // detail nor the error are written), so a slow fetch for a since-deselected
  // row can never paint over a newer selection's detail.
  async resolveSelection(
    token: SelectionToken,
    fetch: () => Promise<T | null>,
  ): Promise<boolean> {
    try {
      const detail = await fetch();
      if (!this.selectionLatest.isCurrent(token)) return false;
      this.rowDetail = detail;
      this.errorMessage = "";
      return true;
    } catch (err) {
      if (!this.selectionLatest.isCurrent(token)) return false;
      this.errorMessage = err instanceof Error ? err.message : String(err);
      this.rowDetail = null;
      return true;
    }
  }
}
