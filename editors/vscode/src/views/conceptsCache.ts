// Race-safe cache for the Concepts tree's async list load.
//
// Deliberately free of `vscode` imports so it is unit-testable, mirroring
// `connection/manager.ts`'s generation-counter guard against an out-of-order
// settle: if the user switches clusters while a `load()` against the OLD
// cluster is still in flight, that stale request's continuation must not
// overwrite whatever a fresher `load()` already wrote (or is about to
// write). ConceptsTreeProvider owns one instance and adapts it to VS Code's
// TreeDataProvider surface; this module owns only the cache/error/
// generation state machine, with no rendering concerns.
export class ConceptsCache<T> {
  private cache: T[] | undefined;
  private error: string | undefined;
  private generation = 0;

  get cachedError(): string | undefined {
    return this.error;
  }

  // invalidate bumps the generation counter and clears cached state. Call it
  // on a manual refresh() and whenever the underlying connection state
  // changes -- concepts are per-cluster, so a cache/error that survived a
  // cluster switch would show the wrong cluster's registry. Any load()
  // already in flight for a prior generation has its result discarded (not
  // written to cache/error) when it eventually settles.
  invalidate(): void {
    this.generation++;
    this.cache = undefined;
    this.error = undefined;
  }

  // load returns the cached list if present, otherwise awaits `fetch()` and
  // caches the result (or, on rejection, caches the error message and
  // returns []).
  //
  // If invalidate() runs while `fetch()` is in flight, the generation
  // captured before the await no longer matches by the time it resumes.
  // That stale settle's value is still returned to THIS caller (so its
  // awaiting getChildren() resolves to something rather than hanging), but
  // it is discarded from the cache/error slots -- a fresher load, which may
  // already have written its own result by then, must win.
  async load(fetch: () => Promise<T[]>): Promise<T[]> {
    if (this.cache !== undefined) return this.cache;
    const gen = this.generation;
    try {
      const result = await fetch();
      if (gen !== this.generation) return result;
      this.cache = result;
      this.error = undefined;
      return result;
    } catch (err) {
      if (gen !== this.generation) return [];
      this.error = err instanceof Error ? err.message : String(err);
      return [];
    }
  }
}
