// Coming back from GitHub (epic memql#4915, design section A step 2).
//
// ===========================================================================
// READ AT BOOT, SCRUBBED IMMEDIATELY, PARKED UNTIL THE SHELL EXISTS
// ===========================================================================
// The callback redirects the browser to this origin with a marker in the
// query. That is the AuthProvider's own shape and the conventions come with
// it: read it before anything renders, `history.replaceState` it away at
// once so a reload cannot replay it, then hand it to the surface that asked.
//
// PARKED, because the surface does not exist yet when the marker is read.
// The Shell mounts only once somebody is signed in (`app/App.tsx`), and the
// window that will show the result is opened from inside it -- so the value
// waits in this module between the two.
//
// TWO THINGS THIS MUST NOT DO:
//
//   * It must not render a result page of its own. The result belongs on the
//     surface that asked for the connection, beside the control somebody
//     pressed -- a full-page "You are connected!" is a toast with more
//     pixels.
//   * It must not treat a MISSING marker as a failure. Somebody who typed
//     the OS's address into a browser has not failed at anything, and this
//     module answers null for them exactly as it does before any connect has
//     ever happened.
//
// SCRUBBING IS SURGICAL. `AuthProvider` reads `code` and `state` out of the
// same query and requires `/auth/callback` to still be the pathname, so this
// removes ONLY its own two parameters and leaves the path, the hash and
// every other parameter exactly as they were. A blanket
// `replaceState({}, "", "/")` here would eat a sign-in mid-flight.

/** The callback's own marker: `ok`, or the refusal code it stopped on. */
export const CONNECT_RESULT_PARAM = "github_connect";

/** The section hint THIS OS put in the return path, so the answer lands
 *  where the question was asked. A courtesy rather than a contract: a
 *  callback that rebuilds the URL and drops it still returns somebody to a
 *  sensible place. */
export const CONNECT_SECTION_PARAM = "connect";

/** Where a return with no section hint goes: the Sources settings group,
 *  which is the one surface that is always there to receive it. */
export const DEFAULT_CONNECT_SECTION = "settings";

export interface ConnectReturn {
  /** `ok`, or a refusal code the copy table names. */
  reason: string;
  /** The Deployables section to open. */
  section: string;
}

/**
 * The path the callback should send the browser back to.
 *
 * A PATH AND NOT A URL. The cluster composes the origin from its own domain,
 * so nothing this browser says can redirect somebody off-cluster -- which is
 * the whole reason the argument is shaped this way.
 */
export function returnPathFor(section: string): string {
  const params = new URLSearchParams();
  params.set(CONNECT_SECTION_PARAM, section);
  return `/?${params.toString()}`;
}

/**
 * Read a return out of a query string, or null when there is no marker.
 *
 * A marker with an EMPTY value is still a return: a callback that redirected
 * without saying how it went has still brought somebody back, and the
 * surfaces read an unrecognised reason as "the cluster said something this
 * build has no name for" rather than as success.
 */
export function readConnectReturn(search: string): ConnectReturn | null {
  const params = new URLSearchParams(search);
  if (!params.has(CONNECT_RESULT_PARAM)) return null;
  const section = (params.get(CONNECT_SECTION_PARAM) ?? "").trim();
  return {
    reason: (params.get(CONNECT_RESULT_PARAM) ?? "").trim(),
    section: section === "" ? DEFAULT_CONNECT_SECTION : section,
  };
}

/** Whether a return says the connection was made. */
export function connectSucceeded(result: ConnectReturn): boolean {
  return result.reason === "ok";
}

/** The same query with this epic's two parameters removed, leading `?`
 *  included, or "" when nothing else was there. */
export function scrubbedSearch(search: string): string {
  const params = new URLSearchParams(search);
  params.delete(CONNECT_RESULT_PARAM);
  params.delete(CONNECT_SECTION_PARAM);
  const rest = params.toString();
  return rest === "" ? "" : `?${rest}`;
}

// ---------------------------------------------------------------------------
// The parked value
// ---------------------------------------------------------------------------

let parked: ConnectReturn | null = null;

/**
 * Read the marker, scrub it from the address bar, and park it.
 *
 * Called ONCE from `main.tsx`, at module scope, before React renders --
 * which is what makes it strictly earlier than `AuthProvider`'s own query
 * read and therefore impossible for either to eat the other's parameters.
 *
 * Returns what it parked, so a caller that wants to act immediately can.
 */
export function captureConnectReturn(win: Window): ConnectReturn | null {
  const result = readConnectReturn(win.location.search);
  if (result === null) return null;
  parked = result;
  const next = `${win.location.pathname}${scrubbedSearch(win.location.search)}${win.location.hash}`;
  // REPLACE, never push: a reload must not replay the return, and the back
  // button must not walk somebody into a URL whose meaning has been spent.
  win.history.replaceState({}, "", next);
  return result;
}

/**
 * Take the parked return, once.
 *
 * ONCE is the whole contract: the effect that consumes this runs again on a
 * StrictMode remount and on any re-render that re-registers it, and a value
 * that survived would open a second window every time.
 */
export function takeParkedConnectReturn(): ConnectReturn | null {
  const held = parked;
  parked = null;
  return held;
}

/** Tests only: forget anything parked, so one case cannot leak into the next. */
export function clearParkedConnectReturn(): void {
  parked = null;
}
