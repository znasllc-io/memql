import { describe, expect, it } from "vitest";

import {
  ACCOUNT_ANY,
  ACCOUNT_NONE,
  DEFAULT_FILTER,
  applyFilters,
} from "../../src/apps/files/filters";
import { artifactFromRow } from "../../src/apps/files/rows";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { invitationFromRow } from "../../src/apps/users/rows";
import { domainMetaFromRow } from "../../src/apps/training/rows";

// The four ties, checked where they are cheapest to check: the pure
// projections and the one fold that decides what a filter shows.
//
// D1 IS THE SUBJECT OF EVERY ASSERTION HERE. A tie is an optional plain
// reference with no read effect, so the thing worth pinning is that a row
// WITHOUT one behaves exactly as it did before the field existed -- under
// every filter combination, on every surface.

describe("the site tie", () => {
  it("reads accountId, and an untied site reads empty", () => {
    expect(siteFromRow({ id: "s1", payload: { accountId: "a1" } }).accountId).toBe("a1");
    expect(siteFromRow({ id: "s1", payload: { hostname: "x.example.com" } }).accountId).toBe("");
  });
});

describe("the invitation tie", () => {
  it("reads accountId, and an untied invitation reads empty", () => {
    expect(invitationFromRow({ id: "i1", payload: { accountId: "a1" } }).accountId).toBe("a1");
    expect(invitationFromRow({ id: "i1", payload: { kind: "user" } }).accountId).toBe("");
  });
});

describe("the knowledge-domain tag", () => {
  it("reads the catalog row's own facts, which had no client read surface before", () => {
    const meta = domainMetaFromRow({
      id: "v1:knowledge:knowledgeDomain:realestate",
      payload: { name: "Residential real estate", category: "personal", tier: "A", accountId: "a1" },
    });
    expect(meta.name).toBe("Residential real estate");
    expect(meta.accountId).toBe("a1");
  });

  it("reads an untagged domain as untagged rather than as missing", () => {
    expect(domainMetaFromRow({ id: "d1", payload: { name: "Cooking" } }).accountId).toBe("");
  });
});

describe("the Files client filter", () => {
  const tied = artifactFromRow({
    id: "f1",
    lens: "artifact",
    kind: "file",
    title: "Contract.pdf",
    accountIds: ["a1"],
    createdAt: "2026-08-02T00:00:00Z",
  });
  const twoClients = artifactFromRow({
    id: "f2",
    lens: "artifact",
    kind: "file",
    title: "Joint.pdf",
    accountIds: ["a1", "a2"],
    createdAt: "2026-08-03T00:00:00Z",
  });
  // NO KEY AT ALL -- every row promoted before the field existed. This is the
  // row the whole fold is written around.
  const legacy = artifactFromRow({
    id: "f3",
    lens: "artifact",
    kind: "file",
    title: "Old.pdf",
    createdAt: "2026-08-01T00:00:00Z",
  });
  const rows = [tied, twoClients, legacy];

  function ids(accountId: string) {
    return applyFilters(rows, { ...DEFAULT_FILTER, folderId: null, accountId }).map((r) => r.id);
  }

  it("shows everything under 'any client', including rows with no key", () => {
    expect(ids(ACCOUNT_ANY).sort()).toEqual(["f1", "f2", "f3"]);
  });

  it("reads an ABSENT accountIds and an empty list as the same answer", () => {
    // The `folderId` lesson applied to a list: distinguishing them would hide
    // the entire pre-existing Library from the view somebody uses to find
    // what still needs filing.
    expect(ids(ACCOUNT_NONE)).toEqual(["f3"]);
  });

  it("matches membership, so a two-client file appears under both", () => {
    expect(ids("a1").sort()).toEqual(["f1", "f2"]);
    expect(ids("a2")).toEqual(["f2"]);
  });

  it("returns nothing for a client nothing is filed to, rather than everything", () => {
    expect(ids("a-unknown")).toEqual([]);
  });

  it("leaves every other facet's behaviour unchanged for an untied row", () => {
    // The D1 assertion, stated as a matrix: `legacy` has no account key, and
    // under every combination of the other filters it behaves as it did
    // before this epic.
    for (const kind of ["all", "file"] as const) {
      for (const showArchived of [false, true]) {
        const out = applyFilters(rows, {
          ...DEFAULT_FILTER,
          folderId: null,
          accountId: ACCOUNT_ANY,
          kind,
          showArchived,
        });
        expect(out.map((r) => r.id)).toContain("f3");
      }
    }
  });
});
