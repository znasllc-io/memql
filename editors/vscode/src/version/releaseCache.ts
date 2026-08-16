// The newest release that exists, cached.
//
// `listReleaseTags` shells out to `git ls-remote`. That is fine for the install
// wizard, which asks once when a human opens a page, and completely wrong for
// the surfaces this epic adds: a Clusters tree with N rows each wanting to know
// whether a newer release exists would become N subprocesses per repaint.
//
// `listReleaseTags` itself is UNCHANGED. It already never throws -- every
// failure arrives as an empty list with a reason -- so there is nothing to fix
// there, only a call rate to bound. This module is that bound.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// THREE PROPERTIES, EACH DEFENDING A DIFFERENT FAILURE
//
// Single-flight. Concurrent askers share one in-flight fetch, so a tree paint
// costs one subprocess no matter how many rows it has.
//
// Serve-stale-on-failure. A failed refresh keeps the last good tags. Dropping
// them would silently turn "a newer release exists" into "up to date" the
// moment an operator's network went away -- the exact failure direction this
// epic exists to prevent, arriving through the back door.
//
// Nothing fetches at rest. Construction does no work, and `peek()` never
// fetches, so activation stays offline and the first `git ls-remote` is caused
// by a surface actually being looked at.

import { listReleaseTags, type TagListing } from "../install/tags.js";
import { DEFAULT_STACK_REPO } from "../install/stackPin.js";

/**
 * Ten minutes.
 *
 * Releases are cut on a human timescale, so the cost of a stale listing is
 * showing "up to date" for a few minutes after a release lands -- while the
 * cost of a short TTL is a subprocess every time a tree repaints. An operator
 * who needs the answer now has `refresh()`, which ignores this entirely.
 */
export const RELEASE_TTL_MS = 10 * 60 * 1000;

export interface ReleaseListing {
  /** Release tags, newest first. Empty until a fetch has succeeded. */
  tags: string[];
  /**
   * Why the MOST RECENT attempt failed, when it did. Present alongside stale
   * `tags` -- that combination is what a surface renders as "showing the
   * listing from <fetchedAt>, could not refresh because <error>".
   */
  error?: string;
  /**
   * When `tags` were obtained. Describes the TAGS, not the last attempt: a
   * failed refresh must not advance it, or a page rendering "as of <fetchedAt>"
   * would claim a freshness it does not have.
   */
  fetchedAt: number;
}

export interface ReleaseCache {
  /** The listing, fetching if the TTL has expired. Single-flight. */
  get(): Promise<ReleaseListing>;
  /** Fetch regardless of the TTL. Joins an in-flight fetch rather than racing it. */
  refresh(): Promise<ReleaseListing>;
  /** What is already known, without causing a fetch. Undefined before the first attempt. */
  peek(): ReleaseListing | undefined;
}

export interface ReleaseCacheOptions {
  /** Injectable for tests; the real `git ls-remote` listing when absent. */
  fetch?: () => Promise<TagListing>;
  /** Injectable for tests. */
  now?: () => number;
  ttlMs?: number;
}

/**
 * The newest release, or undefined when nothing is known.
 *
 * Answers from a STALE listing too, which is the point of serving stale: when
 * the network is gone, the newest release we last saw is still the honest
 * answer to "is there something newer than this cluster".
 */
export function latestRelease(listing: ReleaseListing | undefined): string | undefined {
  return listing?.tags[0];
}

// The wizard's shape: ask the repository URL directly rather than a checkout's
// origin, because "what is the newest release" is a question about the project
// and not about any one cluster's clone -- and most clusters this asks on
// behalf of have no checkout on this machine at all.
const defaultFetch = (): Promise<TagListing> =>
  listReleaseTags({ cwd: process.cwd(), repo: DEFAULT_STACK_REPO });

export function createReleaseCache(options: ReleaseCacheOptions = {}): ReleaseCache {
  const fetch = options.fetch ?? defaultFetch;
  const now = options.now ?? Date.now;
  const ttlMs = options.ttlMs ?? RELEASE_TTL_MS;

  let tags: string[] = [];
  let fetchedAt = 0;
  let error: string | undefined;
  // Separate from `fetchedAt` on purpose. The TTL bounds how often we ATTEMPT,
  // so an offline editor retries every ttlMs rather than on every repaint;
  // `fetchedAt` describes when the tags we hold were obtained. Collapsing the
  // two would either hammer the network while offline or backdate the listing.
  let lastAttemptAt: number | undefined;
  let inFlight: Promise<ReleaseListing> | undefined;

  const snapshot = (): ReleaseListing => ({ tags: [...tags], error, fetchedAt });

  const start = (): Promise<ReleaseListing> => {
    if (inFlight !== undefined) return inFlight;
    const attempt = (async () => {
      try {
        const result = await fetch();
        // `listReleaseTags` reports "the repository published no release tags"
        // as an error string, indistinguishable here from a network failure.
        // Both are treated as a failed attempt, which is right: neither is a
        // reason to forget tags we already have.
        if (result.error === "") {
          tags = result.tags;
          fetchedAt = now();
          error = undefined;
        } else {
          error = result.error;
        }
      } catch (err) {
        // `listReleaseTags` does not reject, but an injected fetch might, and a
        // rejection escaping here would leave `inFlight` set forever.
        error = err instanceof Error ? err.message : String(err);
      } finally {
        lastAttemptAt = now();
        inFlight = undefined;
      }
      return snapshot();
    })();
    inFlight = attempt;
    return attempt;
  };

  return {
    get: () => {
      if (inFlight !== undefined) return inFlight;
      if (lastAttemptAt !== undefined && now() - lastAttemptAt < ttlMs) {
        return Promise.resolve(snapshot());
      }
      return start();
    },
    refresh: () => start(),
    peek: () => (lastAttemptAt === undefined ? undefined : snapshot()),
  };
}

/**
 * The one cache the whole extension shares.
 *
 * Shared rather than per-surface because single-flight is only meaningful
 * across callers: two surfaces holding their own caches would be two
 * subprocesses, which is the thing this module exists to prevent.
 */
export const releaseCache: ReleaseCache = createReleaseCache();
