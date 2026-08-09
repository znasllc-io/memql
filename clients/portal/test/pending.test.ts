// The pending-authorization record: the only state that crosses the redirect.
//
// Two properties matter beyond the round trip itself. It must be CONSUMED
// (a verifier handed out twice is a verifier that can be replayed), and the
// return path must be treated as untrusted input even though it came out of
// this origin's own storage -- it is written before a navigation to a third
// party and read after coming back, which is exactly the shape of assumption
// that turns into an open redirect.

import { describe, expect, it } from "vitest";

import {
  clearPending,
  consumePending,
  DEFAULT_RETURN_TO,
  safeReturnTo,
  savePending,
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
  returnTo: "/concepts/v1:cluster:node",
  createdAt: 1_000_000,
};

describe("pending authorization", () => {
  it("round-trips the state, verifier and destination", () => {
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

  it("removes the record even when it is unusable, so a failure cannot poison the next attempt", () => {
    const storage = memoryStorage();
    storage.setItem("memql-portal-pending-auth", "{{{not json");
    expect(consumePending(storage)).toBeNull();
    expect(storage.map.size).toBe(0);
  });

  it("discards an abandoned authorization rather than replaying it", () => {
    const storage = memoryStorage();
    savePending(RECORD, storage);
    // A tab left open overnight: the verifier is stale and the code it was
    // paired with expired long ago.
    expect(consumePending(storage, RECORD.createdAt + 11 * 60 * 1000)).toBeNull();
  });

  it("reports failure rather than silently proceeding when storage is blocked", () => {
    // The caller must NOT redirect after this: the callback would arrive with
    // no verifier and no state, which presents to the operator as a sign-in
    // that loops forever with no error.
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
    expect(safeReturnTo("/concepts/v1:cluster:node?x=1")).toBe("/concepts/v1:cluster:node?x=1");
  });

  it("rejects anything that could navigate off-origin", () => {
    for (const hostile of [
      "https://evil.example/steal",
      // Protocol-relative: the browser resolves "//evil.example" as an
      // absolute URL, which is the classic way this guard gets bypassed.
      "//evil.example",
      "http://evil.example",
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

  it("sanitizes the destination on the way OUT of storage too", () => {
    // Belt and braces: the record is written before handing control to a third
    // party, so it is re-checked when it comes back rather than trusted.
    const storage = memoryStorage();
    storage.setItem(
      "memql-portal-pending-auth",
      JSON.stringify({ ...RECORD, returnTo: "//evil.example" }),
    );
    expect(consumePending(storage, RECORD.createdAt)?.returnTo).toBe(DEFAULT_RETURN_TO);
  });
});
