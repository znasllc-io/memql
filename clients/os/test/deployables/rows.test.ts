import { describe, expect, it } from "vitest";

import {
  bundleForm,
  bundleFormLabel,
  domainOf,
  flatten,
  liveUrlFor,
  ownerLabel,
  siteFromRow,
  siteIsClusterOwned,
  SITE_STATUSES,
  siteIsCurrent,
  siteName,
  statusDotTone,
  statusTone,
  storefrontBinding,
} from "../../src/apps/deployables/rows";
import { APEX, DELETED, DOCS, PORTAL, SHOP, siteRow } from "./harness";

// The projections, on fixtures. Pure functions, so the list, the detail and the
// map are all checked against the same answers -- which is what stops the three
// surfaces of one app disagreeing about a row.

describe("the two wire shapes reach the same object", () => {
  it("flattens a subscription envelope and lets the envelope's id win", () => {
    const seeded = siteFromRow(SHOP);
    const folded = siteFromRow({
      id: "site-shop",
      createdAt: "2026-08-01T00:00:00Z",
      payload: { ...SHOP, id: "payload-id-that-must-not-win" },
    });
    expect(folded.id).toBe("site-shop");
    expect(folded.hostname).toBe(seeded.hostname);
    expect(folded.bundleRef).toBe(seeded.bundleRef);
    expect(folded.kind).toBe(seeded.kind);
  });

  it("leaves a flat row alone", () => {
    expect(flatten({ id: "a", hostname: "h" })).toEqual({ id: "a", hostname: "h" });
  });
});

describe("absent booleans take the concept's own default", () => {
  it("reads a missing `deleted` as NOT deleted", () => {
    // A folded event carries only what the write touched. Reading absent as
    // deleted would empty the list on the first event that did not name it.
    const partial = siteFromRow({ id: "site-x", hostname: "x.example.com" });
    expect(partial.deleted).toBe(false);
    expect(partial.systemOwned).toBe(false);
  });

  it("keeps an explicit true", () => {
    expect(siteFromRow(DELETED).deleted).toBe(true);
    expect(siteFromRow(PORTAL).systemOwned).toBe(true);
  });
});

describe("ownership", () => {
  it("labels an EMPTY ownerUserId cluster-owned -- the seeded portal is the case", () => {
    const portal = siteFromRow(PORTAL);
    expect(siteIsClusterOwned(portal)).toBe(true);
    expect(ownerLabel(portal, "u-me")).toBe("cluster-owned");
  });

  it("labels the viewer's own rows and somebody else's differently", () => {
    expect(ownerLabel(siteFromRow(SHOP), "u-me")).toBe("yours");
    expect(ownerLabel(siteFromRow(SHOP), "u-other")).toBe("another owner");
  });

  it("does not claim a row is yours when the viewer is unresolved", () => {
    // "" is "we do not know who is looking yet", and it must not match an
    // owner id that is also somehow blank.
    expect(ownerLabel(siteFromRow(SHOP), "")).toBe("another owner");
  });
});

describe("status", () => {
  it("is current only when live", () => {
    expect(siteIsCurrent(siteFromRow(SHOP))).toBe(true);
    expect(siteIsCurrent(siteFromRow(DOCS))).toBe(false);
  });

  it("gives disabled the WARN tone, not the error one", () => {
    // A deliberately paused site answering 503 is a state somebody chose.
    expect(statusTone(siteFromRow(siteRow({ id: "a", status: "disabled" })))).toBe("warn");
    expect(statusTone(siteFromRow(siteRow({ id: "b", status: "draft" })))).toBe("muted");
    expect(statusTone(siteFromRow(siteRow({ id: "c", status: "live" })))).toBe("ok");
  });

  it("maps onto the SHELL's dot language rather than inventing a fourth dot", () => {
    // Three states, three tones, and one of them is silence: a draft has never
    // been reachable, and painting a screen of new deployables amber would
    // alarm somebody about the normal case.
    expect(statusDotTone(siteFromRow(siteRow({ id: "a", status: "live" })))).toBe("reachable");
    expect(statusDotTone(siteFromRow(siteRow({ id: "b", status: "disabled" })))).toBe("unreachable");
    expect(statusDotTone(siteFromRow(siteRow({ id: "c", status: "draft" })))).toBe("unknown");
    // A row whose status has not arrived is not asserted to be anything.
    expect(statusDotTone(siteFromRow({ id: "d" }))).toBe("unknown");
  });

  it("refuses a status the enum does not declare rather than passing it through", () => {
    expect(siteFromRow(siteRow({ id: "d", status: "retired" })).status).toBe("");
  });
});

describe("the bundle reference's three usage forms", () => {
  it("names each one", () => {
    expect(bundleForm("file:///app/portal")).toBe("baked-portal");
    expect(bundleForm("file:///app/sites/docs")).toBe("baked-site");
    expect(bundleForm("blob://sites/site-shop/v1/")).toBe("uploaded");
    expect(bundleForm("")).toBe("none");
  });

  it("does NOT fold an unrecognised reference into one of the three", () => {
    // A reference nobody recognises is a fact worth showing as itself rather
    // than being described as something it may not be.
    expect(bundleForm("s3://elsewhere/bundle")).toBe("other");
    expect(bundleFormLabel("other")).toBe("unrecognised reference");
  });

  it("does not mistake a path that merely starts like the portal's", () => {
    expect(bundleForm("file:///app/portalish")).toBe("other");
  });

  it("labels each form for a reader", () => {
    expect(bundleFormLabel(bundleForm(PORTAL["bundleRef"] as string))).toBe("baked portal");
    expect(bundleFormLabel(bundleForm(DOCS["bundleRef"] as string))).toBe("baked site");
    expect(bundleFormLabel(bundleForm(SHOP["bundleRef"] as string))).toBe("uploaded bundle");
  });
});

describe("the storefront binding", () => {
  it("carries the store domain and the NAME of the token secret", () => {
    const binding = storefrontBinding(siteFromRow(SHOP));
    expect(binding.storeDomain).toBe("example.myshopify.com");
    expect(binding.storefrontTokenRef).toBe("shopify-storefront-token");
  });

  it("is empty for a site with no binding", () => {
    expect(storefrontBinding(siteFromRow(DOCS))).toEqual({
      storeDomain: "",
      storefrontTokenRef: "",
    });
  });
});

describe("naming and addressing", () => {
  it("names a deployable by its hostname", () => {
    expect(siteName(siteFromRow(SHOP))).toBe("shop.memql.example.com");
  });

  it("never renders blank", () => {
    expect(siteName(siteFromRow({ id: "site-x" }))).toBe("site-x");
    expect(siteName(siteFromRow({ id: "site-y", title: "Only a label" }))).toBe("Only a label");
  });

  it("addresses a site over https regardless of where the shell is served from", () => {
    expect(liveUrlFor("shop.memql.example.com")).toBe("https://shop.memql.example.com/");
  });

  it("returns nothing for a blank hostname rather than a link to https:///", () => {
    expect(liveUrlFor("")).toBe("");
  });
});

describe("the domain a hostname groups under", () => {
  it("drops the first label of a subdomain", () => {
    expect(domainOf("shop.memql.example.com")).toBe("memql.example.com");
    expect(domainOf("docs.memql.example.com")).toBe("memql.example.com");
  });

  it("leaves an APEX alone -- dropping a label would group it under `com`", () => {
    expect(domainOf(APEX["hostname"] as string)).toBe("example.org");
    expect(domainOf("localhost")).toBe("localhost");
  });

  it("folds case and a trailing dot", () => {
    expect(domainOf("SHOP.MemQL.Example.COM.")).toBe("memql.example.com");
  });

  it("answers empty for an empty hostname", () => {
    expect(domainOf("")).toBe("");
  });
});

describe("the status projection admits every declared value", () => {
  // THE BUG THIS PINS: `siteFromRow` normalised an unrecognised status to "",
  // against a hand-written allowlist that was a SECOND copy of the union type.
  // When `archived` arrived with the packages epic it went into the type and
  // not into the allowlist, so every archived row reached its component with a
  // blank status -- and the Archived filter, whose whole purpose is showing
  // them, listed rows that did not say what they were.
  //
  // It was inert while the enum had three members, which is exactly why it
  // survived review: the allowlist dropped nothing until there was something
  // to drop.
  it("keeps every value the concept declares, including archived", () => {
    for (const status of SITE_STATUSES) {
      const site = siteFromRow(siteRow({ id: "s1", status }));
      expect(site.status).toBe(status);
    }
  });

  it("still drops a value the concept does not declare", () => {
    // The reachable positive for the check itself: without this, the test
    // above would pass against a projection that had simply stopped filtering.
    const site = siteFromRow(siteRow({ id: "s1", status: "nonsense" }));
    expect(site.status).toBe("");
  });

  it("renders archived as its own state rather than as a fault", () => {
    const archived = siteFromRow(siteRow({ id: "s1", status: "archived" }));
    // No dot: an archived site is filed, not unreachable. The chip says which.
    expect(statusDotTone(archived)).toBe("unknown");
    expect(statusTone(archived)).toBe("muted");
  });
});
