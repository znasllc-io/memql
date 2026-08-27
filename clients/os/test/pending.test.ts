import { describe, expect, it } from "vitest";

import {
  clearPending,
  consumePending,
  DEFAULT_RETURN_TO,
  rememberPending,
  safeReturnTo,
  savePending,
  takePending,
  type StorageLike,
} from "../src/auth/pending";

function memoryStorage(): StorageLike & { map: Map<string, string> } {
  const map = new Map<string, string>();
  return {
    map,
    getItem: (k) => map.get(k) ?? null,
    setItem: (k, v) => void map.set(k, v),
    removeItem: (k) => void map.delete(k),
  };
}

const RECORD = {
  state: "STATE-1",
  verifier: "VERIFIER-1",
  returnTo: "/",
  createdAt: 1_000_000,
};

const PENDING_KEY = "memql-os-pending-auth";

describe("pending authorization", () => {
  it("round-trips the state and verifier", () => {
    const storage = memoryStorage();
    expect(savePending(RECORD, storage)).toBe(true);
    expect(consumePending(storage, RECORD.createdAt + 1000)).toEqual(RECORD);
  });

  it("consumes -- a second read gets nothing", () => {
    const storage = memoryStorage();
    savePending(RECORD, storage);
    expect(consumePending(storage, RECORD.createdAt)).not.toBeNull();
    expect(consumePending(storage, RECORD.createdAt)).toBeNull();
    expect(storage.map.size).toBe(0);
  });

  it("removes the record even when it is unusable", () => {
    const storage = memoryStorage();
    storage.setItem(PENDING_KEY, "{{{not json");
    expect(consumePending(storage)).toBeNull();
    expect(storage.map.size).toBe(0);
  });

  it("discards an abandoned authorization rather than replaying it", () => {
    const storage = memoryStorage();
    savePending(RECORD, storage);
    expect(consumePending(storage, RECORD.createdAt + 11 * 60 * 1000)).toBeNull();
  });

  it("reports failure rather than silently proceeding when storage is blocked", () => {
    expect(savePending(RECORD, null)).toBe(false);
    const throwing: StorageLike = {
      getItem: () => null,
      setItem: () => {
        throw new DOMException("QuotaExceededError");
      },
      removeItem: () => {},
    };
    expect(savePending(RECORD, throwing)).toBe(false);
  });

  it("clearPending tolerates absent storage", () => {
    expect(() => clearPending(null)).not.toThrow();
  });
});

describe("safeReturnTo", () => {
  it("accepts an in-app path", () => {
    expect(safeReturnTo("/")).toBe("/");
  });

  it("rejects anything that could navigate off-origin", () => {
    for (const hostile of [
      "https://evil.example/steal",
      "//evil.example",
      "javascript:alert(1)",
      "concepts",
      "",
      null,
      undefined,
      42,
    ]) {
      expect(safeReturnTo(hostile)).toBe(DEFAULT_RETURN_TO);
    }
  });
});

describe("pending authorization across magic-link new tabs (#4228 / #4707)", () => {
  it("defaultStorage writes localStorage so a fresh tab can consume", () => {
    sessionStorage.clear();
    localStorage.clear();

    expect(rememberPending("VERIFIER-1", "STATE-1")).toBe(true);

    expect(sessionStorage.getItem(PENDING_KEY)).toBeNull();
    expect(localStorage.getItem(PENDING_KEY)).not.toBeNull();

    const pending = takePending();
    expect(pending?.state).toBe("STATE-1");
    expect(pending?.verifier).toBe("VERIFIER-1");
    expect(sessionStorage.getItem(PENDING_KEY)).toBeNull();
  });

  it("still discards an expired record when it lives in localStorage", () => {
    sessionStorage.clear();
    localStorage.clear();
    expect(savePending(RECORD)).toBe(true);
    expect(localStorage.getItem(PENDING_KEY)).not.toBeNull();
    expect(consumePending(localStorage, RECORD.createdAt + 11 * 60 * 1000)).toBeNull();
  });

  it("consume still removes the localStorage record", () => {
    sessionStorage.clear();
    localStorage.clear();
    expect(savePending(RECORD)).toBe(true);
    expect(localStorage.getItem(PENDING_KEY)).not.toBeNull();
    expect(consumePending(localStorage, RECORD.createdAt)).not.toBeNull();
    expect(localStorage.getItem(PENDING_KEY)).toBeNull();
    expect(consumePending(localStorage, RECORD.createdAt)).toBeNull();
  });
});
