import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { SITE_CONCEPT } from "../../src/apps/deployables/concepts";
import { headStateFor } from "../../src/apps/deployables/page/head";
import { HEAD_STATES, headActionFor } from "../../src/apps/deployables/page/rail";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import {
  credentialFingerprint,
  credentialFromRow,
} from "../../src/apps/deployables/sources/rows";
import { packageFromRow } from "../../src/apps/deployables/packages/rows";
import { siteFromRow } from "../../src/apps/deployables/rows";
import {
  NOTE,
  PDF,
  PORTAL,
  SHOP,
  ZIP,
  click,
  credentialRow,
  emit,
  fakeConnection,
  siteRow,
  type as typeInto,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// The deployable page (epic memql#4885, task memql#4890): one page for every
// deployable that has a site row, in its standing and deploy readings.
//
// Everything here goes through `connection.query` and `connection.subscriptions`
// exactly as production does, so the real LiveCollection, the real projections
// and the real generated builders all run -- the harness answers at
// `executeNamed` for exactly that reason. The assertions are what a person
// SEES: a button's text and whether it is disabled, a sentence, a data-state.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection | null, opts: { role?: string; userId?: string; section?: string } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp
        sectionId={opts.section ?? "deployables"}
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
      />,
      { role: opts.role ?? "owner", userId: opts.userId ?? "u-me" },
    ),
  );
}

/** Opens a deployable from the list and returns its page. */
async function open(hostname: string): Promise<HTMLElement> {
  // AN ARCHIVED DEPLOYABLE IS FOUND UNDER THE FLIP, which is where a person
  // finds one: the list's default population is what serves, and the archive
  // is a place below it rather than a checkbox in front of it (DESIGN.md
  // rule 10). The wait is for the feed rather than for the row, so "not on
  // the active list" is a real answer rather than "has not arrived yet".
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  if (screen.queryAllByText(hostname).length === 0) {
    const flip = screen.queryByRole("button", { name: /Show archived/ });
    if (flip !== null) await click(flip);
  }
  // Before the page opens, the only element carrying the hostname is the
  // list row; once it is open the Head, the rail and the address all do.
  await click((await screen.findByText(hostname)).closest("button"));
  return screen.findByRole("region", { name: `Deployable ${hostname}` });
}

async function mountAndOpen(seed: FakeSeed, hostname: string, opts: { role?: string } = {}) {
  const connection = fakeConnection(seed);
  mount(connection, opts);
  const page = await open(hostname);
  return { connection, page };
}

const HEAD_LABELS = ["Analyze", "Deploy", "Make it live", "Deploy the update", "Retry", "Redeploy"] as const;

/** The Head's one action, or null when the page offers none. */
function headAction(page: HTMLElement): HTMLButtonElement | null {
  const head = page.querySelector(".os-head") as HTMLElement;
  for (const label of HEAD_LABELS) {
    const found = within(head).queryByRole("button", { name: label });
    if (found) return found as HTMLButtonElement;
  }
  return null;
}

beforeEach(() => {
  h.connection = null;
});

// ---------------------------------------------------------------------------
// Fixtures: a package, the site it produced, and its runs
// ---------------------------------------------------------------------------

const ACME: Row = {
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
};

/** The site ACME produced: joined to its package by `packageId`. */
const STORE = siteRow({
  id: "site-store",
  hostname: "store.memql.example.com",
  kind: "spa",
  status: "live",
  bundleRef: "blob://sites/site-store/v2/",
  packageId: "pkg-acme",
  packageDeployableName: "storefront",
  createdAt: "2026-09-01T12:01:00Z",
});

/** A sibling app the same package produced. */
const ADMIN = siteRow({
  id: "site-admin",
  hostname: "admin.memql.example.com",
  kind: "spa",
  status: "live",
  bundleRef: "blob://sites/site-admin/v2/",
  packageId: "pkg-acme",
  packageDeployableName: "admin",
});

const REPORT = {
  name: "acme",
  formatVersion: 1,
  deployables: [
    { name: "storefront", kind: "spa", path: "clients/web", buildPlan: "already built: dist", output: "dist", prebuilt: true },
  ],
  dslDomains: [],
  problems: [],
  ok: true,
};

function run(over: Partial<Record<string, unknown>> & { id: string }): Row {
  return {
    packageId: "pkg-acme",
    sourceVersion: "cccccccccccccccccccc",
    status: "succeeded",
    report: REPORT,
    dslVersion: "",
    deployables: [{ name: "storefront", siteId: "site-store", hostname: "store.memql.example.com", bundleRef: "blob://sites/site-store/v2/", version: "v2", created: false }],
    snapshotArtifactId: "",
    buildLogTail: "",
    error: null,
    requestedBy: "u-me",
    startedAt: "2026-09-01T12:00:00Z",
    finishedAt: "2026-09-01T12:00:30Z",
    createdAt: "2026-09-01T12:00:00Z",
    ...over,
  };
}

const SUCCEEDED = run({ id: "dep-ok" });
const PARKED = run({ id: "dep-parked", status: "awaiting_confirm", deployables: [], finishedAt: "", startedAt: "2026-09-01T13:00:00Z", createdAt: "2026-09-01T13:00:00Z" });
const BUILDING = run({ id: "dep-building", status: "building", deployables: [], finishedAt: "", startedAt: "2026-09-01T13:00:00Z", createdAt: "2026-09-01T13:00:00Z" });
const REFUSED = run({
  id: "dep-refused",
  status: "refused",
  deployables: [],
  buildLogTail: "npm ERR! missing script: build",
  error: { code: "deployable_build_failed", message: "npm run build exited 1 in clients/web -- add a build script, or ship the built output", scope: "storefront" },
  startedAt: "2026-09-01T13:00:00Z",
  createdAt: "2026-09-01T13:00:00Z",
});

const WITH_PACKAGE: FakeSeed = { sites: [STORE, ADMIN], packages: [ACME], deployments: { "pkg-acme": [SUCCEEDED] } };

// ---------------------------------------------------------------------------
// The Head's one action, for every row of the design's table
// ---------------------------------------------------------------------------

describe("the Head's action, by state", () => {
  it("derives every row of the table from what the page holds", () => {
    const site = siteFromRow(STORE);
    const pkg = packageFromRow(ACME);
    const d = (over: Partial<Record<string, unknown>> & { id: string }) => {
      const flat = run(over);
      return {
        id: String(flat["id"]),
        packageId: "pkg-acme",
        sourceVersion: "c",
        status: String(flat["status"]),
        report: REPORT,
        dslVersion: "",
        deployables: [],
        snapshotArtifactId: "",
        buildLogTail: "",
        error: null,
        requestedBy: "u-me",
        startedAt: "",
        finishedAt: "",
        createdAt: "",
      };
    };
    const base = { site, pkg, run: null, canWrite: true };

    expect(headStateFor({ ...base, run: d({ id: "r", status: "building" }) })).toEqual({ at: "running" });
    expect(headStateFor({ ...base, run: d({ id: "r", status: "analyzing" }) })).toEqual({ at: "running" });
    // Every app of an existing deployable already has an address, so the
    // placements are complete by construction and Deploy is enabled.
    expect(headStateFor({ ...base, run: d({ id: "r", status: "awaiting_confirm" }) })).toEqual({ at: "awaiting_confirm", placementsComplete: true });
    expect(headStateFor({ ...base, run: d({ id: "r", status: "refused" }) })).toEqual({ at: "refused_or_failed" });
    expect(headStateFor({ ...base, run: d({ id: "r", status: "failed" }) })).toEqual({ at: "refused_or_failed" });
    expect(headStateFor({ ...base, site: { ...site, status: "draft" } })).toEqual({ at: "draft_with_bundle" });
    expect(headStateFor({ ...base, site: { ...site, status: "draft", bundleRef: "blob://sites/site-store/pending/" } })).toBeNull();
    expect(headStateFor({ ...base, pkg: { ...pkg, updateAvailable: true } })).toEqual({ at: "live", updateAvailable: true });
    expect(headStateFor(base)).toEqual({ at: "live", updateAvailable: false });
    // A paused site reads as live for the action's purpose; Resume is the
    // Live stop's.
    expect(headStateFor({ ...base, site: { ...site, status: "disabled" } })).toEqual({ at: "live", updateAvailable: false });
    // A hand-made site: live, nothing newer, so the quiet Redeploy.
    expect(headStateFor({ ...base, pkg: null })).toEqual({ at: "live", updateAvailable: false });
    // No action at all: archived, system-owned, an archived source, a reader.
    expect(headStateFor({ ...base, site: { ...site, status: "archived" } })).toBeNull();
    expect(headStateFor({ ...base, site: { ...site, systemOwned: true } })).toBeNull();
    expect(headStateFor({ ...base, pkg: { ...pkg, status: "archived" } })).toBeNull();
    expect(headStateFor({ ...base, canWrite: false })).toBeNull();
    // And every derived state is one the rail's table answers.
    for (const state of HEAD_STATES) expect(() => headActionFor(state)).not.toThrow();
  });

  it("a running run: no action -- the rail is moving", async () => {
    const { page } = await mountAndOpen({ ...WITH_PACKAGE, deployments: { "pkg-acme": [BUILDING, SUCCEEDED] } }, "store.memql.example.com");
    expect(headAction(page)).toBeNull();
    const rail = within(page).getByRole("list", { name: "Deployable stops" });
    const states = [...rail.querySelectorAll(":scope > li")].map((li) => li.getAttribute("data-state"));
    expect(states).toEqual(["done", "done", "done", "current", "ahead"]);
  });

  it("a parked run: Deploy, enabled, and it sends confirm: true with no placements", async () => {
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, deployments: { "pkg-acme": [PARKED, SUCCEEDED] } }, "store.memql.example.com");
    const deploy = headAction(page);
    expect(deploy?.textContent).toBe("Deploy");
    expect(deploy?.disabled).toBe(false);
    expect(deploy?.getAttribute("data-tone")).toBe("primary");
    await click(deploy);
    expect(connection.callsNamed("packageDeploy")).toEqual(['builtin packageDeploy(packageId: "pkg-acme", confirm: true)']);
  });

  it("a refused run: Retry, which starts a fresh unconfirmed run", async () => {
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, deployments: { "pkg-acme": [REFUSED, SUCCEEDED] } }, "store.memql.example.com");
    const retry = headAction(page);
    expect(retry?.textContent).toBe("Retry");
    await click(retry);
    expect(connection.callsNamed("packageDeploy")).toEqual(['builtin packageDeploy(packageId: "pkg-acme", confirm: false)']);
  });

  it("a draft with a bundle: Make it live, which flips the status", async () => {
    const draft = siteRow({ ...STORE, id: "site-store", status: "draft" });
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, sites: [draft, ADMIN] }, "store.memql.example.com");
    const live = headAction(page);
    expect(live?.textContent).toBe("Make it live");
    await click(live);
    expect(connection.callsNamed("updateSiteStatus")).toEqual(['mutation updateSiteStatus(siteId: "site-store", status: "live")']);
  });

  it("live with a newer version upstream: Deploy the update, primary", async () => {
    const { connection, page } = await mountAndOpen(
      { ...WITH_PACKAGE, packages: [{ ...ACME, latestKnownVersion: "bbbbbbbbbbbbbbbbbbbb", updateAvailable: true }] },
      "store.memql.example.com",
    );
    const update = headAction(page);
    expect(update?.textContent).toBe("Deploy the update");
    expect(update?.getAttribute("data-tone")).toBe("primary");
    // The Source stop carries the standing mark and the newer version.
    expect(within(page).getByText("update")).toBeTruthy();
    expect(within(page).getByText(/newer version upstream: bbbbbbb\./)).toBeTruthy();
    await click(update);
    expect(connection.callsNamed("packageDeploy")).toEqual(['builtin packageDeploy(packageId: "pkg-acme", confirm: false)']);
  });

  it("live with nothing newer: Redeploy, quiet", async () => {
    const { connection, page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com");
    const redeploy = headAction(page);
    expect(redeploy?.textContent).toBe("Redeploy");
    expect(redeploy?.getAttribute("data-tone")).toBe("quiet");
    await click(redeploy);
    expect(connection.callsNamed("packageDeploy")).toEqual(['builtin packageDeploy(packageId: "pkg-acme", confirm: false)']);
  });

  it("a hand-made site's Redeploy opens the Source stop's zip picker", async () => {
    const { connection, page } = await mountAndOpen({ sites: [SHOP], artifacts: [ZIP] }, "shop.memql.example.com");
    expect(connection.calls.filter((c) => c === "query libraryArtifacts()")).toHaveLength(0);
    await click(headAction(page));
    expect(await within(page).findByText("storefront-build.zip")).toBeTruthy();
    expect(connection.calls.filter((c) => c === "query libraryArtifacts()")).toHaveLength(1);
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
  });

  it("a system-owned row: no action, and no lifecycle controls at all", async () => {
    const { page } = await mountAndOpen({ sites: [PORTAL] }, "portal.memql.example.com");
    expect(headAction(page)).toBeNull();
    for (const label of [/Pause/, /Resume/, /Archive/, /Restore/, /Show the last/, /Roll back/]) {
      expect(within(page).queryByRole("button", { name: label })).toBeNull();
    }
    expect(within(page).getByText(/one of the cluster's own surfaces/)).toBeTruthy();
  });

  it("an archived deployable: no action", async () => {
    const archived = siteRow({ ...STORE, id: "site-store", status: "archived" });
    const { page } = await mountAndOpen({ ...WITH_PACKAGE, sites: [archived, ADMIN] }, "store.memql.example.com");
    expect(headAction(page)).toBeNull();
  });

  it("a reader: no action, and no controls", async () => {
    const { page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com", { role: "reader" });
    expect(headAction(page)).toBeNull();
    expect(within(page).queryByRole("button", { name: /Pause/ })).toBeNull();
    expect(within(page).queryByRole("button", { name: /from a zip/ })).toBeNull();
  });

  it("carries the quiet Ask and Open beside it", async () => {
    const { page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com");
    const head = page.querySelector(".os-head") as HTMLElement;
    expect(within(head).getByRole("button", { name: "Ask about store.memql.example.com" })).toBeTruthy();
    const link = within(head).getByRole("link", { name: /Open/ });
    expect(link.getAttribute("href")).toBe("https://store.memql.example.com/");
    expect(link.getAttribute("rel")).toContain("noopener");
  });

  it("renders a deploy refusal beneath the Head, headline above and the server's sentence beneath", async () => {
    const { page } = await mountAndOpen(
      {
        ...WITH_PACKAGE,
        deployError:
          "dsl_requires_cluster_owner: this package ships MemQL DSL (acme), and deploying DSL changes what this whole cluster can do -- so it is reserved to a cluster owner.",
      },
      "store.memql.example.com",
    );
    await click(headAction(page));
    expect(await within(page).findByText("Deploying MemQL is a cluster owner's decision")).toBeTruthy();
    expect(within(page).getByText(/reserved to a cluster owner/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// A first publish
// ---------------------------------------------------------------------------

describe("a first publish", () => {
  it("ends on 'Published to ... Not serving yet.' with Make it live as the Head's, and the stop offers no second one", async () => {
    const draft = siteRow({ id: "site-docs", hostname: "docs.memql.example.com", kind: "static", status: "draft", bundleRef: "blob://sites/site-docs/v1/" });
    const { page } = await mountAndOpen({ sites: [draft] }, "docs.memql.example.com");
    expect(within(page).getByText("Published to docs.memql.example.com. Not serving yet.")).toBeTruthy();
    expect(within(page).getAllByRole("button", { name: "Make it live" })).toHaveLength(1);
    const rail = within(page).getByRole("list", { name: "Deployable stops" });
    expect(rail.querySelector('li[data-state="open"]')).not.toBeNull();
  });

  it("a placeholder is not a publish: the source is waiting for the first push", async () => {
    const pending = siteRow({ id: "site-ci", hostname: "ci.memql.example.com", kind: "static", status: "draft", bundleRef: "blob://sites/site-ci/pending/" });
    const { page } = await mountAndOpen({ sites: [pending] }, "ci.memql.example.com");
    expect(headAction(page)).toBeNull();
    expect(within(page).getByText("waiting for the first push")).toBeTruthy();
    expect(within(page).getByText("Nothing published yet.")).toBeTruthy();
    expect(within(page).getByText(/POST \/sites\/site-ci\/bundles/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The Source stop
// ---------------------------------------------------------------------------

describe("the Source stop", () => {
  it("shows a hand-made site's bundle, its provenance and what that means", async () => {
    const { connection, page } = await mountAndOpen({ sites: [SHOP] }, "shop.memql.example.com");
    expect(within(page).getByText("blob://sites/site-shop/v1/")).toBeTruthy();
    expect(within(page).getByText("uploaded bundle")).toBeTruthy();
    expect(within(page).getByText("artifact-zip")).toBeTruthy();
    expect(within(page).getByText("Published from the Library.")).toBeTruthy();
    // The storefront's binding names the secret and NEVER fetches its value.
    expect(within(page).getByText("example.myshopify.com")).toBeTruthy();
    expect(within(page).getByText("shopify-storefront-token")).toBeTruthy();
    expect(connection.calls.some((c) => c.toLowerCase().includes("secret"))).toBe(false);
  });

  it("says a CI-pushed bundle was pushed by your CI", async () => {
    const pushed = siteRow({ id: "site-ci", hostname: "ci.memql.example.com", kind: "static", status: "live", bundleRef: "blob://sites/site-ci/v3/", artifactId: "" });
    const { page } = await mountAndOpen({ sites: [pushed] }, "ci.memql.example.com");
    expect(within(page).getByText(/Pushed by your CI/)).toBeTruthy();
    expect(within(page).queryByText("Published from the Library.")).toBeNull();
  });

  it("shows a package-produced site's source as facts", async () => {
    const { page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com");
    expect(within(page).getByText("acme/storefront at main")).toBeTruthy();
    expect(within(page).getByText("Tracking")).toBeTruthy();
    expect(within(page).getByText("Deployed")).toBeTruthy();
    expect(within(page).getAllByText("aaaaaaa").length).toBeGreaterThan(0);
  });

  describe("the credential chip", () => {
    const PRIVATE = { ...ACME, credentialId: "cred-1" };

    it("renders the label and the fingerprint, and never anything token-shaped", async () => {
      const card = credentialRow({ id: "cred-1", token: "ghp_should_never_arrive" });
      const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, packages: [PRIVATE], credentials: [card] }, "store.memql.example.com");
      expect(await within(page).findByText("acme deploy token")).toBeTruthy();
      expect(within(page).getByText("sha256:ab12cd34")).toBeTruthy();
      expect(page.textContent).not.toContain("ghp_");
      expect(within(page).queryByText("revoked")).toBeNull();
      // The credentials feed is the root's, read once, and never a value.
      expect(connection.calls.filter((c) => c === "query sourceCredentialsMine()")).toHaveLength(1);
      expect(connection.calls.some((c) => c.includes("token"))).toBe(false);
    });

    it("says revoked, in the warn tone, when the card is", async () => {
      const card = credentialRow({ id: "cred-1", status: "revoked", revokedAt: "2026-09-01T00:00:00Z" });
      const { page } = await mountAndOpen({ ...WITH_PACKAGE, packages: [PRIVATE], credentials: [card] }, "store.memql.example.com");
      const revoked = await within(page).findByText("revoked");
      expect(revoked.getAttribute("data-tone")).toBe("warn");
    });

    it("says public for a repository with no credential", async () => {
      const { page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com");
      expect(within(page).getByText("public")).toBeTruthy();
    });

    it("says 'a credential you cannot see' for an id that resolves to no card", async () => {
      const { page } = await mountAndOpen({ ...WITH_PACKAGE, packages: [PRIVATE], credentials: [] }, "store.memql.example.com");
      expect(await within(page).findByText("a credential you cannot see")).toBeTruthy();
      expect(within(page).queryByText("cred-1")).toBeNull();
    });

    it("switches which credential the source fetches under, for a writer", async () => {
      // ROTATION IS THIS CONTROL: a credential's value is sealed once and
      // never replaced, so rotating one means adding another and pointing
      // the source at it. The picker does both in one place.
      const card = credentialRow({ id: "cred-1" });
      const other = credentialRow({ id: "cred-2", label: "new laptop", fingerprint: "sha256:9f2c" });
      const { connection, page } = await mountAndOpen(
        { ...WITH_PACKAGE, packages: [PRIVATE], credentials: [card, other] },
        "store.memql.example.com",
      );
      const save = within(page).getByRole("button", { name: "Save" }) as HTMLButtonElement;
      // Nothing has changed yet, so there is nothing to save.
      expect(save.disabled).toBe(true);

      await click(within(page).getByLabelText("The credential this source is fetched under, on github.com"));
      await click(await screen.findByRole("option", { name: /new laptop/ }));
      expect(save.disabled).toBe(false);
      await click(save);

      expect(connection.callsNamed("updatePackageSource")).toEqual([
        'mutation updatePackageSource(packageId: "pkg-acme", credentialId: "cred-2")',
      ]);
    });

    it("offers a reader no switch at all, disabled or otherwise", async () => {
      const card = credentialRow({ id: "cred-1" });
      const { page } = await mountAndOpen(
        { ...WITH_PACKAGE, packages: [PRIVATE], credentials: [card] },
        "store.memql.example.com",
        { role: "reader" },
      );
      // The chip still says which credential is in force -- reading is not
      // the privileged half.
      expect(await within(page).findByText("acme deploy token")).toBeTruthy();
      expect(within(page).queryByLabelText("The credential this source is fetched under, on github.com")).toBeNull();
    });

    it("follows a revocation live, because the feed broadcasts updates", async () => {
      const card = credentialRow({ id: "cred-1" });
      const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, packages: [PRIVATE], credentials: [card] }, "store.memql.example.com");
      await within(page).findByText("acme deploy token");
      await emit(connection, "v1:platform:sourceCredential", credentialRow({ id: "cred-1", status: "revoked" }));
      expect(await within(page).findByText("revoked")).toBeTruthy();
    });
  });

  describe("the source's own lifecycle", () => {
    it("archives the source and every app it produced, after the typed name, and names the apps", async () => {
      const { connection, page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com");
      await click(within(page).getByRole("button", { name: "Archive this source and every app it produced" }));
      // The confirmation names what "every app" means.
      expect(within(page).getByText("admin.memql.example.com", { exact: false })).toBeTruthy();
      const archive = within(page).getByRole("button", { name: "Archive" }) as HTMLButtonElement;
      expect(archive.disabled).toBe(true);
      await typeInto(within(page).getByLabelText("Type acme to confirm") as HTMLInputElement, "acme");
      expect(archive.disabled).toBe(false);
      await click(archive);
      expect(connection.callsNamed("packageArchive")).toEqual(['builtin packageArchive(packageId: "pkg-acme", confirmName: "acme")']);
    });

    it("renders the server's refusal in place when apps are still serving", async () => {
      const { page } = await mountAndOpen(
        { ...WITH_PACKAGE, archiveError: "package_has_active_deployables: storefront (store.memql.example.com) and admin (admin.memql.example.com) are still serving; archive them first" },
        "store.memql.example.com",
      );
      await click(within(page).getByRole("button", { name: "Archive this source and every app it produced" }));
      await typeInto(within(page).getByLabelText("Type acme to confirm") as HTMLInputElement, "acme");
      await click(within(page).getByRole("button", { name: "Archive" }));
      expect(await within(page).findByText("This package still has sites that are serving")).toBeTruthy();
      expect(within(page).getByText(/archive them first/)).toBeTruthy();
    });

    it("restores an archived source", async () => {
      const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, packages: [{ ...ACME, status: "archived" }] }, "store.memql.example.com");
      await click(within(page).getByRole("button", { name: "Restore this source" }));
      expect(connection.callsNamed("packageRestore")).toEqual(['builtin packageRestore(packageId: "pkg-acme")']);
    });
  });
});

// ---------------------------------------------------------------------------
// What it is, and Build
// ---------------------------------------------------------------------------

describe("What it is", () => {
  it("shows the newest run's report", async () => {
    const { page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com");
    const apps = within(page).getByText("Web apps").closest("section") as HTMLElement;
    expect(within(apps).getByText("storefront")).toBeTruthy();
    expect(within(apps).getByText("already built")).toBeTruthy();
  });

  it("says the analysis is reading the tree while a run is at analyzing, with no second spinner", async () => {
    const analyzing = run({ id: "dep-an", status: "analyzing", report: null, deployables: [], finishedAt: "", startedAt: "2026-09-01T14:00:00Z", createdAt: "2026-09-01T14:00:00Z" });
    const { page } = await mountAndOpen({ ...WITH_PACKAGE, deployments: { "pkg-acme": [analyzing, SUCCEEDED] } }, "store.memql.example.com");
    expect(within(page).getByText(/Reading the tree/)).toBeTruthy();
    expect(page.querySelectorAll("[aria-busy='true']")).toHaveLength(0);
  });

  it("says only what is known of a hand-made site", async () => {
    const { page } = await mountAndOpen({ sites: [SHOP] }, "shop.memql.example.com");
    expect(within(page).queryByText("Web apps")).toBeNull();
    expect(within(page).getByText("Shopify storefront")).toBeTruthy();
    expect(within(page).queryByText(/index\.html present/)).toBeNull();
  });
});

describe("Build", () => {
  it("renders the engine's needs-a-build refusal in place, verbatim, with the build output", async () => {
    const { page } = await mountAndOpen({ ...WITH_PACKAGE, deployments: { "pkg-acme": [REFUSED, SUCCEEDED] } }, "store.memql.example.com");
    const rail = within(page).getByRole("list", { name: "Deployable stops" });
    const build = [...rail.querySelectorAll(":scope > li")].find((li) => li.textContent?.includes("Build")) as HTMLElement;
    expect(build.getAttribute("data-state")).toBe("stopped");
    expect(within(build).getByText("The build did not finish")).toBeTruthy();
    expect(within(build).getAllByText(/npm run build exited 1 in clients\/web/).length).toBeGreaterThan(0);
    expect(within(build).getByText(/npm ERR! missing script: build/)).toBeTruthy();
    // ...and the site is still serving what it was serving (design H).
    const live = [...rail.querySelectorAll(":scope > li")].find((li) => li.textContent?.includes("Live at")) as HTMLElement;
    expect(live.getAttribute("data-state")).toBe("done");
  });

  it("a prebuilt app: skipped with the reason", async () => {
    const { page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com");
    expect(within(page).getByText("its built output is in the source")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Where it lives
// ---------------------------------------------------------------------------

describe("Where it lives", () => {
  it("shows the address as a link, and the client picker for everyone who can read the page", async () => {
    const { page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com", { role: "reader" });
    const address = within(page).getByRole("link", { name: "store.memql.example.com" });
    expect(address.getAttribute("href")).toBe("https://store.memql.example.com/");
    expect(within(page).getByLabelText("The client this deployable is for")).toBeTruthy();
  });

  it("writes the tie through updateSiteAccount and inserts nothing locally", async () => {
    // Tied to a client this reader cannot see: the picker keeps the id in
    // place, and choosing "No client" is a change. The write is the same
    // call the detail panel made; the Select is the kit's own, so the choice
    // goes through its listbox.
    const tied = siteRow({ ...STORE, id: "site-store", accountId: "acct-1" });
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, sites: [tied, ADMIN] }, "store.memql.example.com");
    expect(within(page).getByText("acct-1")).toBeTruthy();
    await click(within(page).getByLabelText("The client this deployable is for"));
    await click(await screen.findByRole("option", { name: "No client" }));
    await waitFor(() => expect(connection.callsNamed("updateSiteAccount")).toHaveLength(1));
    expect(connection.callsNamed("updateSiteAccount")[0]).toContain('siteId: "site-store"');
  });

  it("mounts the Domains content for a cluster owner only", async () => {
    const { page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com", { role: "admin" });
    expect(within(page).queryByText("Domains")).toBeNull();
    expect(within(page).queryByLabelText("Domain to bind")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The Live stop
// ---------------------------------------------------------------------------

describe("the Live stop", () => {
  it("says since when, walks the versions on demand, and rolls back to one", async () => {
    const history = [
      { ...STORE, bundleRef: "blob://sites/site-store/v2/", createdAt: "2026-09-01T12:01:00Z" },
      { ...STORE, bundleRef: "blob://sites/site-store/v1/", createdAt: "2026-08-20T12:00:00Z" },
    ];
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, siteHistory: history }, "store.memql.example.com");
    expect(within(page).getByText("Live since")).toBeTruthy();
    // Loaded on DEMAND: nothing walked until asked.
    expect(connection.callsNamed("siteById")).toHaveLength(0);
    await click(within(page).getByRole("button", { name: /Show the last 6/ }));
    expect(await within(page).findByText("serving now")).toBeTruthy();
    expect(within(page).getByText("v1")).toBeTruthy();
    expect(connection.calls.filter((c) => c.startsWith("asOf(siteById(")).length).toBeGreaterThan(0);

    await click(within(page).getByRole("button", { name: /Roll store\.memql\.example\.com back/ }));
    expect(connection.callsNamed("updateSiteBundle")).toEqual([
      'mutation updateSiteBundle(siteId: "site-store", bundleRef: "blob://sites/site-store/v1/")',
    ]);
  });

  it("pauses a live site with the 503-versus-404 sentence, and refuses to archive it until paused", async () => {
    const { connection, page } = await mountAndOpen(WITH_PACKAGE, "store.memql.example.com");
    expect(within(page).getAllByText(/503 rather than 404/).length).toBeGreaterThan(0);
    const archive = within(page).getByRole("button", { name: "Archive this deployable" }) as HTMLButtonElement;
    expect(archive.disabled).toBe(true);
    expect(within(page).getByText(/Pause it first/)).toBeTruthy();
    await click(within(page).getByRole("button", { name: "Pause store.memql.example.com" }));
    expect(connection.callsNamed("updateSiteStatus")).toEqual(['mutation updateSiteStatus(siteId: "site-store", status: "disabled")']);
  });

  it("resumes a paused site from the stop, not the Head", async () => {
    const paused = siteRow({ ...STORE, id: "site-store", status: "disabled" });
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, sites: [paused, ADMIN] }, "store.memql.example.com");
    expect(headAction(page)?.textContent).toBe("Redeploy");
    await click(within(page).getByRole("button", { name: "Resume store.memql.example.com" }));
    expect(connection.callsNamed("updateSiteStatus")).toEqual(['mutation updateSiteStatus(siteId: "site-store", status: "live")']);
  });

  it("archives a paused site after the typed hostname", async () => {
    const paused = siteRow({ ...STORE, id: "site-store", status: "disabled" });
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, sites: [paused, ADMIN] }, "store.memql.example.com");
    await click(within(page).getByRole("button", { name: "Archive this deployable" }));
    const archive = within(page).getByRole("button", { name: "Archive" }) as HTMLButtonElement;
    expect(archive.disabled).toBe(true);
    await typeInto(within(page).getByLabelText("Type store.memql.example.com to confirm") as HTMLInputElement, "store.memql.example.com");
    expect(archive.disabled).toBe(false);
    await click(archive);
    expect(connection.callsNamed("siteArchive")).toEqual(['builtin siteArchive(siteId: "site-store", confirmHostname: "store.memql.example.com")']);
  });

  it("restores an archived site, paused", async () => {
    const archived = siteRow({ ...STORE, id: "site-store", status: "archived" });
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, sites: [archived, ADMIN] }, "store.memql.example.com");
    await click(within(page).getByRole("button", { name: "Restore, paused" }));
    expect(connection.callsNamed("siteRestore")).toEqual(['builtin siteRestore(siteId: "site-store")']);
  });

  it("renders every lifecycle refusal in place, in the server's words", async () => {
    const refusal = "site_status_guard: store.memql.example.com is system-owned and cannot be paused";
    const { page } = await mountAndOpen({ ...WITH_PACKAGE, siteStatusError: refusal }, "store.memql.example.com");
    await click(within(page).getByRole("button", { name: "Pause store.memql.example.com" }));
    expect(await within(page).findByText(/cannot be paused/)).toBeTruthy();
    expect(within(page).getByText("This cluster refused")).toBeTruthy();
  });

  it("renders a Make-it-live refusal on the page too", async () => {
    const draft = siteRow({ id: "site-docs", hostname: "docs.memql.example.com", kind: "static", status: "draft", bundleRef: "blob://sites/site-docs/v1/" });
    const { page } = await mountAndOpen({ sites: [draft], siteStatusError: "updateSiteStatus: refused by the status guard" }, "docs.memql.example.com");
    await click(headAction(page));
    expect(await within(page).findByText(/refused by the status guard/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Every attempt
// ---------------------------------------------------------------------------

describe("Every attempt", () => {
  it("lists every run with its own rail, and rolls back to a succeeded one that is not the latest", async () => {
    const older = run({ id: "dep-older", sourceVersion: "bbbbbbbbbbbbbbbbbbbb", startedAt: "2026-08-20T12:00:00Z", createdAt: "2026-08-20T12:00:00Z" });
    const { connection, page } = await mountAndOpen({ ...WITH_PACKAGE, deployments: { "pkg-acme": [SUCCEEDED, older] } }, "store.memql.example.com");
    const attempts = within(page).getByRole("list", { name: "Deployments of acme" });
    expect(within(attempts).getAllByRole("list", { name: "Deploy stages" })).toHaveLength(2);
    // Only the older succeeded run offers a roll back.
    const rollbacks = within(attempts).getAllByRole("button", { name: /Roll back to/ });
    expect(rollbacks).toHaveLength(1);
    await click(rollbacks[0]);
    expect(connection.callsNamed("packageRollback")).toEqual(['builtin packageRollback(packageId: "pkg-acme", deploymentId: "dep-older")']);
  });

  it("lists a parked run as waiting for you", async () => {
    const { page } = await mountAndOpen({ ...WITH_PACKAGE, deployments: { "pkg-acme": [PARKED, SUCCEEDED] } }, "store.memql.example.com");
    expect(within(page).getByText("waiting for you")).toBeTruthy();
  });

  it("a hand-made site has no attempts, and says so in one line", async () => {
    const { page } = await mountAndOpen({ sites: [SHOP] }, "shop.memql.example.com");
    expect(within(page).getByText(/has no attempts/)).toBeTruthy();
    expect(within(page).queryByRole("list", { name: /Deployments of/ })).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The retained-feed contract
// ---------------------------------------------------------------------------

describe("the timeline is retained by the page, never the root", () => {
  it("issues packageDeployments only once the page mounts", async () => {
    const connection = fakeConnection(WITH_PACKAGE);
    mount(connection);
    await screen.findByText("store.memql.example.com");
    expect(connection.callsNamed("packageDeployments")).toHaveLength(0);
    await open("store.memql.example.com");
    await waitFor(() => expect(connection.callsNamed("packageDeployments")).toHaveLength(1));
    expect(connection.callsNamed("packageDeployments")[0]).toContain('packageId: "pkg-acme"');
  });

  it("reads no timeline at all for a hand-made site", async () => {
    const { connection } = await mountAndOpen({ sites: [SHOP] }, "shop.memql.example.com");
    expect(connection.callsNamed("packageDeployments")).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// The bundleRef flip marker
// ---------------------------------------------------------------------------

describe("the bundle flip marker", () => {
  it("marks a bundle that flipped while the page was open", async () => {
    const { connection, page } = await mountAndOpen({ sites: [SHOP] }, "shop.memql.example.com");
    expect(within(page).queryByText("changed just now")).toBeNull();
    await emit(connection, SITE_CONCEPT, { ...SHOP, bundleRef: "blob://sites/site-shop/v9/" });
    expect(await screen.findByText("changed just now")).toBeTruthy();
    expect(screen.getByText("blob://sites/site-shop/v9/")).toBeTruthy();
  });

  it("does NOT mark a change that left the bundle alone", async () => {
    const { connection, page } = await mountAndOpen({ sites: [SHOP] }, "shop.memql.example.com");
    await emit(connection, SITE_CONCEPT, { ...SHOP, title: "Renamed" });
    await waitFor(() => expect(screen.queryByText("changed just now")).toBeNull());
    // The reachable positive, scoped to the PAGE: the rename really did
    // arrive. It also renames the list row behind the page, which is why the
    // query is scoped rather than global.
    expect(within(page).getByText("Renamed")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Redeploying a hand-made site from a zip (the picker, folded into Source)
// ---------------------------------------------------------------------------

describe("redeploying from a zip", () => {
  async function openPicker(connection: FakeConnection, opts: { role?: string } = {}) {
    mount(connection, opts);
    const page = await open("shop.memql.example.com");
    await click(within(page).getByRole("button", { name: "Redeploy from a zip" }));
    return page;
  }

  it("is offered to an admin and not to a reader", async () => {
    const admin = await mountAndOpen({ sites: [SHOP] }, "shop.memql.example.com", { role: "admin" });
    expect(within(admin.page).getByRole("button", { name: "Redeploy from a zip" })).toBeTruthy();
  });

  it("offers only the zips, and reads the Library once", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP, PDF, NOTE] });
    await openPicker(connection);
    expect(await screen.findByText("storefront-build.zip")).toBeTruthy();
    expect(screen.queryByText("brief.pdf")).toBeNull();
    expect(screen.queryByText("Standup notes")).toBeNull();
    expect(connection.calls.filter((c) => c === "query libraryArtifacts()")).toHaveLength(1);
  });

  it("reads NOTHING from the Library until the picker is opened", async () => {
    const { connection } = await mountAndOpen({ sites: [SHOP], artifacts: [ZIP] }, "shop.memql.example.com");
    expect(connection.calls.filter((c) => c === "query libraryArtifacts()")).toHaveLength(0);
  });

  it("deploys the chosen bundle and summarises what the cluster did, in place", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP] });
    await openPicker(connection);
    await click(await screen.findByText("storefront-build.zip"));
    await click(screen.getByRole("button", { name: /^Deploy storefront-build\.zip$/ }));
    expect(connection.callsNamed("sitePublishFromArtifact")).toEqual([
      'builtin sitePublishFromArtifact(siteId: "site-shop", artifactId: "artifact-zip")',
    ]);
    expect(await screen.findByText(/Published version v7f3c19a2bb01 -- 12 files, 2\.0 MB\./)).toBeTruthy();
    expect(screen.getByText("blob://sites/site-shop/v7f3c19a2bb01/")).toBeTruthy();
  });

  it("turns a stable refusal reason into a sentence, and never prints the token", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      artifacts: [ZIP],
      publishError: "sitePublishFromArtifact refused: bundle_missing_index -- bundle for site-shop has no index.html at its root",
    });
    await openPicker(connection);
    await click(await screen.findByText("storefront-build.zip"));
    await click(screen.getByRole("button", { name: /^Deploy storefront-build\.zip$/ }));
    expect(await screen.findByText(/no index\.html at its top level/)).toBeTruthy();
    expect(screen.getByText(/still serving what it was serving/)).toBeTruthy();
    expect(screen.queryByText(/bundle_missing_index/)).toBeNull();
  });

  it("does NOT retry, and clears the refusal only when a different bundle is chosen", async () => {
    const zip2 = { ...ZIP, id: "artifact-zip-2", title: "second.zip" };
    const connection = fakeConnection({
      sites: [SHOP],
      artifacts: [ZIP, zip2],
      publishError: "sitePublishFromArtifact refused: artifact_not_a_zip -- not a zip",
    });
    await openPicker(connection);
    await click(await screen.findByText("storefront-build.zip"));
    await click(screen.getByRole("button", { name: /^Deploy storefront-build\.zip$/ }));
    await screen.findByText(/has to be a zip/);
    expect(connection.callsNamed("sitePublishFromArtifact")).toHaveLength(1);
    await click(screen.getByText("second.zip"));
    await waitFor(() => expect(screen.queryByText(/has to be a zip/)).toBeNull());
    expect(connection.callsNamed("sitePublishFromArtifact")).toHaveLength(1);
  });

  it("keeps an UNKNOWN failure's own message -- inventing a friendly one hides a fault", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP], publishError: "stream closed before a reply arrived" });
    await openPicker(connection);
    await click(await screen.findByText("storefront-build.zip"));
    await click(screen.getByRole("button", { name: /^Deploy storefront-build\.zip$/ }));
    expect(await screen.findByText("stream closed before a reply arrived")).toBeTruthy();
  });

  it("says there may be older bundles when the LIBRARY page was full, not when the zip list is", async () => {
    const filler = Array.from({ length: 48 }, (_, i) => ({ ...PDF, id: `pdf-${i}` }));
    const connection = fakeConnection({
      sites: [SHOP],
      artifacts: [ZIP, { ...ZIP, id: "artifact-zip-2", title: "second.zip" }, ...filler],
    });
    await openPicker(connection);
    await screen.findByText("storefront-build.zip");
    expect(screen.getByText(/50 most recent Library entries/)).toBeTruthy();
    expect(screen.getByText("second.zip")).toBeTruthy();
    expect(screen.queryByText("brief.pdf")).toBeNull();
  });

  it("stays quiet when the Library page was not full", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP, PDF] });
    await openPicker(connection);
    await screen.findByText("storefront-build.zip");
    expect(screen.queryByText(/most recent Library entries/)).toBeNull();
  });

  it("will not deploy before a bundle is chosen", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP] });
    await openPicker(connection);
    const button = (await screen.findByRole("button", { name: "Pick a bundle" })) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
  });

  it("calls a never-published site's picker Deploy, not Redeploy", async () => {
    const pending = siteRow({ id: "site-ci", hostname: "ci.memql.example.com", kind: "static", status: "draft", bundleRef: "blob://sites/site-ci/pending/" });
    const { page } = await mountAndOpen({ sites: [pending] }, "ci.memql.example.com");
    expect(within(page).getByRole("button", { name: "Deploy from a zip" })).toBeTruthy();
  });
});

describe("what the page does not do", () => {
  it("mounts no toast container and no dialog anywhere", async () => {
    const connection = fakeConnection(WITH_PACKAGE);
    const { container } = mount(connection);
    await open("store.memql.example.com");
    expect(container.querySelector("[data-toast], .os-toast, dialog, [role='dialog']")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The credential card: what a person sees of a token, and never the token
// ---------------------------------------------------------------------------

const CARD: Row = {
  id: "cred-1",
  ownerUserId: "u-me",
  host: "github.com",
  label: "acme deploy token",
  fingerprint: "sha256:ab12cd34",
  status: "active",
  lastUsedAt: "2026-09-01T12:00:00Z",
  revokedAt: "",
  createdAt: "2026-08-20T00:00:00Z",
};

describe("the credential card", () => {
  it("projects the card fields and nothing that could be a value", () => {
    const card = credentialFromRow(CARD);
    expect(card).toEqual({
      id: "cred-1",
      ownerUserId: "u-me",
      host: "github.com",
      label: "acme deploy token",
      fingerprint: "sha256:ab12cd34",
      status: "active",
      // A row written before memql#4915 carries no `kind` at all, and it
      // reads as the pasted kind rather than as a third state. Three states
      // where the model has two is how every pre-epic credential would come
      // to be listed under neither heading in the Sources group.
      kind: "token",
      login: "",
      installationIds: [],
      lastUsedAt: "2026-09-01T12:00:00Z",
      revokedAt: "",
      createdAt: "2026-08-20T00:00:00Z",
    });
    // The projection has no home for a token. A row that carried one -- it
    // never should -- would be dropped here rather than reaching a chip.
    expect(Object.keys(credentialFromRow({ ...CARD, token: "ghp_should_never_arrive" }))).not.toContain("token");
  });

  it("reads a subscription envelope the same as a seed row", () => {
    const folded = credentialFromRow({
      id: "cred-1",
      createdAt: "2026-08-20T00:00:00Z",
      payload: { ...CARD, id: "payload-id-that-must-not-win" },
    });
    expect(folded.id).toBe("cred-1");
    expect(folded.label).toBe("acme deploy token");
    expect(folded.status).toBe("active");
  });

  describe("what counts as news on a credential", () => {
    // Both directions, pinned. Anything named in a fingerprint announces
    // itself, so a liveness field turns the list into a strobe; a fingerprint
    // that misses a real change makes it go quiet exactly when somebody
    // needed telling.
    const base = credentialFromRow(CARD);

    it("fires on a rename, a revocation, a host change or a rotated fingerprint", () => {
      for (const change of [
        { label: "renamed" },
        { status: "revoked" },
        { host: "gitlab.com" },
        { fingerprint: "sha256:ff00ff00" },
      ]) {
        expect(credentialFingerprint({ ...base, ...change })).not.toBe(credentialFingerprint(base));
      }
    });

    it("fires when a grant reaches a new organisation, or is remade as somebody else", () => {
      // The grant half (epic memql#4915). An installation webhook moving
      // `installationIds` IS what the connected-account card exists to show,
      // and a reconnect under a different GitHub account is a different
      // connection wearing the same row id.
      const grant = credentialFromRow({ ...CARD, kind: "github_app", login: "octocat", installationIds: ["i-1"] });
      for (const change of [
        { installationIds: ["i-1", "i-2"] },
        { installationIds: [] },
        { login: "someone-else" },
        { kind: "token" },
      ]) {
        expect(credentialFingerprint({ ...grant, ...change })).not.toBe(credentialFingerprint(grant));
      }
    });

    it("stays silent when the same installations come back in a different order", () => {
      // GitHub answers no ordering guarantee, so a re-read is free to hand
      // back the same set reversed. Ringing on that would announce a change
      // to a card nothing had happened to.
      const grant = credentialFromRow({
        ...CARD,
        kind: "github_app",
        login: "octocat",
        installationIds: ["i-1", "i-2"],
      });
      expect(credentialFingerprint({ ...grant, installationIds: ["i-2", "i-1"] })).toBe(
        credentialFingerprint(grant),
      );
    });

    it("stays SILENT on lastUsedAt -- a heartbeat is not news", () => {
      // `lastUsedAt` moves on every fetch of every source that uses the
      // credential. Naming it would ring the chip on a ten-minute poll cycle.
      expect(credentialFingerprint({ ...base, lastUsedAt: "2026-09-02T09:00:00Z" })).toBe(
        credentialFingerprint(base),
      );
      expect(credentialFingerprint({ ...base, revokedAt: "2026-09-02T09:00:00Z" })).toBe(
        credentialFingerprint(base),
      );
      expect(credentialFingerprint({ ...base, createdAt: "2027-01-01T00:00:00Z" })).toBe(
        credentialFingerprint(base),
      );
    });
  });
});
