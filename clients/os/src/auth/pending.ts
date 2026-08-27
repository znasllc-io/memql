// PKCE pending state that must survive a full-page redirect.
//
// localStorage rather than sessionStorage (memql#4228, same cut on #4707):
// a magic-link from mail opens a NEW tab on this origin. sessionStorage is
// tab-scoped, so the tab that started Continue would hold the verifier and
// the tab that landed on the callback would find nothing. Any OS tab on this
// origin must be able to finish the exchange.
//
// This is not the access token. The record is consume-once and discarded
// after MAX_AGE_MS.

const STORAGE_KEY = "memql-os-pending-auth";
const MAX_AGE_MS = 10 * 60 * 1000;

export interface PendingAuthorization {
  state: string;
  verifier: string;
  returnTo: string;
  createdAt: number;
}

export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

function defaultStorage(): StorageLike | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

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
    // Record expires on its own.
  }
}

export const DEFAULT_RETURN_TO = "/";

export function safeReturnTo(value: unknown): string {
  if (typeof value !== "string") return DEFAULT_RETURN_TO;
  if (!value.startsWith("/") || value.startsWith("//")) return DEFAULT_RETURN_TO;
  return value;
}

/** AuthProvider helper: write verifier+state before the identity redirect. */
export function rememberPending(verifier: string, state: string): boolean {
  return savePending({
    state,
    verifier,
    returnTo: DEFAULT_RETURN_TO,
    createdAt: Date.now(),
  });
}

export function takePending(): PendingAuthorization | null {
  return consumePending();
}

export function forgetPending(): void {
  clearPending();
}
