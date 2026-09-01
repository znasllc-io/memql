import { describe, expect, it } from "vitest";

import {
  accountFingerprint,
  accountFromRow,
  accountIdOf,
  accountIdsOf,
  accountIsArchived,
  accountIsSelf,
  accountName,
  accountNameFrom,
  needsFirstRun,
  SELF_ACCOUNT_ID,
} from "../../src/apps/accounts/rows";

// The pure layer, checked without a browser, a cluster or React -- the reason
// these projections live in their own module. Every assertion here is about a
// function of a row.

describe("accountFromRow", () => {
  it("reads the flat seed form and the nested subscription form identically", () => {
    const flat = accountFromRow({
      id: "v1:accounts:account:a1",
      name: "Acme",
      domain: "acme.com",
      status: "active",
    });
    // The fold hands a CDC envelope whose concept fields sit under `payload`.
    // The two forms have to produce the same object or a row renders one way
    // on load and another the moment anything about it changes.
    const nested = accountFromRow({
      id: "v1:accounts:account:a1",
      payload: { name: "Acme", domain: "acme.com", status: "active" },
    });
    expect(nested).toEqual(flat);
  });

  it("does not invent a status for a row the fold has not filled", () => {
    // A partial CDC event carries only what the write touched. Defaulting to
    // "active" here would put an archived client back in the default list on
    // the strength of a guess.
    const row = accountFromRow({ id: "a1", payload: { name: "Acme" } });
    expect(row.status).toBe("");
    expect(accountIsArchived(row)).toBe(false);
  });
});

describe("SELF_ACCOUNT_ID", () => {
  // THE ID THIS APP COMPARES IS THE ID THE WIRE SENDS, which is the bare
  // shortId -- the engine strips `{concept}:` at every egress seam
  // (docs/public/concepts/identifiers.md).
  //
  // Pinned as its own case because every other test in this file supplies the
  // constant on BOTH sides of the comparison: seed the fixture with whatever
  // this constant says and `accountIsSelf` agrees with itself no matter what
  // it says. That is how the canonical spelling shipped -- the suite was
  // green, and in the browser the first-run card, the `you` chip and the
  // detail view's self branch were all silently dead.
  it("is the BARE row id, never the canonical one", () => {
    expect(SELF_ACCOUNT_ID).toBe("self");
    expect(SELF_ACCOUNT_ID).not.toContain(":");
  });

  it("matches the row shape the engine actually delivers", () => {
    // Literal, not the constant: this is the assertion that fails if the
    // constant drifts back to `v1:accounts:account:self`.
    expect(accountIsSelf(accountFromRow({ id: "self", name: "My company" }))).toBe(true);
    expect(
      accountIsSelf(accountFromRow({ id: "v1:accounts:account:self", name: "My company" })),
    ).toBe(false);
  });
});

describe("needsFirstRun", () => {
  it("is true only for the self singleton with no configuredAt", () => {
    const unconfiguredSelf = accountFromRow({ id: SELF_ACCOUNT_ID, name: "My company" });
    expect(needsFirstRun(unconfiguredSelf)).toBe(true);
  });

  it("is false once configuredAt is stamped", () => {
    const configured = accountFromRow({
      id: SELF_ACCOUNT_ID,
      name: "Acme",
      configuredAt: "2026-09-01T00:00:00Z",
    });
    expect(needsFirstRun(configured)).toBe(false);
  });

  it("is false for an ordinary account with no configuredAt", () => {
    // No other account can raise the card, because no other account was
    // filled in by a boot. An ordinary row's empty stamp means "created and
    // never edited", which is a smaller claim.
    const other = accountFromRow({ id: "a1", name: "Acme" });
    expect(needsFirstRun(other)).toBe(false);
    expect(accountIsSelf(other)).toBe(false);
  });

  it("is false when there is no self row at all", () => {
    // A cluster whose seed has not run has nothing to prepopulate and nothing
    // to save into. Offering the form would write a second self row.
    expect(needsFirstRun(null)).toBe(false);
  });

  it("treats whitespace as unanswered", () => {
    const blank = accountFromRow({ id: SELF_ACCOUNT_ID, configuredAt: "   " });
    expect(needsFirstRun(blank)).toBe(true);
  });
});

describe("accountFingerprint", () => {
  it("changes for every field a person would call a change", () => {
    const base = accountFromRow({ id: "a1", name: "Acme", domain: "acme.com", status: "active" });
    for (const patch of [
      { name: "Acme Ltd" },
      { domain: "acme.co.uk" },
      { primaryContactName: "Dana" },
      { primaryContactEmail: "dana@acme.com" },
      { notes: "renewal in May" },
      { status: "archived" },
    ]) {
      const changed = accountFromRow({ id: "a1", name: "Acme", domain: "acme.com", status: "active", ...patch });
      expect(accountFingerprint(changed)).not.toBe(accountFingerprint(base));
    }
  });

  it("does NOT change when only configuredAt moves", () => {
    // configuredAt is stamped by every save, so naming it would fire the cue
    // twice for one edit: once for the field somebody changed and once for
    // the timestamp that changing it stamped.
    const before = accountFromRow({ id: "a1", name: "Acme", configuredAt: "2026-08-01T00:00:00Z" });
    const after = accountFromRow({ id: "a1", name: "Acme", configuredAt: "2026-09-01T00:00:00Z" });
    expect(accountFingerprint(after)).toBe(accountFingerprint(before));
  });
});

describe("accountName", () => {
  it("never renders blank", () => {
    // A blank cell is indistinguishable from a cell that failed to render.
    const nameless = accountFromRow({ id: "v1:accounts:account:a1" });
    expect(accountName(nameless)).toContain("a1");
    expect(accountName(nameless).trim()).not.toBe("");
  });
});

describe("the tie readers", () => {
  it("reads an absent accountIds and an empty list as the same answer", () => {
    // Every artifact promoted before the field existed carries no key at all.
    // Distinguishing them would hide the entire pre-existing Library from the
    // untied view.
    expect(accountIdsOf({ id: "x" })).toEqual([]);
    expect(accountIdsOf({ id: "x", accountIds: [] })).toEqual([]);
    expect(accountIdsOf({ id: "x", payload: { accountIds: ["a1"] } })).toEqual(["a1"]);
  });

  it("drops non-string members rather than rendering them", () => {
    expect(accountIdsOf({ id: "x", accountIds: ["a1", 7, null] as never })).toEqual(["a1"]);
  });

  it("reads the single tie through the nested form too", () => {
    expect(accountIdOf({ id: "s1", payload: { accountId: "a1" } })).toBe("a1");
    expect(accountIdOf({ id: "s1" })).toBe("");
  });
});

describe("accountNameFrom", () => {
  const accounts = [accountFromRow({ id: "a1", name: "Acme" })];

  it("resolves a known id to its name", () => {
    expect(accountNameFrom(accounts, "a1")).toBe("Acme");
  });

  it("keeps an unresolvable id rather than rendering blank", () => {
    // An account can be archived, or owned by somebody whose rows this caller
    // cannot read. The tie is still true, and "tied to something you cannot
    // see" is more useful than nothing.
    expect(accountNameFrom(accounts, "a-unknown")).toBe("a-unknown");
  });

  it("renders nothing for no tie", () => {
    expect(accountNameFrom(accounts, "")).toBe("");
    expect(accountNameFrom(accounts, "   ")).toBe("");
  });
});
