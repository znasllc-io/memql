import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  ACCOUNT_ANY,
  ACCOUNT_NONE,
  DEFAULT_LIST_FILTER,
  filterIsNarrowing,
  foldDeployables,
  groupFingerprint,
  listViewKey,
  sourceOf,
  standingInputFor,
  type ListFilter,
} from "../../src/apps/deployables/list";
import { deploymentFromRow, packageFromRow } from "../../src/apps/deployables/packages/rows";
import { railFor } from "../../src/apps/deployables/page/rail";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { siteRow } from "./harness";

// The Deployables list's fold (design D2): one row per thing that serves or
// will, grouped under the source it came from. Pure, so what the list SAYS
// is asserted here and the section only draws it.

const ACME = packageFromRow({
  id: "pkg-acme",
  ownerUserId: "u-me",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/storefront",
  repoRef: "main",
  credentialId: "",
  artifactId: "",
  deployedVersion: "aaaaaaaaaaaaaaaaaaaa",
  latestKnownVersion: "aaaaaaaaaaaaaaaaaaaa",
  updateAvailable: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
} as Row);

const ZIPPED = packageFromRow({
  id: "pkg-zip",
  ownerUserId: "u-me",
  name: "brochure",
  sourceKind: "artifact",
  repoUrl: "",
  repoRef: "",
  credentialId: "",
  artifactId: "artifact-zip",
  deployedVersion: "",
  latestKnownVersion: "",
  updateAvailable: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
} as Row);

const STORE = siteFromRow(
  siteRow({
    id: "site-store",
    hostname: "store.memql.example.com",
    status: "live",
    packageId: "pkg-acme",
    packageDeployableName: "storefront",
    accountId: "acct-1",
  }),
);

const ADMIN = siteFromRow(
  siteRow({
    id: "site-admin",
    hostname: "admin.memql.example.com",
    status: "disabled",
    packageId: "pkg-acme",
    packageDeployableName: "admin",
  }),
);

/** Hand-made, published from a Library zip. */
const SHOP = siteFromRow(
  siteRow({ id: "site-shop", hostname: "shop.memql.example.com", kind: "shopify_storefront", title: "Storefront", artifactId: "artifact-zip" }),
);

/** Hand-made, waiting for its CI's first push. */
const PENDING = siteFromRow(
  siteRow({ id: "site-ci", hostname: "ci.memql.example.com", status: "draft", bundleRef: "blob://sites/site-ci/pending/" }),
);

/** The seeded portal: baked into the image, which is none of the three sources. */
const PORTAL = siteFromRow(
  siteRow({ id: "site-portal", ownerUserId: "", hostname: "portal.memql.example.com", bundleRef: "file:///app/portal", systemOwned: true }),
);

const ARCHIVED = siteFromRow(siteRow({ id: "site-old", hostname: "old.memql.example.com", status: "archived" }));

function parkedRun(over: Partial<Record<string, unknown>> & { id: string; packageId: string }) {
  return deploymentFromRow({
    sourceVersion: "cccccccccccccccccccc",
    status: "awaiting_confirm",
    report: {
      deployables: [
        { name: "storefront", kind: "spa", path: "clients/web", buildPlan: "already built", output: "dist", prebuilt: true },
        { name: "admin", kind: "spa", path: "clients/admin", buildPlan: "already built", output: "dist", prebuilt: true },
      ],
      dslDomains: [],
      problems: [],
      ok: true,
    },
    dslVersion: "",
    deployables: [],
    error: null,
    requestedBy: "u-me",
    startedAt: "2026-09-01T13:00:00Z",
    finishedAt: "",
    createdAt: "2026-09-01T13:00:00Z",
    ...over,
  } as Row);
}

const fold = (
  sites = [STORE, ADMIN, SHOP],
  packages = [ACME],
  parked: ReturnType<typeof parkedRun>[] = [],
  filter: ListFilter = DEFAULT_LIST_FILTER,
  showArchived = false,
) => foldDeployables(sites, packages, parked, filter, showArchived);

describe("one row per thing that serves or will, grouped under its source", () => {
  it("groups a package's apps under it, once, and a hand-made site stands alone", () => {
    const groups = fold();
    expect(groups.map((g) => g.id)).toEqual(["pkg:pkg-acme", "site:site-shop"]);
    const acme = groups[0]!;
    expect(acme.pkg?.name).toBe("acme");
    // Sorted by hostname within the group.
    expect(acme.rows.map((r) => r.hostname)).toEqual(["admin.memql.example.com", "store.memql.example.com"]);
    // The app's manifest name is what a person calls it; the address stands beside it.
    expect(acme.rows.map((r) => r.name)).toEqual(["admin", "storefront"]);
    const shop = groups[1]!;
    expect(shop.pkg).toBeNull();
    expect(shop.rows).toHaveLength(1);
    // A hand-made site's name is its label when it has one, else its hostname.
    expect(shop.rows[0]?.name).toBe("Storefront");
    expect(shop.rows[0]?.hostname).toBe("shop.memql.example.com");
  });

  it("orders groups by their first address", () => {
    const groups = fold([SHOP, STORE, ADMIN, PORTAL], [ACME]);
    expect(groups.map((g) => g.rows[0]?.hostname)).toEqual([
      "admin.memql.example.com",
      "portal.memql.example.com",
      "shop.memql.example.com",
    ]);
  });

  it("a site naming a package the feed does not hold stands alone rather than vanishing", () => {
    const groups = fold([STORE], []);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.pkg).toBeNull();
    expect(groups[0]?.rows[0]?.hostname).toBe("store.memql.example.com");
  });
});

describe("the parked run: the waiting mark and the rows that will serve", () => {
  it("marks every row of the package a parked run belongs to", () => {
    const run = parkedRun({ id: "dep-1", packageId: "pkg-acme" });
    const acme = fold([STORE, ADMIN], [ACME], [run])[0]!;
    expect(acme.rows.every((r) => r.parked?.id === "dep-1")).toBe(true);
  });

  it("adds a row with no address for an app the report names that has no site yet", () => {
    const run = parkedRun({ id: "dep-1", packageId: "pkg-acme" });
    const acme = fold([STORE], [ACME], [run])[0]!;
    expect(acme.rows.map((r) => [r.name, r.hostname, r.site === null])).toEqual([
      ["storefront", "store.memql.example.com", false],
      ["admin", "", true],
    ]);
    // The rail reads the ROW, not the parked run's stage: a run waiting at the
    // confirm gate is not executing, so nothing is "running now". This app has
    // a source and a kind and nothing else yet, which is what it draws. (It
    // read `current` at What-it-is before, and since every app row of a source
    // carries that source's parked run, one unanswered gate redrew the whole
    // package -- serving apps included -- as unreached.)
    const states = railFor(standingInputFor(acme.rows[1]!)).stages.map((s) => s.state);
    // Build reads `skipped`, not `ahead`: this app is prebuilt, so there is no
    // build to run and saying so is more than the parked run was letting
    // through. That is the whole gain -- the row's own facts, instead of one
    // unanswered gate flattening every stop after the second.
    expect(states).toEqual(["done", "done", "ahead", "skipped", "ahead"]);
    expect(acme.rows[1]?.kind).toBe("spa");
  });

  it("a run whose report names no app still has a row, named after the source", () => {
    // A parked run must never be invisible: somebody who closed the window
    // mid-compose finds it on its row (design section A).
    const run = parkedRun({ id: "dep-1", packageId: "pkg-zip", report: { deployables: [], problems: [], ok: false } });
    const groups = fold([], [ZIPPED], [run]);
    expect(groups.map((g) => g.id)).toEqual(["pkg:pkg-zip"]);
    expect(groups[0]?.rows.map((r) => [r.name, r.app, r.hostname])).toEqual([["brochure", "", ""]]);
  });

  it("a parked run for a package the feed does not hold adds nothing", () => {
    const run = parkedRun({ id: "dep-1", packageId: "pkg-unknown" });
    expect(fold([STORE], [ACME], [run]).map((g) => g.id)).toEqual(["pkg:pkg-acme"]);
  });

  it("the newest parked run per package is the one that marks", () => {
    const older = parkedRun({ id: "dep-old", packageId: "pkg-acme", startedAt: "2026-09-01T12:00:00Z", createdAt: "2026-09-01T12:00:00Z" });
    const newer = parkedRun({ id: "dep-new", packageId: "pkg-acme" });
    const acme = fold([STORE, ADMIN], [ACME], [older, newer])[0]!;
    expect(acme.rows[0]?.parked?.id).toBe("dep-new");
  });
});

describe("the archived flip is a place", () => {
  it("hides archived sites and archived sources' rows from the active list, and shows only them under the flip", () => {
    const retired = { ...ACME, id: "pkg-old", name: "retired", status: "archived" };
    const oldApp = siteFromRow(siteRow({ id: "site-retired", hostname: "retired.memql.example.com", status: "archived", packageId: "pkg-old", packageDeployableName: "app" }));
    const active = fold([STORE, ARCHIVED, oldApp], [ACME, retired]);
    expect(active.map((g) => g.id)).toEqual(["pkg:pkg-acme"]);
    const archived = fold([STORE, ARCHIVED, oldApp], [ACME, retired], [], DEFAULT_LIST_FILTER, true);
    // SECTION FIRST, THEN ADDRESS -- so the retired SOURCE precedes the
    // retired standalone site even though `old.` sorts before `retired.`.
    // The archived flip is the same list under a different predicate, and it
    // would be a worse place if it sorted by a rule the active list does not.
    expect(archived.map((g) => g.id)).toEqual(["pkg:pkg-old", "site:site-old"]);
  });
});

describe("the facets are client-side folds over the seeded snapshot", () => {
  it("kind, status and client narrow, and a will-serve row has no status to match", () => {
    const run = parkedRun({ id: "dep-1", packageId: "pkg-acme" });
    const all = [STORE, ADMIN, SHOP];
    expect(fold(all, [ACME], [], { ...DEFAULT_LIST_FILTER, kind: "shopify_storefront" }).map((g) => g.id)).toEqual(["site:site-shop"]);
    expect(fold(all, [ACME], [], { ...DEFAULT_LIST_FILTER, status: "disabled" })[0]?.rows.map((r) => r.name)).toEqual(["admin"]);
    expect(fold(all, [ACME], [], { ...DEFAULT_LIST_FILTER, accountId: "acct-1" })[0]?.rows.map((r) => r.name)).toEqual(["storefront"]);
    // "No client" is a first-class answer: what still needs filing.
    const none = fold(all, [ACME], [], { ...DEFAULT_LIST_FILTER, accountId: ACCOUNT_NONE });
    expect(none.flatMap((g) => g.rows.map((r) => r.name))).toEqual(["admin", "Storefront"]);
    // A status facet leaves a will-serve row out: it has no status yet.
    const live = fold([STORE], [ACME], [run], { ...DEFAULT_LIST_FILTER, status: "live" });
    expect(live[0]?.rows.map((r) => r.name)).toEqual(["storefront"]);
  });

  it("reads each row's source as one of the three a person can choose, or none", () => {
    expect(sourceOf({ site: STORE, pkg: ACME })).toBe("repository");
    expect(sourceOf({ site: null, pkg: ZIPPED })).toBe("zip");
    expect(sourceOf({ site: SHOP, pkg: null })).toBe("zip");
    expect(sourceOf({ site: PENDING, pkg: null })).toBe("ci");
    expect(sourceOf({ site: siteFromRow(siteRow({ id: "s", bundleRef: "blob://sites/s/v3/" })), pkg: null })).toBe("ci");
    // Baked into the image is none of the three: it matches no source facet
    // rather than being described as something it is not.
    expect(sourceOf({ site: PORTAL, pkg: null })).toBe("");
    const ci = fold([STORE, SHOP, PENDING, PORTAL], [ACME], [], { ...DEFAULT_LIST_FILTER, source: "ci" });
    expect(ci.map((g) => g.id)).toEqual(["site:site-ci"]);
  });

  it("search matches the name, the address, the source and the app, case-insensitively", () => {
    const all = [STORE, ADMIN, SHOP];
    expect(fold(all, [ACME], [], { ...DEFAULT_LIST_FILTER, search: "ADMIN" })[0]?.rows.map((r) => r.name)).toEqual(["admin"]);
    expect(fold(all, [ACME], [], { ...DEFAULT_LIST_FILTER, search: "shop.memql" }).map((g) => g.id)).toEqual(["site:site-shop"]);
    // The source label: a package's rows all match their repository.
    expect(fold(all, [ACME], [], { ...DEFAULT_LIST_FILTER, search: "acme/storefront" })[0]?.rows).toHaveLength(2);
    expect(fold(all, [ACME], [], { ...DEFAULT_LIST_FILTER, search: "nothing like this" })).toEqual([]);
  });

  it("knows when it is narrowing, and writes every facet into the view key", () => {
    expect(filterIsNarrowing(DEFAULT_LIST_FILTER)).toBe(false);
    expect(filterIsNarrowing({ ...DEFAULT_LIST_FILTER, search: " " })).toBe(false);
    expect(filterIsNarrowing({ ...DEFAULT_LIST_FILTER, accountId: ACCOUNT_ANY })).toBe(false);
    expect(filterIsNarrowing({ ...DEFAULT_LIST_FILTER, kind: "spa" })).toBe(true);
    // The key is the transform's inputs written down: two filters that mean
    // different things must never share one, or the view would not rebuild.
    const keys = new Set([
      listViewKey(DEFAULT_LIST_FILTER, false),
      listViewKey(DEFAULT_LIST_FILTER, true),
      listViewKey({ ...DEFAULT_LIST_FILTER, search: "a" }, false),
      listViewKey({ ...DEFAULT_LIST_FILTER, kind: "spa" }, false),
      listViewKey({ ...DEFAULT_LIST_FILTER, status: "live" }, false),
      listViewKey({ ...DEFAULT_LIST_FILTER, accountId: "acct-1" }, false),
      listViewKey({ ...DEFAULT_LIST_FILTER, source: "zip" }, false),
    ]);
    expect(keys.size).toBe(7);
  });
});

describe("what counts as a change to a group", () => {
  it("fires on a site change, a source change and a run parking, and not on a field nobody would call one", () => {
    const base = fold([STORE, ADMIN], [ACME])[0]!;
    const before = groupFingerprint(base);
    const flipped = fold([{ ...STORE, bundleRef: "blob://sites/site-store/v9/" }, ADMIN], [ACME])[0]!;
    expect(groupFingerprint(flipped)).not.toBe(before);
    const updated = fold([STORE, ADMIN], [{ ...ACME, updateAvailable: true }])[0]!;
    expect(groupFingerprint(updated)).not.toBe(before);
    const waiting = fold([STORE, ADMIN], [ACME], [parkedRun({ id: "dep-1", packageId: "pkg-acme" })])[0]!;
    expect(groupFingerprint(waiting)).not.toBe(before);
    const notes = fold([{ ...STORE, notes: "scratch" }, ADMIN], [ACME])[0]!;
    expect(groupFingerprint(notes)).toBe(before);
    const created = fold([STORE, ADMIN], [{ ...ACME, createdAt: "2027-01-01T00:00:00Z" }])[0]!;
    expect(groupFingerprint(created)).toBe(before);
  });
});

// ---------------------------------------------------------------------------
// TWO SECTIONS, SO THE TWO KINDS NEVER INTERLEAVE
// ---------------------------------------------------------------------------
//
// Groups used to be sorted by first address alone, with no distinction between
// a package group and a standalone site -- so a two-app source sorted
// alphabetically between two hand-made ones and the list read as a jumble of
// two different shapes. The order is now section first, address within it.
describe("the two sections", () => {
  it("puts every source-backed group before every standalone one", () => {
    // `store` and `admin` belong to ACME; `shop` is hand-made. By address
    // alone `admin` < `ci` < `shop` < `store` would interleave them.
    const groups = fold([STORE, ADMIN, SHOP, PENDING], [ACME]);
    expect(groups.map((g) => g.section)).toEqual(["source", "standalone", "standalone"]);
  });

  it("marks the first group of each section, and only the first", () => {
    const groups = fold([STORE, ADMIN, SHOP, PENDING], [ACME]);
    expect(groups.map((g) => g.startsSection)).toEqual([true, true, false]);
  });

  it("still sorts by address WITHIN a section", () => {
    const groups = fold([STORE, ADMIN, SHOP, PENDING], [ACME]);
    const standalone = groups.filter((g) => g.section === "standalone");
    expect(standalone.map((g) => g.rows[0]!.hostname)).toEqual([
      "ci.memql.example.com",
      "shop.memql.example.com",
    ]);
  });

  it("marks the first group even when a section has no groups before it", () => {
    // Standalone only: it is still the first of its section and still carries
    // the heading, or a list of hand-made sites would render with none.
    const groups = fold([SHOP], []);
    expect(groups.map((g) => [g.section, g.startsSection])).toEqual([["standalone", true]]);
  });

  it("marks the first source group when there are no standalone sites at all", () => {
    const groups = fold([STORE, ADMIN], [ACME]);
    expect(groups.map((g) => [g.section, g.startsSection])).toEqual([["source", true]]);
  });
});

// ---------------------------------------------------------------------------
// A DECLARED APP THAT WAS NEVER DEPLOYED IS STILL ONE OF THE SOURCE'S APPS
// ---------------------------------------------------------------------------
//
// A site row is written only for an app that actually deployed, so an app
// SKIPPED at the confirm gate had no row and was invisible: absent from this
// list and absent from its own source's page. A person could therefore choose
// not to deploy one app of a source and then be unable to find it again --
// which is what `v1:platform:package.declares` exists to answer.
describe("what a source declares but has not deployed", () => {
  const DECLARES = { ...ACME, declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }] };

  it("lists the declared app that has no site, under its source", () => {
    const groups = fold([STORE], [DECLARES]);
    expect(groups).toHaveLength(1);
    expect(groups[0]!.rows.map((r) => r.name)).toEqual(["storefront", "web"]);
  });

  it("gives it no site and no address, because it has neither", () => {
    const web = fold([STORE], [DECLARES])[0]!.rows.find((r) => r.name === "web")!;
    expect(web.site).toBeNull();
    expect(web.hostname).toBe("");
    expect(web.kind).toBe("spa");
  });

  it("does NOT duplicate an app that has been deployed", () => {
    // `storefront` is both declared and live. One row, the site's.
    const rows = fold([STORE], [DECLARES])[0]!.rows.filter((r) => r.name === "storefront");
    expect(rows).toHaveLength(1);
    expect(rows[0]!.site).not.toBeNull();
  });

  it("adds nothing when every declared app has a site", () => {
    const both = { ...ACME, declares: [{ name: "storefront", kind: "spa" }, { name: "admin", kind: "spa" }] };
    expect(fold([STORE, ADMIN], [both])[0]!.rows).toHaveLength(2);
  });

  it("adds nothing for a source that has never been analyzed", () => {
    // The negative control: no catalogue, no invented rows.
    expect(fold([STORE], [{ ...ACME, declares: [] }])[0]!.rows.map((r) => r.name)).toEqual(["storefront"]);
  });
});

// ---------------------------------------------------------------------------
// A DISABLED APP IS LISTED AND INERT
// ---------------------------------------------------------------------------
//
// Skipping an app at the confirm gate used to leave it merely "declared with
// no site" -- indistinguishable from one nobody had got to yet -- so the
// console offered to deploy it, and discarding the result brought it straight
// back as another offer. The owner's choice had nowhere to live. It does now,
// and the row it produces can be found and cannot be deployed.
describe("a declared app that a parked run is about", () => {
  const SRC = { ...ACME, declares: [{ name: "storefront", kind: "spa" }, { name: "admin", kind: "spa" }] };

  it("carries the RUN, so the gate somebody left open can be reopened", () => {
    // The declared pass claimed the key before the parked pass could, so no
    // row ever carried `parked`: the reopen path was dead, and clicking the
    // row started ANOTHER analyze run every time. Somebody who closed the
    // window mid-compose could not get back to their own gate and silently
    // accumulated runs.
    const run = parkedRun({ id: "dep-parked", packageId: "pkg-acme" });
    const rows = fold([], [SRC], [run])[0]!.rows;
    const storefront = rows.find((r) => r.name === "storefront")!;
    expect(storefront.parked).not.toBeNull();
    expect(storefront.parked?.id).toBe("dep-parked");
  });

  it("still lists a declared app the run does NOT name", () => {
    // The negative control: the parked pass must not swallow the catalogue.
    // This run's report names storefront and admin, so a THIRD declared app
    // is still the declared pass's to add.
    const three = { ...SRC, declares: [...SRC.declares, { name: "docs", kind: "spa" }] };
    const rows = fold([], [three], [parkedRun({ id: "dep-parked", packageId: "pkg-acme" })])[0]!.rows;
    expect(rows.map((r) => r.name).sort()).toEqual(["admin", "docs", "storefront"]);
    expect(rows.find((r) => r.name === "docs")!.parked).toBeNull();
  });
});

describe("a declared app its owner turned off", () => {
  const SRC = {
    ...ACME,
    declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }],
    disabledDeployables: ["web"],
  };

  it("is still listed under its source, so it can be found and turned back on", () => {
    expect(fold([STORE], [SRC])[0]!.rows.map((r) => r.name)).toEqual(["storefront", "web"]);
  });

  it("is marked disabled, which is what makes it inert rather than an offer", () => {
    const web = fold([STORE], [SRC])[0]!.rows.find((r) => r.name === "web")!;
    expect(web.disabled).toBe(true);
    expect(web.site).toBeNull();
  });

  it("leaves an app nobody turned off alone", () => {
    // The negative control: `declares` on its own must not mark anything.
    const web = fold([STORE], [{ ...SRC, disabledDeployables: [] }])[0]!.rows.find((r) => r.name === "web")!;
    expect(web.disabled).toBe(false);
  });

  it("does not mark a DEPLOYED app disabled, whatever the list says", () => {
    // A name can linger after the app was deployed again. The site is the
    // fact; the list is a preference about one that has none.
    const rows = fold([STORE], [{ ...SRC, disabledDeployables: ["storefront", "web"] }])[0]!.rows;
    expect(rows.find((r) => r.name === "storefront")!.disabled).toBe(false);
  });
});
