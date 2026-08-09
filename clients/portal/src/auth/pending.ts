// The one piece of state that has to survive a full-page redirect.
//
// Signing in navigates the whole document away to the identity service and
// back, which destroys every JavaScript value the portal held. Three things
// must outlive that: the PKCE verifier (or the code cannot be exchanged), the
// OAuth `state` (or the callback cannot be checked), and the route the
// operator was actually trying to reach (or every sign-in dumps them on the
// landing page, breaking the deep links #3316 requires).
//
// ---------------------------------------------------------------------------
// WHY sessionStorage, and why that is not the same decision as "where does the
// access token live".
// ---------------------------------------------------------------------------
//
// The access token is NEVER stored (see identityAuthSource.ts). This is not
// the access token. The verifier is a single-use secret that is worthless on
// its own: redeeming it requires the matching authorization code, which
// arrives at the portal's registered redirect_uri and is itself single-use and
// expires in minutes. So the exposure being traded is "script on this origin
// can read a value that is useful only in combination with a code it would
// have to also intercept, during a window measured in seconds" -- and script
// on this origin could simply start its own authorization instead. There is no
// storage-free alternative: the redirect is a document navigation, so an
// in-memory value cannot survive it.
//
// sessionStorage rather than localStorage, deliberately: it is scoped to the
// tab and cleared when the tab closes, so an abandoned half-finished sign-in
// does not persist on a shared machine. And cleared eagerly the moment the
// exchange completes or fails -- consume(), not read(), is the API.

const STORAGE_KEY = "memql-portal-pending-auth";

// MAX_AGE_MS bounds how long a pending authorization stays usable. The
// identity service's own auth codes expire well inside this; the bound exists
// for the abandoned-flow case (the operator opened sign-in, wandered off, came
// back tomorrow to a tab that never closed) so a stale verifier is discarded
// rather than replayed against a fresh callback.
const MAX_AGE_MS = 10 * 60 * 1000;

export interface PendingAuthorization {
  // The OAuth `state` this tab sent. The callback's state must equal it.
  state: string;
  // The PKCE code_verifier for the exchange.
  verifier: string;
  // Where the operator was heading, as an in-app path ("/concepts/v1:x:y").
  returnTo: string;
  // Epoch ms, for the staleness bound.
  createdAt: number;
}

// StorageLike is the slice of the Storage API used here, so tests need not
// stand up a whole jsdom storage.
export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

function defaultStorage(): StorageLike | null {
  try {
    return globalThis.sessionStorage ?? null;
  } catch {
    // Throws outright on an origin with storage blocked. Treated as absent;
    // savePending's caller surfaces the failure as an unusable sign-in rather
    // than crashing the page.
    return null;
  }
}

// savePending records the authorization in flight. Returns false when storage
// is unavailable -- the caller MUST NOT then redirect, because the callback
// would arrive with nothing to verify against and no verifier to exchange,
// which presents to the operator as a sign-in that loops forever.
export function savePending(
  pending: PendingAuthorization,
  storage: StorageLike | null = defaultStorage(),
): boolean {
  if (!storage) return false;
  try {
    storage.setItem(STORAGE_KEY, JSON.stringify(pending));
    return true;
  } catch {
    return false;
  }
}

// consumePending reads and REMOVES the pending authorization. Removal happens
// even on a malformed or expired record, and even if the caller then fails:
// a verifier that has been handed out once must never be handed out again,
// and leaving a stale record behind is how a failed sign-in poisons the next
// attempt.
export function consumePending(
  storage: StorageLike | null = defaultStorage(),
  now: number = Date.now(),
): PendingAuthorization | null {
  if (!storage) return null;
  let raw: string | null = null;
  try {
    raw = storage.getItem(STORAGE_KEY);
    storage.removeItem(STORAGE_KEY);
  } catch {
    return null;
  }
  if (!raw) return null;

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  const record = parsed as Partial<PendingAuthorization> | null;
  if (
    !record ||
    typeof record.state !== "string" ||
    typeof record.verifier !== "string" ||
    !record.state ||
    !record.verifier
  ) {
    return null;
  }
  const createdAt = typeof record.createdAt === "number" ? record.createdAt : 0;
  if (now - createdAt > MAX_AGE_MS) return null;

  return {
    state: record.state,
    verifier: record.verifier,
    returnTo: safeReturnTo(record.returnTo),
    createdAt,
  };
}

export function clearPending(storage: StorageLike | null = defaultStorage()): void {
  try {
    storage?.removeItem(STORAGE_KEY);
  } catch {
    // Nothing useful to do; the record expires on its own.
  }
}

// DEFAULT_RETURN_TO is where an operator lands with no recorded destination.
export const DEFAULT_RETURN_TO = "/";

// safeReturnTo admits only an in-app path.
//
// This is an OPEN-REDIRECT guard, and it matters even though the value came
// out of this origin's own sessionStorage: the record is written before a
// navigation to a third party and read after coming back, so treating it as
// trusted input is exactly the assumption that goes wrong. Anything that is
// not a single-slash-rooted path -- "https://evil.example", "//evil.example"
// (protocol-relative, which the browser resolves as an absolute URL), a bare
// word -- collapses to the default rather than being navigated to.
export function safeReturnTo(value: unknown): string {
  if (typeof value !== "string") return DEFAULT_RETURN_TO;
  if (!value.startsWith("/") || value.startsWith("//")) return DEFAULT_RETURN_TO;
  return value;
}
