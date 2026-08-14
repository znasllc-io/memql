// fetchSiteVersionHistory / justBefore (src/sites/history.ts, memql#3717
// ruling 2): the rollback picker's version walk.
//
// Driven against a fake QueryClient rather than through React, for the same
// reason rowWalk.test.ts is driven at the reducer: the walk is a property of
// the whole sequence of calls, not of any one render, and asserting it here
// can check the EXACT asOf timestamps issued -- not just that "some second
// row" appeared, which is the vacuous version of this test. The UI-level
// wiring (does the picker actually call this, does clicking a version call
// updateSiteBundle) is covered separately in sitesAuthoring.test.tsx.

import { describe, expect, it } from "vitest";
import type { QueryClient } from "@znasllc-io/memql-sdk-core/client";

import { fetchSiteVersionHistory, justBefore, MAX_HISTORY_VERSIONS } from "../src/sites/history";

interface FakeVersion {
  bundleRef: string;
  createdAt: string;
  status?: string;
}

// fakeQueryClient answers exactly the way the real engine's asOf semantics
// are documented to behave (dsl/deployment/queries.memql:78-97, D10/#2880):
// given a timestamp T, it returns the newest version whose createdAt <= T --
// not the version AT T, and not the next one after. `versions` must be
// supplied newest-first, matching how the walk consumes them.
function fakeQueryClient(versions: readonly FakeVersion[]): { query: QueryClient; calls: string[] } {
  const calls: string[] = [];
  const executeNamed = async (_name: string, call: string) => {
    calls.push(call);

    const asOfMatch = /^asOf\(siteById\(siteId: "[^"]*"\), "([^"]+)"\)$/.exec(call);
    const plainMatch = /^query siteById\(siteId: "[^"]*"\)$/.exec(call);
    if (!asOfMatch && !plainMatch) {
      throw new Error(`fakeQueryClient: unexpected call ${call}`);
    }

    const at = asOfMatch ? asOfMatch[1] : undefined;
    const chosen =
      at === undefined
        ? versions[0]
        : versions.find((v) => v.createdAt <= at);

    return {
      rows: () =>
        chosen === undefined
          ? []
          : [{ id: "site-1", createdAt: chosen.createdAt, bundleRef: chosen.bundleRef, status: chosen.status ?? "live" }],
    };
  };
  return { query: { executeNamed } as unknown as QueryClient, calls };
}

describe("justBefore", () => {
  it("backs an RFC3339 instant off by exactly one millisecond", () => {
    expect(justBefore("2026-08-10T00:00:00.000Z")).toBe("2026-08-09T23:59:59.999Z");
  });

  it("returns empty for an unparseable instant, so the walk can stop instead of loop", () => {
    expect(justBefore("not-a-date")).toBe("");
    expect(justBefore("")).toBe("");
  });
});

describe("fetchSiteVersionHistory", () => {
  const V3 = { bundleRef: "blob://sites/shop/v3/", createdAt: "2026-08-10T12:00:00.000Z" };
  const V2 = { bundleRef: "blob://sites/shop/v2/", createdAt: "2026-08-05T09:00:00.000Z" };
  const V1 = { bundleRef: "blob://sites/shop/v1/", createdAt: "2026-08-01T00:00:00.000Z" };

  it("offers a prior version -- not the current one -- with its own bundleRef and timestamp", async () => {
    const { query } = fakeQueryClient([V3, V2, V1]);
    const result = await fetchSiteVersionHistory(query, "site-1", 5);

    expect(result.length).toBeGreaterThanOrEqual(2);
    const [current, prior] = result;
    // The property that matters: version two is a DIFFERENT row, not the
    // current one repeated. A bug that re-issued the same asOf timestamp (or
    // dropped the -1ms backoff) would make these equal.
    expect(prior?.bundleRef).not.toBe(current?.bundleRef);
    expect(prior?.createdAt).not.toBe(current?.createdAt);
    expect(prior?.bundleRef).toBe(V2.bundleRef);
    expect(prior?.createdAt).toBe(V2.createdAt);
  });

  it("walks the full history newest-first when it fits under the limit", async () => {
    const { query } = fakeQueryClient([V3, V2, V1]);
    const result = await fetchSiteVersionHistory(query, "site-1", 5);
    expect(result.map((v) => v.bundleRef)).toEqual([V3.bundleRef, V2.bundleRef, V1.bundleRef]);
    expect(result.map((v) => v.createdAt)).toEqual([V3.createdAt, V2.createdAt, V1.createdAt]);
  });

  it("issues asOf at a strictly decreasing instant each step, one ms before the prior result", async () => {
    const { query, calls } = fakeQueryClient([V3, V2, V1]);
    await fetchSiteVersionHistory(query, "site-1", 5);

    expect(calls[0]).toBe('query siteById(siteId: "site-1")');
    expect(calls[1]).toBe(`asOf(siteById(siteId: "site-1"), "${justBefore(V3.createdAt)}")`);
    expect(calls[2]).toBe(`asOf(siteById(siteId: "site-1"), "${justBefore(V2.createdAt)}")`);
  });

  it("stops at the caller's limit even when older versions still exist", async () => {
    const { query, calls } = fakeQueryClient([V3, V2, V1]);
    const result = await fetchSiteVersionHistory(query, "site-1", 2);
    expect(result.length).toBe(2);
    // One plain read plus exactly one asOf step -- the walk did not keep
    // going past the limit "just in case".
    expect(calls.length).toBe(2);
  });

  it("defaults to MAX_HISTORY_VERSIONS when no limit is given", async () => {
    const many = Array.from({ length: MAX_HISTORY_VERSIONS + 5 }, (_, i) => ({
      bundleRef: `v${MAX_HISTORY_VERSIONS + 5 - i}`,
      createdAt: new Date(Date.UTC(2026, 7, MAX_HISTORY_VERSIONS + 5 - i)).toISOString(),
    }));
    const { query } = fakeQueryClient(many);
    const result = await fetchSiteVersionHistory(query, "site-1");
    expect(result.length).toBe(MAX_HISTORY_VERSIONS);
  });

  it("stops cleanly (not an error) when a version has no predecessor", async () => {
    const { query, calls } = fakeQueryClient([V1]);
    const result = await fetchSiteVersionHistory(query, "site-1", 5);
    expect(result).toEqual([{ bundleRef: V1.bundleRef, createdAt: V1.createdAt, status: "live" }]);
    // One plain read, one asOf step that came back empty, then stop.
    expect(calls.length).toBe(2);
  });

  it("returns empty when the site has no current row at all", async () => {
    const { query } = fakeQueryClient([]);
    const result = await fetchSiteVersionHistory(query, "site-1", 5);
    expect(result).toEqual([]);
  });

  it("returns empty for an empty siteId without calling the query surface", async () => {
    const { query, calls } = fakeQueryClient([V3, V2, V1]);
    const result = await fetchSiteVersionHistory(query, "", 5);
    expect(result).toEqual([]);
    expect(calls.length).toBe(0);
  });
});
