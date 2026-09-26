import { AccountsApp } from "../src/apps/accounts/AccountsApp";
import { LocalAccountsSettingsStore } from "../src/apps/accounts/settings";
import { fakeConnection as accountConnection, accountRow, withSession as accountSession } from "../test/accounts/harness";
import { createRoot } from "react-dom/client";
import { useEffect, useRef, useState, type ReactNode } from "react";

import "../src/styles/index.css";
import { Panel } from "../src/kit";
import { setRoleLadder } from "../src/system/roles";
import { SEEDED_LADDER } from "../test/seededLadder";
import {
  DEV_STORE,
  DOCS,
  PLATFORM_SITE,
  SHOP,
  STORE,
  fakeConnection,
  githubGrantRow,
  sourceConnectionRow,
  probeReply,
  repositoriesReply,
  repositoryFixture,
  siteRow,
  storeHealthRow,
  domainStateRow,
  withSession,
  type FakeSeed,
} from "../test/deployables/harness";
import { installQaConnection } from "./connectionShim";
import { StorePanel } from "../src/apps/deployables/store/StorePanel";
import { DeployablesApp } from "../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../src/apps/deployables/settings";
import { PageNavigationProvider } from "../src/kit/pageNavigation";
import { TrailRow } from "../src/kit/TrailRow";
import { DeployablePage } from "../src/apps/deployables/page/DeployablePage";
import { ALL_PARTS, partsWithout } from "../src/apps/deployables/parts";
import {
  previewGrantRow,
  previewObservationRow,
  previewReadinessRow,
} from "../test/deployables/harness";
import { siteFromRow } from "../src/apps/deployables/rows";
import { MachineDetail } from "../src/apps/fleet/machines/MachineDetail";
import { ShareDialog } from "../src/apps/fleet/machines/ShareDialog";
import type { MachineWrites } from "../src/apps/fleet/machines/useMachineWrites";
import { machineFromRow } from "../src/apps/fleet/rows";
import { MachinesProvider } from "../src/live/machines";
import {
  fakeConnection as fleetConnection,
  machineRow as fleetMachineRow,
  shareDirectoryRow,
  withSession as fleetSession,
} from "../test/fleet/harness";
import { OriginsSection } from "../src/apps/cluster/origins/OriginsSection";
import { MeshSection } from "../src/apps/cluster/mesh/MeshSection";
import {
  clusterNodeRow,
  dataOriginRow as clusterOriginRow,
  fakeConnection as clusterConnection,
  syncStateRow as clusterSyncStateRow,
  withSession as clusterSession,
} from "../test/cluster/harness";

// The browser QA harness for the storefront's Store surface.
//
// clients/os/DESIGN.md: "the acceptance for any surface change under these
// rules is rendered screenshots, both modes, empty and populated -- not the
// diff." jsdom performs no layout, resolves no custom property and never puts
// a value beside its own label, so a suite can be entirely green over a
// surface whose columns collide, whose action bar is below the fold, or whose
// value begins with the verb its own label already supplied.
//
// IT MOUNTS THE REAL COMPONENTS OVER THE SUITE'S OWN FIXTURE CONNECTION.
// `test/deployables/harness.tsx` is imported rather than re-implemented, so a
// screenshot cannot disagree with what the tests assert. `vitest`'s `vi` is
// shimmed for the same reason -- a second copy of the fake is a second thing
// to keep in step.
//
// WHAT IT DOES NOT DO: it does not click. The Store pane is reached in the
// product by clicking the workspace's Store slot, which needs React's
// synthetic event system and therefore a real driver; here each view is
// rendered directly, in the wrapper the page gives it. So these captures judge
// LAYOUT, COLOUR, COPY and DENSITY, and say nothing about the navigation that
// reaches them -- which is what `test/deployables/store.test.tsx` is for.

// A MODULE THAT FAILS TO LOAD LEAVES A BLANK PAGE AND A SILENT CONSOLE, so
// the harness reports its own state in the TITLE -- which `--dump-dom` and a
// one-shot headless capture can both read without a debugger attached. The
// first failure here was `test/seededAccess.ts` reading the seeds with
// node:fs, which is an unresolved import rather than a thrown error and so
// reached no error handler at all.
document.title = "loaded";
window.addEventListener("error", (e) => { document.title = "ERR: " + e.message; });
window.addEventListener("unhandledrejection", (e) => { document.title = "REJ: " + String(e.reason); });

setRoleLadder(SEEDED_LADDER);

const BOUND: FakeSeed = {
  sites: [SHOP],
  stores: [STORE, DEV_STORE],
  storeHealth: [
    storeHealthRow({
      storeId: "store-example",
      domain: "example.myshopify.com",
      scopesMissing: ["read_inventory"],
      scopesNeeded: ["read_products", "read_orders", "read_inventory"],
      domains: [
        domainStateRow({ concept: "v1:shopify:product", driftLast: 12, lagSeconds: 4, outboxDepth: 0, lastAppliedAt: "2026-09-20T18:40:00Z" }),
        domainStateRow({ concept: "v1:shopify:order", phase: "backfilling", driftLast: 3, lagSeconds: 61, outboxDepth: 7, lastAppliedAt: "2026-09-20T18:12:00Z" }),
        domainStateRow({ concept: "v1:shopify:customer", lastError: "shopify: 429 from the Admin API" }),
      ],
      // `costBucket` is a TOP-LEVEL key of the report and `subscriptions`
      // lives under `health` -- that is what the Go handler emits and what
      // readStoreHealth reads. Nesting the bucket rendered it as absent, which
      // is the honest answer to a fixture that never gave it one.
      costBucket: { currentlyAvailable: 1840, maximumAvailable: 2000, restoreRate: 100 },
      health: {
        subscriptions: { existing: 11, desired: 12, at: "2026-09-20T03:15:00Z", failed: ["orders/edited"] },
      },
    }),
  ],
};

/** Nothing measured yet: no bucket, no reconcile, no domain. */
const QUIET: FakeSeed = {
  sites: [SHOP],
  stores: [STORE],
  storeHealth: [storeHealthRow({ storeId: "store-example", domain: "example.myshopify.com" })],
};

const UNBOUND_SITE = siteRow({
  id: "site-unbound",
  hostname: "new.memql.example.com",
  kind: "shopify_storefront",
  status: "draft",
  bundleRef: "blob://sites/site-unbound/pending/",
  binding: {},
});

const UNBOUND: FakeSeed = { sites: [UNBOUND_SITE], stores: [STORE, DEV_STORE] };

// ---------------------------------------------------------------------------
// THE TWO LISTS (Deployables and Sources), populated
// ---------------------------------------------------------------------------
//
// A cluster somebody develops on has two built-in deployables and no source,
// so neither list has ever been SEEN with the rows it was designed for: an
// origin on a row, "3 apps, 2 deployed", Review needed beside Update available.
// These seeds are those rows. One of every origin a deployable can have, and
// one of every state word a source can carry.

function packageRow(over: Record<string, unknown> & { id: string; name: string }) {
  return {
    ownerUserId: "u-me",
    sourceKind: "repo",
    repoUrl: `https://github.com/acme/${over.name}`,
    repoRef: "main",
    credentialId: "",
    artifactId: "",
    deployedVersion: "aaaaaaaaaaaaaaaaaaaa",
    latestKnownVersion: "aaaaaaaaaaaaaaaaaaaa",
    updateAvailable: false,
    status: "active",
    createdAt: "2026-09-01T10:00:00Z",
    ...over,
  };
}

const ACME = packageRow({
  id: "pkg-acme",
  name: "acme",
  repoUrl: "https://github.com/acme/storefront",
  declares: [{ name: "storefront", kind: "spa" }, { name: "admin", kind: "spa" }, { name: "reports", kind: "static" }],
});
const WIDGETS = packageRow({ id: "pkg-widgets", name: "widgets-co", repoUrl: "https://github.com/acme/widgets", updateAvailable: true, latestKnownVersion: "bbbbbbbbbbbbbbbbbbbb" });
const BROCHURE = packageRow({ id: "pkg-brochure", name: "", sourceKind: "artifact", repoUrl: "", repoRef: "", artifactId: "artifact-zip" });
const LEGACY = packageRow({ id: "pkg-legacy", name: "legacy-portal", repoUrl: "https://github.com/acme/legacy-portal", status: "archived" });
const FRESH = packageRow({ id: "pkg-fresh", name: "field-notes", repoUrl: "https://github.com/acme/field-notes", deployedVersion: "" });

const LIST_SITES = [
  siteRow({ id: "site-store", hostname: "store.memql.example.com", bundleRef: "blob://sites/site-store/v2/", packageId: "pkg-acme", packageDeployableName: "storefront" }),
  siteRow({ id: "site-admin", hostname: "admin.memql.example.com", status: "disabled", bundleRef: "blob://sites/site-admin/v2/", packageId: "pkg-acme", packageDeployableName: "admin" }),
  siteRow({ id: "site-widget", hostname: "widgets.memql.example.com", packageId: "pkg-widgets", packageDeployableName: "widgets" }),
  siteRow({ id: "site-brochure", hostname: "brochure.memql.example.com", kind: "static", bundleRef: "blob://sites/site-brochure/v1/", packageId: "pkg-brochure", packageDeployableName: "brochure" }),
  siteRow({ id: "site-marketing", hostname: "marketing.memql.example.com", kind: "static", status: "draft", bundleRef: "blob://sites/site-marketing/pending/", title: "Marketing" }),
  SHOP,
  DOCS,
  PLATFORM_SITE,
];

const PARKED = {
  id: "dep-parked",
  packageId: "pkg-acme",
  sourceVersion: "cccccccccccccccccccc",
  status: "awaiting_confirm",
  report: {
    name: "acme",
    formatVersion: 1,
    deployables: [
      { name: "storefront", kind: "spa", path: "clients/web", buildPlan: "already built: dist", output: "dist", prebuilt: true },
      { name: "reports", kind: "static", path: "clients/reports", buildPlan: "already built: out", output: "out", prebuilt: true },
    ],
    dslDomains: [],
    problems: [],
    ok: true,
  },
  dslVersion: "",
  deployables: [],
  snapshotArtifactId: "",
  buildLogTail: "",
  error: null,
  requestedBy: "u-me",
  startedAt: "2026-09-01T13:00:00Z",
  finishedAt: "",
  createdAt: "2026-09-01T13:00:00Z",
};

const LISTS: FakeSeed = {
  sites: LIST_SITES,
  packages: [ACME, WIDGETS, BROCHURE, LEGACY, FRESH] as never,
  awaitingConfirm: [PARKED] as never,
};

/** A GitHub account already connected, so the Repository step is its picker. */
const CONNECTED: FakeSeed = {
  ...LISTS,
  credentials: [githubGrantRow({ id: "cred-grant" })],
  // The app's installation page, which is what puts "Install on another
  // organization" under the picker's groups.
  installUrl: "https://github.com/apps/memql/installations/new",
  repositories: repositoriesReply({
    repositories: [
      repositoryFixture({ fullName: "acme/storefront", private: true, visibility: "private" }),
      repositoryFixture({ fullName: "acme/widgets" }),
      repositoryFixture({ fullName: "acme/field-notes" }),
      repositoryFixture({ fullName: "octocat/dotfiles", installationId: "i-octocat" }),
    ],
  }),
};

const SOURCE_CHOOSER: FakeSeed = {
  ...CONNECTED,
  githubApp: { configured: true, installUrl: "https://github.com/apps/memql/installations/new" },
  accounts: [{ id: "client", name: "Client account", status: "active" }, { id: "self", name: "Operator organization", status: "active" }],
  credentials: [githubGrantRow({ id: "cred-grant", login: "octocat" }), githubGrantRow({ id: "cred-work", login: "workcat" })],
  sourceConnections: [sourceConnectionRow({ credentialId: "cred-grant" }), sourceConnectionRow({ id: "source-personal", credentialId: "cred-grant", installationId: "i-octocat", accountLogin: "octocat", accountType: "User" }), sourceConnectionRow({ id: "source-work", credentialId: "cred-work", installationId: "i-studio", accountLogin: "studio" })],
  sourceInstallations: {
    "cred-grant": { reason: "ok", installations: [{ id: "i-acme", account: "acme", accountType: "Organization" }, { id: "i-octocat", account: "octocat", accountType: "User" }], pending: [] },
    "cred-work": { reason: "ok", installations: [{ id: "i-studio", account: "studio", accountType: "Organization" }], pending: [{ login: "partner" }] },
  },
  repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/storefront" }), repositoryFixture({ fullName: "acme/field-notes" }), repositoryFixture({ fullName: "octocat/dotfiles", installationId: "i-octocat" }), repositoryFixture({ fullName: "studio/portal", installationId: "i-studio" })] }),
  packages: [ACME, WIDGETS, FRESH].map(p => ({ ...p, accountId: "self" })),
  sourceProbe: { "": probeReply({ branches: ["main", "release"] }) },
};

// Exercise the production wizard while the source read is in flight; these
// fixture requests never contact GitHub or create a real deployment.
function analysisConnection(result: "pending" | "failed" | "review") {
  const active = { ...PARKED, id: "dep-new", status: "analyzing", report: null,
    startedAt: new Date(Date.now() - 116000).toISOString() };
  const seed: FakeSeed = { ...SOURCE_CHOOSER, packages: [{ ...ACME, declares: [] }], sites: [],
    awaitingConfirm: [active], deployments: { "pkg-acme": [active] } };
  const connection = fakeConnection(seed);
  const execute = connection.query.executeNamed.bind(connection.query);
  let scheduled = false;
  function finish(cancelled = false) {
    const next = cancelled ? { ...active, status: "cancelled", error: { code: "deployment_cancelled", message: "You stopped this analysis. Nothing was deployed." } }
      : result === "failed" ? { ...active, status: "failed", error: { code: "deploy_failed", message: "Source download timed out. Nothing was deployed." } }
      : { ...PARKED, id: "dep-new" };
    seed.deployments!["pkg-acme"] = [next];
    seed.awaitingConfirm = next.status === "awaiting_confirm" ? [next] : [];
    connection.subscriptions.emit("v1:platform:packageDeployment", next);
  }
  connection.query.executeNamed = async (name, call, opts) => {
    if (name === "packageDeployments" && result !== "pending" && !scheduled) {
      scheduled = true;
      setTimeout(() => finish(), 2500);
    }
    if (name === "packageCancelDeployment") {
      setTimeout(() => finish(true), 1200);
    }
    return execute(name, call, opts);
  };
  return connection;
}

/** A cluster with NO GitHub App, seen by somebody who may register one. Press
 *  + and choose "A repository": the step asks the one question and the floor
 *  says Set up GitHub. */
const NO_APP_OWNER: FakeSeed = { ...LISTS, githubApp: { configured: false, canSetup: true } };

/** The same cluster, seen by somebody who may not. */
const NO_APP_MEMBER: FakeSeed = { ...LISTS, githubApp: { configured: false, canSetup: false } };

/** A cluster whose app was registered from the product, with an account
 *  connected through it: Settings > Sources ends in the GitHub App block. */
const APP_FROM_HERE: FakeSeed = {
  ...CONNECTED,
  githubApp: { configured: true, source: "cluster", slug: "memql-on-memql-example-com", canSetup: true },
};

function settingsStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

/**
 * THE WINDOW'S BODY, as `chrome/WindowFrame` composes it: the one trail row,
 * then the content. A list judged without the trail row above it is judged at
 * the wrong height, and a wizard judged outside a bounded body has no floor --
 * its action bar is only at the bottom of something that has a bottom.
 */
function WindowBody({ fallback, children }: { fallback: string; children: ReactNode }) {
  const content = useRef<HTMLDivElement>(null);
  return (
    <div className="os-window" style={{ position: "fixed", inset: 0, display: "flex", flexDirection: "column" }}>
      <div className="os-window-body">
        <PageNavigationProvider root={content} trail={[]}>
          <TrailRow fallback={fallback} />
          <div ref={content} className="os-window-content" data-os-window-content>
            {children}
          </div>
        </PageNavigationProvider>
      </div>
    </div>
  );
}

function Lists({ section }: { section: "deployables" | "sources" | "repositories" | "settings" }) {
  return (
    <WindowBody fallback={section === "sources" ? "Sources" : section === "repositories" ? "Repositories" : section === "settings" ? "Settings" : "Deployables"}>
      <DeployablesApp sectionId={section} navigate={() => {}} askContext={() => {}} store={settingsStore()} />
    </WindowBody>
  );
}

/** The pane wrapper `DeployablePage` gives the Store view, verbatim. */
function StorePane({ site, canBind }: { site: ReturnType<typeof siteFromRow>; canBind: boolean }) {
  return (
    <div className="os-deploy-pane deployable-workspace">
      <div className="os-deploy-scroll">
        <Panel label={`Store for ${site.hostname}`}>
          <StorePanel
            site={site}
            canBind={canBind}
            trail={[{ label: "Deployables" }, { label: site.hostname }, { label: "Store" }]}
            back={{ label: site.hostname, onSelect: () => {} }}
          />
        </Panel>
      </div>
    </div>
  );
}

function Overview({ site }: { site: ReturnType<typeof siteFromRow> }) {
  return (
    <DeployablePage
      site={site}
      pkg={null}
      credentials={[]}
      viewerUserId="u-me"
      nameOf={() => ""}
      can={ALL_PARTS}
      clusterDomain="memql.example.com"
      onBack={() => {}}
      onOpenSource={() => {}}
      onOpenHistory={() => {}}
    />
  );
}

// ---------------------------------------------------------------------------
// THE PREVIEW SECTION (epic memql#5531)
// ---------------------------------------------------------------------------
// Four states, and the three that are not the happy path are the ones worth
// capturing: a deployable with nothing being exercised, one whose preview
// binding points at the store shoppers reach (the refusal), and one whose
// checks have been run with only some of the four answering. jsdom can assert
// each of those sentences and cannot see that the lane's store sits under the
// version it belongs to rather than beside the other one's.

/** The storefront with a candidate and a development store attached. */
const SHOP_PREVIEWING = siteRow({
  ...SHOP,
  id: "site-shop",
  candidateRef: "blob://sites/site-shop/v2/",
  previewBinding: { storeId: "store-example-dev" },
} as never);

const READY_ROW = previewReadinessRow({
  siteId: "site-shop",
  hostname: "shop.memql.example.com",
  bundleRef: "blob://sites/site-shop/v1/",
  candidateRef: "blob://sites/site-shop/v2/",
  hasCandidate: true,
  storeId: "store-example",
  storeDomain: "example.myshopify.com",
  storeReadable: true,
  previewStoreId: "store-example-dev",
  previewStoreDomain: "example-dev.myshopify.com",
  canPreview: true,
  canPromote: true,
  canGoLive: true,
  previewRefusal: { code: "", message: "", remedy: "" },
  promoteRefusal: { code: "", message: "", remedy: "" },
} as never);

/** Exercised: three of the four answered, the fourth has not happened yet. */
const PREVIEW_MEASURED: FakeSeed = {
  ...BOUND,
  sites: [SHOP_PREVIEWING, DOCS, PLATFORM_SITE],
  previewReadiness: { "site-shop": READY_ROW },
  previewGrants: { "site-shop": [previewGrantRow({ id: "grant-1", lastSeenAt: new Date(Date.now() - 4 * 60_000).toISOString(), expiresAt: new Date(Date.now() + 22 * 60_000).toISOString() })] },
  previewObservations: {
    "site-shop": [
      previewObservationRow({ kind: "checkout_url", detail: "checkout is hosted at example-dev.myshopify.com; the payment walk itself happens in a browser and is not something this cluster can observe", durationMs: 812 }),
      previewObservationRow({ kind: "cart_accepted", detail: 'a cart accepted one line of "Canvas tote"', durationMs: 611 }),
      previewObservationRow({ kind: "catalog_read", detail: 'read product "canvas-tote"', durationMs: 338 }),
    ],
  },
};

/** Nothing exercised yet: a candidate, a development store, and no measurement. */
const PREVIEW_EMPTY: FakeSeed = {
  ...BOUND,
  sites: [SHOP_PREVIEWING, DOCS, PLATFORM_SITE],
  previewReadiness: { "site-shop": READY_ROW },
};

/** A step that was asked and did not answer, beside two that did. */
const PREVIEW_FAILING: FakeSeed = {
  ...PREVIEW_MEASURED,
  previewObservations: {
    "site-shop": [
      previewObservationRow({ kind: "cart_accepted", ok: false, failure: "the store refused the line: Not enough items available", durationMs: 604 }),
      previewObservationRow({ kind: "catalog_read", detail: 'read product "canvas-tote" (its first variant is not available for sale)', durationMs: 341 }),
    ],
  },
};

/** The refusal: a preview binding pointed at the store shoppers reach. */
const PREVIEW_REFUSED: FakeSeed = {
  ...BOUND,
  sites: [siteRow({ ...SHOP, id: "site-shop", candidateRef: "blob://sites/site-shop/v2/", previewBinding: { storeId: "store-example" } } as never), DOCS, PLATFORM_SITE],
  previewReadiness: {
    "site-shop": previewReadinessRow({
      siteId: "site-shop",
      hostname: "shop.memql.example.com",
      bundleRef: "blob://sites/site-shop/v1/",
      candidateRef: "blob://sites/site-shop/v2/",
      hasCandidate: true,
      storeId: "store-example",
      storeDomain: "example.myshopify.com",
      storeReadable: true,
      previewStoreId: "store-example",
      previewStoreDomain: "example.myshopify.com",
      canPreview: false,
      canPromote: true,
      canGoLive: true,
      previewRefusal: {
        code: "preview_binding_is_not_development_store",
        message: "the preview binding names example.myshopify.com, which is the store shoppers reach -- exercising a candidate against it would put test carts and test payments in the merchant's real store.",
        remedy: "Point the preview binding at a development store. Shopify marks one on the store row as isDevelopment.",
      },
      promoteRefusal: { code: "", message: "", remedy: "" },
    } as never),
  },
};

/** Nothing being exercised at all: the state most deployables live in. */
const PREVIEW_NONE: FakeSeed = {
  ...BOUND,
  previewReadiness: {
    "site-shop": previewReadinessRow({
      siteId: "site-shop",
      hostname: "shop.memql.example.com",
      bundleRef: "blob://sites/site-shop/v1/",
      storeId: "store-example",
      storeDomain: "example.myshopify.com",
      storeReadable: true,
      canPreview: true,
      previewRefusal: {code: "", message: "", remedy: ""},
    } as never),
  },
};

function PreviewPage({ site }: { site: ReturnType<typeof siteFromRow> }) {
  return (
    <DeployablePage
      site={site}
      pkg={null}
      credentials={[]}
      viewerUserId="u-me"
      nameOf={() => ""}
      can={ALL_PARTS}
      clusterDomain="memql.example.com"
      onBack={() => {}}
      onOpenSource={() => {}}
      onOpenHistory={() => {}}
    />
  );
}

// A view may bring its own CONNECTION and its own session wrapper. The Store
// and Deployables views share `test/deployables/harness`; the Data origins
// views need `test/cluster/harness`, whose fake answers `dataOrigins` and
// `syncStatesAll`. Two fixture harnesses rather than one widened one, for the
// reason the README gives about the fake in general: each is the SUITE's, so a
// screenshot cannot disagree with what those tests assert.
const VIEWS: Record<
  string,
  {
    seed?: FakeSeed;
    connect?: () => unknown;
    wrap?: (el: JSX.Element, role: string) => JSX.Element;
    role?: string;
    framed?: boolean;
    render: () => JSX.Element;
  }
> = {
  accounts: {
    connect: () => accountConnection({ clientAccountsAll: [
      accountRow({ id: "v1:accounts:account:self", name: "Our Studio", domain: "studio.example.com", primaryContactName: "Dana" }),
      accountRow({ id: "client-acme", name: "Acme Consulting", domain: "acme.example.com", primaryContactName: "Avery" }),
      accountRow({ id: "client-borden", name: "Borden Ltd", domain: "borden.example.com", primaryContactName: "Morgan" }),
    ] }),
    wrap: (el, role) => accountSession(el, { role }),
    render: () => <AccountsPane />,
  },
  "accounts-empty": {
    connect: () => accountConnection({ clientAccountsAll: [] }),
    wrap: (el, role) => accountSession(el, { role }),
    render: () => <AccountsPane />,
  },
  // The two lists. `framed` views bring their own window body, because the
  // app's wizard needs a floor and its pages publish to the window's trail.
  list: { seed: LISTS, framed: true, render: () => <Lists section="deployables" /> },
  sources: { seed: LISTS, framed: true, render: () => <Lists section="sources" /> },
  "list-empty": { seed: {}, framed: true, render: () => <Lists section="deployables" /> },
  "sources-empty": { seed: {}, framed: true, render: () => <Lists section="sources" /> },
  // The same app with a GitHub account connected: press + and choose
  // "A repository" and the Repository step is the picker, not the invitation.
  connected: { seed: CONNECTED, framed: true, render: () => <Lists section="deployables" /> },
  "guided-account-empty": { seed: { ...SOURCE_CHOOSER, credentials: [], sourceConnections: [] }, framed: true, render: () => <Lists section="deployables" /> },
  "guided-org-empty": { seed: { ...SOURCE_CHOOSER, sourceInstallations: { "cred-grant": { reason: "ok", installations: [], pending: [] } } }, framed: true, render: () => <Lists section="deployables" /> },
  "source-chooser": { seed: SOURCE_CHOOSER, framed: true, render: () => <Lists section="deployables" /> },
  "analysis-pending": { connect: () => analysisConnection("pending"), framed: true, render: () => <Lists section="deployables" /> },
  "analysis-failed": { connect: () => analysisConnection("failed"), framed: true, render: () => <Lists section="deployables" /> },
  "analysis-review": { connect: () => analysisConnection("review"), framed: true, render: () => <Lists section="deployables" /> },
  "source-settings": { seed: SOURCE_CHOOSER, framed: true, render: () => <Lists section="settings" /> },
  "source-management": { seed: { ...SOURCE_CHOOSER,
    sourceConnections: [...(SOURCE_CHOOSER.sourceConnections ?? []), sourceConnectionRow({ id: "source-work-acme", credentialId: "cred-work" })],
    packages: [
      { ...ACME, accountId: "self", credentialId: "cred-grant", sourceConnectionId: "source-acme" },
      { ...ACME, id: "pkg-acme-work", name: "acme work", accountId: "self", credentialId: "cred-work", sourceConnectionId: "source-work-acme" },
      { ...FRESH, accountId: "self", credentialId: "cred-grant", sourceConnectionId: "source-acme" },
      { ...WIDGETS, accountId: "self" },
    ],
  }, framed: true, render: () => <Lists section="sources" /> },
  "source-empty": { seed: { ...SOURCE_CHOOSER, packages: [], sites: [], awaitingConfirm: [], sourceConnections: [] }, framed: true, render: () => <Lists section="sources" /> },
  "connected-empty": { seed: { ...CONNECTED, repositories: repositoriesReply({ repositories: [], installations: [], pending: [] }) }, framed: true, render: () => <Lists section="deployables" /> },
  // The cluster's GitHub App, in each reading a surface has of it.
  "github-owner": { seed: NO_APP_OWNER, framed: true, render: () => <Lists section="deployables" /> },
  "github-member": { seed: NO_APP_MEMBER, role: "developer", framed: true, render: () => <Lists section="deployables" /> },
  "settings-no-app": { seed: NO_APP_OWNER, framed: true, render: () => <Lists section="settings" /> },
  "settings-no-app-member": { seed: NO_APP_MEMBER, role: "developer", framed: true, render: () => <Lists section="settings" /> },
  "settings-app": { seed: APP_FROM_HERE, framed: true, render: () => <Lists section="settings" /> },
  // The Fleet's machine detail. `fleet-healthy` is the control: nothing
  // should sit above the facts on a machine with nothing wrong with it.
  "fleet-healthy": {
    connect: () => fleetConnection({}),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <MachinePane over={{ credentialExpiresAt: "2026-12-20T00:00:00Z", clockSkewMs: 40 }} />,
  },
  "fleet-expiring": {
    connect: () => fleetConnection({}),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <MachinePane over={{ credentialExpiresAt: "2026-09-28T09:00:00Z", clockSkewMs: 40 }} />,
  },
  "fleet-expired": {
    connect: () => fleetConnection({}),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <MachinePane over={{ credentialExpiresAt: "2026-09-01T09:00:00Z" }} />,
  },
  // BOTH AT ONCE, which is the density question: two advisories plus the
  // facts, with the rename field above them.
  "fleet-skewed": {
    connect: () => fleetConnection({}),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <MachinePane over={{ credentialExpiresAt: "2026-09-26T09:00:00Z", clockSkewMs: -212000 }} />,
  },
  // A machine whose cockpit stamps no timestamps and whose token never
  // expires -- the pre-D4 fleet, where both new facts read as absences.
  "fleet-silent": {
    connect: () => fleetConnection({}),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <MachinePane over={{ credentialExpiresAt: "", rttAt: "", rttMs: 0 }} />,
  },
  // Sharing a machine (epic memql#5344). The panel in each reading of who can
  // use it, then the dialog. `sharing-people` is the one to read first: names,
  // both consents given, and the week's split ledger line.
  "sharing-owner": {
    connect: () => fleetConnection({}),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharingPane over={{ sharing: { mode: "owner" } }} />,
  },
  "sharing-people": {
    connect: () => fleetConnection({ fleetShareDirectory: [SHARE_DIRECTORY], fleetSharingLedger: [SHARE_LEDGER] }),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharingPane over={{ sharing: SHARED_WITH_PEOPLE, capabilityDescriptor: { inferenceServe: "cluster" } }} />,
  },
  // Lent by its owner, not yet agreed to by the machine: the one-line state is
  // muted and the cockpit's line says what to change and where.
  "sharing-waiting": {
    connect: () => fleetConnection({ fleetShareDirectory: [SHARE_DIRECTORY] }),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharingPane over={{ sharing: SHARED_WITH_PEOPLE, capabilityDescriptor: { inferenceServe: "owner" } }} />,
  },
  "sharing-everyone": {
    connect: () => fleetConnection({ fleetSharingLedger: [SHARE_LEDGER] }),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharingPane over={{ sharing: { mode: "cluster", sharedAt: "2026-09-20T10:00:00Z" }, capabilityDescriptor: { inferenceServe: "cluster" } }} />,
  },
  // Somebody else's machine, as a cluster owner sees it in the fleet: counts,
  // never names, and no act.
  "sharing-viewer": {
    connect: () => fleetConnection({}),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharingPane over={{ ownerUserId: "v1:identity:user:olivia", sharing: SHARED_WITH_PEOPLE, capabilityDescriptor: { inferenceServe: "cluster" } }} />,
  },
  // The dialog, opened on a people share with a stale subject on it.
  "share-dialog": {
    connect: () => fleetConnection({ fleetShareDirectory: [SHARE_DIRECTORY] }),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharePane over={{ sharing: SHARED_WITH_PEOPLE, capabilityDescriptor: { inferenceServe: "cluster" } }} />,
  },
  // An admin's directory: everyone, with the emails they already see in Users.
  "share-dialog-admin": {
    connect: () => fleetConnection({ fleetShareDirectory: [{ ...SHARE_DIRECTORY, everyone: true, people: SHARE_PEOPLE_ADMIN, current: { people: [], groups: [] } }] }),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharePane over={{ sharing: { mode: "owner" } }} startPeople />,
  },
  // A person in no group, below admin: nobody to pick, and the way out named.
  "share-dialog-empty": {
    connect: () => fleetConnection({ fleetShareDirectory: [shareDirectoryRow({ id: "v1:worker:registration:studio", machineId: "v1:worker:registration:studio" })] }),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharePane over={{ sharing: { mode: "owner" } }} startPeople />,
  },
  // The engine refused the save: the draft stays, and the engine's words.
  "share-dialog-refused": {
    connect: () => fleetConnection({ fleetShareDirectory: [SHARE_DIRECTORY] }),
    wrap: (el) => fleetSession(<MachinesProvider>{el}</MachinesProvider>),
    render: () => <SharePane over={{ sharing: SHARED_WITH_PEOPLE, capabilityDescriptor: { inferenceServe: "cluster" } }} refuse />,
  },
  overview: { seed: BOUND, render: () => <Overview site={siteFromRow(SHOP)} /> },
  // The preview section, in the five states worth judging as pixels.
  preview: { seed: PREVIEW_MEASURED, render: () => <PreviewPage site={siteFromRow(SHOP_PREVIEWING)} /> },
  "preview-empty": { seed: PREVIEW_EMPTY, render: () => <PreviewPage site={siteFromRow(SHOP_PREVIEWING)} /> },
  "preview-failing": { seed: PREVIEW_FAILING, render: () => <PreviewPage site={siteFromRow(SHOP_PREVIEWING)} /> },
  "preview-refused": { seed: PREVIEW_REFUSED, render: () => <PreviewPage site={siteFromRow(siteRow({ ...SHOP, id: "site-shop", candidateRef: "blob://sites/site-shop/v2/", previewBinding: { storeId: "store-example" } } as never))} /> },
  "preview-none": { seed: PREVIEW_NONE, render: () => <PreviewPage site={siteFromRow(SHOP)} /> },
  store: { seed: BOUND, render: () => <StorePane site={siteFromRow(SHOP)} canBind /> },
  quiet: { seed: QUIET, render: () => <StorePane site={siteFromRow(SHOP)} canBind /> },
  picker: { seed: UNBOUND, render: () => <StorePane site={siteFromRow(UNBOUND_SITE)} canBind /> },
  readonly: { seed: BOUND, role: "reader", render: () => <StorePane site={siteFromRow(SHOP)} canBind={false} /> },
  // --- Data origins: the connector-coverage band (issue memql#5574) ------
  //
  // `origins-silent` is the production state the issue describes: eight
  // declared concepts, nothing reported, and before this band a page of eight
  // correct em dashes saying nothing about the connector.
  "origins-silent": {
    connect: () => clusterConnection({ dataOrigins: SHOPIFY_CONCEPTS, syncStatesAll: [] }),
    wrap: (el, role) => clusterSession(el, { role }),
    render: () => <OriginsPane />,
  },
  // The silent connector BESIDE a healthy one, which is how it will actually
  // be read: the question is whether the one line worth finding is findable,
  // and a page with one line on it cannot answer that.
  "origins-mixed": {
    connect: () =>
      clusterConnection({
        dataOrigins: [...SHOPIFY_CONCEPTS, ...BOOKS_CONCEPTS],
        syncStatesAll: [
          booksHealth("invoice"),
          booksHealth("payment"),
          booksHealth("customer", { lastError: "the origin refused the last page" }),
        ],
      }),
    wrap: (el, role) => clusterSession(el, { role }),
    render: () => <OriginsPane />,
  },
  // A cluster with nothing wrong. The band has to be QUIET here, or an
  // operator learns to scroll past it and the silent case above is lost with
  // it.
  "origins-reporting": {
    connect: () =>
      clusterConnection({
        dataOrigins: BOOKS_CONCEPTS,
        syncStatesAll: [booksHealth("invoice"), booksHealth("payment"), booksHealth("customer")],
      }),
    wrap: (el, role) => clusterSession(el, { role }),
    render: () => <OriginsPane />,
  },
  // --- Cluster > Mesh (epic memql#5338) ------------------------------------
  //
  // `mesh-healthy` is the control, and it is read FIRST: a cluster whose every
  // node hears, where the band has to be quiet enough that nobody learns to
  // scroll past it. `mesh-island` is the production state that opened the
  // epic -- an edge and a product bff that hear nothing -- beside a node on an
  // older release that has not reported and one gone quiet, so the one line
  // worth finding has to be findable among the others. `mesh-node-*` open a
  // node's page through the section's own intent, so no click is needed.
  "mesh-healthy": { connect: () => clusterConnection({ clusterNodes: meshNodes("healthy") }), wrap: (el, role) => clusterSession(el, { role }), render: () => <MeshPane /> },
  "mesh-island": { connect: () => clusterConnection({ clusterNodes: meshNodes("island") }), wrap: (el, role) => clusterSession(el, { role }), render: () => <MeshPane /> },
  "mesh-empty": { connect: () => clusterConnection({ clusterNodes: [] }), wrap: (el, role) => clusterSession(el, { role }), render: () => <MeshPane /> },
  "mesh-node-bff": { connect: () => clusterConnection({ clusterNodes: meshNodes("island") }), wrap: (el, role) => clusterSession(el, { role }), render: () => <MeshPane nodeId="bff-5658459dd4-92gnh" /> },
  "mesh-node-edge": { connect: () => clusterConnection({ clusterNodes: meshNodes("island") }), wrap: (el, role) => clusterSession(el, { role }), render: () => <MeshPane nodeId="edge-6cc7f56755-rlnsv" /> },
  "mesh-node-identity": { connect: () => clusterConnection({ clusterNodes: meshNodes("healthy") }), wrap: (el, role) => clusterSession(el, { role }), render: () => <MeshPane nodeId="identity-7f9c6d5b8-2xk4q" /> },
  "mesh-node-unreported": { connect: () => clusterConnection({ clusterNodes: meshNodes("island") }), wrap: (el, role) => clusterSession(el, { role }), render: () => <MeshPane nodeId="mcp-64b9d8f7c-8wq2z" /> },
  hidden: {
    seed: BOUND,
    role: "reader",
    render: () => (
      <DeployablePage
        site={siteFromRow(SHOP)}
        pkg={null}
        credentials={[]}
        viewerUserId="u-me"
        nameOf={() => ""}
        can={partsWithout("store")}
        clusterDomain="memql.example.com"
        onBack={() => {}}
        onOpenSource={() => {}}
        onOpenHistory={() => {}}
      />
    ),
  },
};

// --- Data origins (issue memql#5574) ------------------------------------
//
// The connector-coverage band cannot be judged from the diff or from jsdom.
// The whole point of it is that ONE line among several has to be findable at a
// glance on a page of ratios, which is a question about contrast, alignment
// and density -- three things a green vitest case says nothing about.
//
// Three views, and the middle one is the one that matters: `origins-silent` is
// memql#5574's production state, `origins-mixed` is the same page with a
// healthy connector beside the silent one (so the silent line is judged
// against something, not in isolation), and `origins-reporting` is a cluster
// with nothing wrong -- where the band must be quiet enough that nobody learns
// to ignore it.

function shopifyConcept(n: number) {
  return clusterOriginRow({
    conceptId: `v1:shopify:concept${String(n).padStart(2, "0")}`,
    dataState: "mirror",
    origin: "shopify",
    connectors: ["shopify"],
  });
}

const SHOPIFY_CONCEPTS = Array.from({ length: 8 }, (_, i) => shopifyConcept(i + 1));

const BOOKS_CONCEPTS = ["invoice", "payment", "customer"].map((name) =>
  clusterOriginRow({
    conceptId: `v1:books:${name}`,
    dataState: "mirror",
    origin: "quickBooks",
    connectors: ["quickBooks"],
  }),
);

function booksHealth(name: string, over: Record<string, unknown> = {}) {
  return clusterSyncStateRow({
    conceptId: `v1:books:${name}`,
    connector: "quickBooks",
    direction: "inbound",
    lagSeconds: 3,
    driftCount: 0,
    outboxDepth: 0,
    deadLetterCount: 0,
    backfillStatus: "complete",
    paused: false,
    ...over,
  } as never);
}

// ---------------------------------------------------------------------------
// The Fleet's machine detail (epic memql#5327)
// ---------------------------------------------------------------------------
//
// THREE THINGS THIS EPIC PUT ON THE PAGE, and every one of them is a sentence
// rather than a figure -- which is exactly the class jsdom cannot judge. A
// warning that wraps onto four lines, a fact whose value runs past its own
// label, or a red panel sitting above a green one all pass 3,492 assertions.
//
// `healthy` is the CONTROL, and it is the view to read first: the whole
// design is that these warnings are ABSENT almost always, so a capture that
// shows an unbroken machine is what says the page is not now covered in
// advisories. The other three are one state each of the thing that can be
// wrong.

const FLEET_NOW = new Date("2026-09-22T12:00:00Z");

function fleetMachine(over: Record<string, unknown>) {
  return machineFromRow(
    fleetMachineRow({
      id: "v1:worker:registration:studio",
      displayName: "Studio mini",
      name: "studio.local",
      version: "1.4.2",
      buildTag: "computeruse",
      labels: { "model:llama3.1:8b": "ctx=131072" },
      operatorLabels: { "room": "studio" },
      concurrency: { HEADLESS: 4 },
      activeCount: 1,
      rttMs: 34,
      rttAt: "2026-09-22T11:59:30Z",
      lastSeenAt: "2026-09-22T11:59:52Z",
      ...over,
    }),
  );
}

function MachinePane({ over }: { over: Record<string, unknown> }) {
  const machine = fleetMachine(over);
  const writes = {
    busyId: "",
    actionError: "",
    rename: async () => true,
    setOperatorLabels: async () => true,
    revoke: async () => null,
    setSharing: async () => null,
  };
  return (
    <div className="os-window-content">
      <MachineDetail machine={machine} writes={writes} now={FLEET_NOW} view="details" />
    </div>
  );
}

function AccountsPane() {
  const [store] = useState(() => new LocalAccountsSettingsStore({ getItem: () => null, setItem: () => {} }));
  return <WindowBody fallback="Accounts"><AccountsApp sectionId="accounts" navigate={() => {}} askContext={() => {}} store={store} /></WindowBody>;
}

// --- Sharing a machine (epic memql#5344) -----------------------------------
//
// The panel and the dialog, over the suite's own fixture connection. The
// dialog views render ShareDialog directly -- the harness does not click --
// and `?modal=0` draws it in the page rather than the top layer, for a capture
// tool that does not paint the top layer.

const SHARED_WITH_PEOPLE = {
  mode: "people",
  userIds: ["ana", "dee"],
  groupIds: ["design"],
  sharedAt: "2026-09-21T16:30:00Z",
  sharedBy: "v1:identity:user:me",
};

const SHARE_PEOPLE = [
  { id: "ana", name: "Ana Ruiz", detail: "" },
  { id: "bo", name: "Bo Chen", detail: "" },
  { id: "cy", name: "Cy Okafor", detail: "" },
  { id: "eli", name: "Eli Marsh", detail: "" },
];

const SHARE_PEOPLE_ADMIN = [
  { id: "ana", name: "Ana Ruiz", detail: "ana.ruiz@example.com" },
  { id: "bo", name: "Bo Chen", detail: "bo@example.com" },
  { id: "cy", name: "Cy Okafor", detail: "cy.okafor@example.com" },
  { id: "dee", name: "Dee Park", detail: "dee.park@example.com" },
  { id: "eli", name: "Eli Marsh", detail: "eli@example.com" },
  { id: "fay", name: "Fay Lindqvist", detail: "fay.lindqvist@example.com" },
];

const SHARE_DIRECTORY = shareDirectoryRow({
  id: "v1:worker:registration:studio",
  machineId: "v1:worker:registration:studio",
  everyone: false,
  people: SHARE_PEOPLE,
  groups: [
    { id: "design", name: "Design", members: 4 },
    { id: "ops", name: "Operations", members: 7 },
  ],
  current: {
    people: [
      { id: "ana", name: "Ana Ruiz", known: true, inDirectory: true },
      { id: "dee", name: "Dee Park", known: true, inDirectory: false },
    ],
    groups: [{ id: "design", name: "Design", known: true, inDirectory: true }],
  },
});

const SHARE_LEDGER = {
  id: "v1:worker:registration:studio",
  machineId: "v1:worker:registration:studio",
  week: "2026-W39",
  calls: 41,
  people: 3,
  otherCalls: 12,
  otherPeople: 2,
  systemCalls: 0,
  readable: true,
  sentence: "Served 41 calls this week, 12 of them for 2 other people.",
};

function qaWrites(refuse = false): MachineWrites {
  return {
    busyId: "",
    actionError: refuse
      ? "some of the people or groups chosen are not ones you can lend this machine to; choose from the people and groups offered"
      : "",
    rename: async () => true,
    setOperatorLabels: async () => true,
    revoke: async () => null,
    setSharing: async (_id, choice) =>
      refuse ? null : { mode: choice.mode, people: choice.userIds.length, groups: choice.groupIds.length, sentence: "Lent to 2 people and 1 group." },
  };
}

function SharingPane({ over }: { over: Record<string, unknown> }) {
  const machine = fleetMachine({ ownerUserId: "v1:identity:user:me", ...over });
  return (
    <div className="os-window-content">
      <MachineDetail machine={machine} writes={qaWrites()} now={FLEET_NOW} view="sharing" />
    </div>
  );
}

function SharePane({ over, startPeople = false, refuse = false }: { over: Record<string, unknown>; startPeople?: boolean; refuse?: boolean }) {
  const machine = fleetMachine({ ownerUserId: "v1:identity:user:me", ...over });
  const modal = new URLSearchParams(window.location.search).get("modal") !== "0";
  if (!modal) {
    // In-page for a capture tool that does not paint the top layer: the SAME
    // component and stylesheet, opened with show() instead of showModal().
    HTMLDialogElement.prototype.showModal = HTMLDialogElement.prototype.show;
  }
  useEffect(() => {
    // One-shot captures cannot click: choose "Specific people and groups"
    // for the views that open on it, and press Save for the refused one.
    const timer = window.setTimeout(() => {
      if (startPeople) {
        const people = Array.from(document.querySelectorAll<HTMLButtonElement>('[role="radio"]')).find((b) =>
          (b.textContent ?? "").startsWith("Specific people"),
        );
        people?.click();
      }
      // `?arrow=N` arrows N times through the results from the search, so a
      // capture shows whether the active option is kept in view (the list is
      // its options' offsetParent only while it is positioned).
      const arrows = Number(new URLSearchParams(window.location.search).get("arrow") ?? "0");
      if (arrows > 0) {
        window.setTimeout(() => {
          const box = document.querySelector<HTMLInputElement>(".fleet-share-search");
          box?.focus();
          for (let i = 0; i < arrows; i += 1) {
            window.setTimeout(() => {
              box?.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
            }, 150 * (i + 1));
          }
        }, 500);
      }
      if (refuse) {
        const remove = document.querySelector<HTMLButtonElement>(".fleet-share-chip-remove");
        remove?.click();
        window.setTimeout(() => {
          const save = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find((b) => b.textContent === "Save");
          save?.click();
        }, 300);
      }
    }, 1200);
    return () => window.clearTimeout(timer);
  }, [startPeople, refuse]);
  return (
    <div className="os-window-content">
      <MachineDetail machine={machine} writes={qaWrites(refuse)} now={FLEET_NOW} view="sharing" />
      <ShareDialog machine={machine} writes={qaWrites(refuse)} onDiscard={() => undefined} onSaved={() => undefined} />
    </div>
  );
}

// --- Cluster > Mesh (epic memql#5338) --------------------------------------
//
// The cloud's topology as memql#5316 measured it, pod names and all: two
// engine bffs dialing every worker and identity, two product bffs doing the
// same, agents and planners and workbenches parented on a bff, edges and mcps
// on theirs, and identity dialed by every bff.
const MESH_PODS = {
  bffA: "bff-5658459dd4-92gnh",
  bffB: "bff-5658459dd4-tprvz",
  prodA: "bff-shop-545bf75455-88bfj",
  prodB: "bff-shop-545bf75455-tprvz",
  agentA: "agent-7c9d8f6b54-abcde",
  agentB: "agent-7c9d8f6b54-fghij",
  plannerA: "planner-58f7b9c6d4-k2m4p",
  plannerB: "planner-58f7b9c6d4-q8r3t",
  benchA: "workbench-6d5f4c7b9-x7v2n",
  benchB: "workbench-6d5f4c7b9-z3w8y",
  edgeA: "edge-6cc7f56755-79j5s",
  edgeB: "edge-6cc7f56755-rlnsv",
  mcpA: "mcp-64b9d8f7c-8wq2z",
  mcpB: "mcp-64b9d8f7c-m5n6b",
  idA: "identity-7f9c6d5b8-2xk4q",
  idB: "identity-7f9c6d5b8-9pl3r",
};

function meshNodes(shape: "healthy" | "island") {
  const P = MESH_PODS;
  const workers = [P.agentA, P.agentB, P.plannerA, P.plannerB, P.benchA, P.benchB];
  const typeOf = (id: string) => id.replace(/-[a-z0-9]+-[a-z0-9]+$/, "").replace(/^bff-shop$/, "bff");
  const link = (node: string, via: string) => ({ node, type: typeOf(node), via });
  const now = Date.now();
  const at = (secondsAgo: number) => new Date(now - secondsAgo * 1000).toISOString();
  const bff = (id: string, children: string[]) => ({
    id,
    nodeType: "bff",
    address: `10.244.0.${Object.values(MESH_PODS).indexOf(id) + 11}:50058`,
    mesh: {
      links: [
        ...workers.map((w) => link(w, [P.agentA, P.agentB, P.plannerA, P.benchA].includes(w) && id === P.bffA ? "both" : "dialed")),
        link(P.idA, "dialed"),
        link(P.idB, "dialed"),
        ...children.map((c) => link(c, "accepted")),
      ],
      heard: 48210 + id.length * 37,
      duplicates: 9120,
      originated: 1840,
      relayed: 46100,
    },
  });
  const worker = (id: string, extra: object[] = []) => ({
    id,
    nodeType: typeOf(id),
    address: `10.244.1.${Object.values(MESH_PODS).indexOf(id) + 11}:${{ agent: 50055, planner: 50056, workbench: 50060 }[typeOf(id)] ?? 50052}`,
    mesh: {
      links: [link(P.bffA, id === P.agentA || id === P.agentB || id === P.plannerA || id === P.benchA ? "both" : "accepted"), link(P.bffB, "accepted"), link(P.prodA, "accepted"), link(P.prodB, "accepted"), ...extra],
      heard: 51002 - id.length * 11,
      duplicates: 22040,
      originated: 610,
      relayed: 300,
    },
  });
  const rows = [
    bff(P.bffA, [P.edgeA, P.mcpA]),
    bff(P.bffB, [P.edgeB, P.mcpB]),
    bff(P.prodA, []),
    bff(P.prodB, []),
    worker(P.agentA, [link(P.benchA, "dialed")]),
    worker(P.agentB, [link(P.benchA, "dialed")]),
    worker(P.plannerA, [link(P.agentA, "dialed"), link(P.agentB, "dialed")]),
    worker(P.plannerB, [link(P.agentA, "dialed"), link(P.agentB, "dialed")]),
    worker(P.benchA),
    worker(P.benchB),
    { id: P.edgeA, nodeType: "edge", address: "10.244.2.7:50062", mesh: { links: [link(P.bffA, "dialed")], heard: 47880, duplicates: 0, originated: 22, relayed: 0 } },
    { id: P.edgeB, nodeType: "edge", address: "10.244.2.9:50062", mesh: { links: [link(P.bffB, "dialed")], heard: 47880, duplicates: 0, originated: 22, relayed: 0 } },
    { id: P.mcpA, nodeType: "mcp", address: "10.244.3.4:50060", mesh: { links: [link(P.bffA, "dialed")], heard: 47700, duplicates: 0, originated: 5, relayed: 0 } },
    { id: P.mcpB, nodeType: "mcp", address: "10.244.3.5:50060", mesh: { links: [link(P.bffB, "dialed")], heard: 47700, duplicates: 0, originated: 5, relayed: 0 } },
    { id: P.idA, nodeType: "identity", address: "10.244.4.2:50061", mesh: { receives: false, links: [P.bffA, P.bffB, P.prodA, P.prodB].map((b) => link(b, "accepted")), heard: 0, duplicates: 0, originated: 412, relayed: 0, lastHeardAt: undefined } },
    { id: P.idB, nodeType: "identity", address: "10.244.4.3:50061", mesh: { receives: false, links: [P.bffA, P.bffB, P.prodA, P.prodB].map((b) => link(b, "accepted")), heard: 0, duplicates: 0, originated: 398, relayed: 0, lastHeardAt: undefined } },
  ] as Array<{ id: string; nodeType: string; address: string; mesh: Record<string, unknown> | null }>;

  if (shape === "island") {
    // The epic's production state: an edge and a product bff that are linked
    // and have heard nothing in hours; an mcp still on the release before the
    // report existed; a planner gone quiet for eleven minutes.
    for (const r of rows) {
      if (r.id === P.edgeB) r.mesh = { ...r.mesh, heard: 0, relayed: 0, lastHeardAt: undefined, since: at(3 * 3600) };
      if (r.id === P.prodB) r.mesh = { ...r.mesh, heard: 0, relayed: 0, duplicates: 0, lastHeardAt: undefined, since: at(3 * 3600) };
      if (r.id === P.mcpA) r.mesh = null;
      if (r.id === P.plannerB) r.mesh = { ...r.mesh, lastHeardAt: at(11 * 60), dropped: 14 };
    }
  }
  return rows.map((r) => {
    const row = clusterNodeRow({ id: r.id, nodeType: r.nodeType, address: r.address, mesh: r.mesh });
    const mesh = row.mesh as Record<string, unknown> | undefined;
    if (mesh && mesh.lastHeardAt === undefined) delete mesh.lastHeardAt;
    return row;
  });
}

function MeshPane({ nodeId }: { nodeId?: string }) {
  return (
    <div className="os-window-content">
      <MeshSection intent={nodeId ? { id: "qa-open", payload: { nodeId } } : undefined} consumeIntent={() => {}} />
    </div>
  );
}

function OriginsPane() {
  return (
    <div className="os-window-content">
      <OriginsSection />
    </div>
  );
}

function App() {
  const params = new URLSearchParams(window.location.search);
  const name = params.get("view") ?? "store";
  const view = VIEWS[name] ?? VIEWS.store;
  const [ready, setReady] = useState(false);

  useEffect(() => {
    installQaConnection(view.connect ? view.connect() : fakeConnection(view.seed ?? {}));
    setReady(true);
  }, [name]);

  if (!ready) return null;
  // THE PROVIDERS ARE THE SUITE'S OWN. `withSession` installs the session, the
  // shell provider and the role's seeded capability set together -- these
  // surfaces read all three (a Logs action opens another app, and `useOs`
  // throws outside its provider), and a harness that wired them itself would
  // be a second reading of the access model beside the one the tests use.
  const role = view.role ?? "owner";
  // A view with its own wrapper renders whole: its render() already supplies
  // whatever body it needs, so wrapping it again in `.os-window-content`
  // would nest two of them.
  if (view.wrap) return view.wrap(view.render(), role);
  if (view.framed) return withSession(view.render(), { role, userId: "u-me" });
  return <div className="os-window-content">{withSession(view.render(), { role })}</div>;
}

// MODE is `data-theme` on the root (src/styles/tokens.css); `data-os-theme`
// is the theme PACK, which is a different axis. Setting the wrong one renders
// the system-preference branch and both captures come out identical.
// A ONE-SHOT CAPTURE CANNOT SCROLL, and every surface long enough to be worth
// judging has something below the fold. `?at=<text>` finds the first element
// whose text starts with it and scrolls it to the top, after the virtual clock
// has let the reads land -- so a section reached by scrolling can be captured
// without a driver attached.
const at = new URLSearchParams(window.location.search).get("at") ?? "";
if (at !== "") {
  window.setTimeout(() => {
    const target = Array.from(document.querySelectorAll<HTMLElement>("h3, h4, .os-subhead")).find(
      (el) => (el.textContent ?? "").trim().startsWith(at),
    );
    target?.scrollIntoView({ block: "start" });
  }, 2500);
}

// A ONE-SHOT CAPTURE CANNOT CLICK EITHER, and a <details> is shut until
// somebody does. `?open=1` opens every one on the page once the reads have
// landed, so a facts list -- the densest thing on a machine detail, and the
// place a long value runs past its own label -- can be judged as pixels
// without a driver attached.
if (new URLSearchParams(window.location.search).get("open") === "1") {
  window.setTimeout(() => {
    for (const el of document.querySelectorAll("details")) el.open = true;
  }, 2500);
}

const mode = new URLSearchParams(window.location.search).get("mode") ?? "dark";
document.documentElement.setAttribute("data-theme", mode);
document.documentElement.setAttribute("data-os-theme", "graphite");
document.documentElement.style.colorScheme = mode;

createRoot(document.getElementById("root")!).render(<App />);
