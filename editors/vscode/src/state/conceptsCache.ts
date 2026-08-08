// Race-safe cache for the Concepts tree's async list load.
//
// Lives under src/state/ (not src/views/) because it holds no VS Code types
// at all: src/views/ and src/webview/ are the adapter layers allowed to
// import `vscode`, and everything with logic in it stays out of them so it
// remains unit-testable under bare `node --test`. The rule is enforced
// mechanically -- see cmd/memql-lsp/vscodeimportrule_test.go.
//
// Staleness is guarded by the shared Latest token (src/async/latest.ts), the
// same guard connection/manager.ts and conceptPanelState.ts use: if the user
// switches clusters while a `load()` against the OLD cluster is still in
// flight, that stale request's continuation must not overwrite whatever a
// fresher `load()` already wrote (or is about to write).
// ConceptsTreeProvider owns one instance and adapts it to VS Code's
// TreeDataProvider surface; this module owns only the cache/error/staleness
// state machine, with no rendering concerns.
import { Latest, type LatestToken } from "../async/latest.js";

export class ConceptsCache<T> {
  private cache: T[] | undefined;
  private error: string | undefined;
  private readonly latest = new Latest<"concepts">();

  // The single in-flight attempt, memoized so concurrent callers share one
  // fetch. VS Code calls getChildren() once for the root and again for every
  // domain the user expands, and those calls overlap freely -- without this
  // each one issued its own listConcepts() round-trip against the same
  // cluster for the same answer. undefined whenever no fetch is running.
  private inFlight: Promise<T[]> | undefined;

  get cachedError(): string | undefined {
    return this.error;
  }

  // invalidate supersedes every in-flight load and clears cached state. Call
  // it on a manual refresh() and whenever the underlying connection state
  // changes -- concepts are per-cluster, so a cache/error that survived a
  // cluster switch would show the wrong cluster's registry. Any load()
  // already in flight for a prior generation has its result discarded (not
  // written to cache/error) when it eventually settles.
  //
  // The memoized promise is dropped too, not just superseded: it is a fetch
  // against the cluster we just left, so the next load() must start a fresh
  // one rather than be handed the old cluster's answer.
  invalidate(): void {
    this.latest.invalidate();
    this.cache = undefined;
    this.error = undefined;
    this.inFlight = undefined;
  }

  // load returns the cached list if present, joins an in-flight fetch if one
  // is running, and otherwise awaits `fetch()` and caches the result (or, on
  // rejection, caches the error message and returns []).
  //
  // If invalidate() runs while `fetch()` is in flight, the token captured
  // before the await is no longer current by the time it resumes. That stale
  // settle's value is still returned to THIS caller (so its awaiting
  // getChildren() resolves to something rather than hanging), but it is
  // discarded from the cache/error slots -- a fresher load, which may already
  // have written its own result by then, must win.
  async load(fetch: () => Promise<T[]>): Promise<T[]> {
    if (this.cache !== undefined) return this.cache;
    if (this.inFlight !== undefined) return this.inFlight;

    const attempt = this.fetchOnce(this.latest.current, fetch);
    this.inFlight = attempt;
    // Release the memo once THIS attempt settles, and only while it is still
    // the memoized one: invalidate() may already have cleared it and a
    // fresher load installed its own, and clearing unconditionally would
    // evict that fresher attempt, letting the next caller start the duplicate
    // fetch the memo exists to prevent. fetchOnce() catches both settle paths
    // and never rejects, so the same handler serves both slots -- passing it
    // twice rather than using .finally() keeps a future edit that lets a
    // rejection escape from turning into an unhandled one here.
    const release = (): void => {
      if (this.inFlight === attempt) this.inFlight = undefined;
    };
    attempt.then(release, release);
    return attempt;
  }

  // fetchOnce is load()'s body past the memo check, split out so `attempt`
  // exists as a value before anything closes over it.
  private async fetchOnce(
    token: LatestToken<"concepts">,
    fetch: () => Promise<T[]>,
  ): Promise<T[]> {
    try {
      const result = await fetch();
      if (!this.latest.isCurrent(token)) return result;
      this.cache = result;
      this.error = undefined;
      return result;
    } catch (err) {
      if (!this.latest.isCurrent(token)) return [];
      this.error = err instanceof Error ? err.message : String(err);
      return [];
    }
  }
}
