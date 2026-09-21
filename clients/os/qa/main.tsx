import { createRoot } from "react-dom/client";
import { useEffect, useState } from "react";

import "../src/styles/index.css";
import { Panel } from "../src/kit";
import { setRoleLadder } from "../src/system/roles";
import { SEEDED_LADDER } from "../test/seededLadder";
import {
  DEV_STORE,
  SHOP,
  STORE,
  fakeConnection,
  siteRow,
  storeHealthRow,
  domainStateRow,
  withSession,
  type FakeSeed,
} from "../test/deployables/harness";
import { installQaConnection } from "./connectionShim";
import { StorePanel } from "../src/apps/deployables/store/StorePanel";
import { DeployablePage } from "../src/apps/deployables/page/DeployablePage";
import { ALL_PARTS, partsWithout } from "../src/apps/deployables/parts";
import { siteFromRow } from "../src/apps/deployables/rows";

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

const VIEWS: Record<string, { seed: FakeSeed; role?: string; render: () => JSX.Element }> = {
  overview: { seed: BOUND, render: () => <Overview site={siteFromRow(SHOP)} /> },
  store: { seed: BOUND, render: () => <StorePane site={siteFromRow(SHOP)} canBind /> },
  quiet: { seed: QUIET, render: () => <StorePane site={siteFromRow(SHOP)} canBind /> },
  picker: { seed: UNBOUND, render: () => <StorePane site={siteFromRow(UNBOUND_SITE)} canBind /> },
  readonly: { seed: BOUND, role: "reader", render: () => <StorePane site={siteFromRow(SHOP)} canBind={false} /> },
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

function App() {
  const params = new URLSearchParams(window.location.search);
  const name = params.get("view") ?? "store";
  const view = VIEWS[name] ?? VIEWS.store;
  const [ready, setReady] = useState(false);

  useEffect(() => {
    installQaConnection(fakeConnection(view.seed));
    setReady(true);
  }, [name]);

  if (!ready) return null;
  // THE PROVIDERS ARE THE SUITE'S OWN. `withSession` installs the session, the
  // shell provider and the role's seeded capability set together -- these
  // surfaces read all three (a Logs action opens another app, and `useOs`
  // throws outside its provider), and a harness that wired them itself would
  // be a second reading of the access model beside the one the tests use.
  return <div className="os-window-content">{withSession(view.render(), { role: view.role ?? "owner" })}</div>;
}

// MODE is `data-theme` on the root (src/styles/tokens.css); `data-os-theme`
// is the theme PACK, which is a different axis. Setting the wrong one renders
// the system-preference branch and both captures come out identical.
const mode = new URLSearchParams(window.location.search).get("mode") ?? "dark";
document.documentElement.setAttribute("data-theme", mode);
document.documentElement.setAttribute("data-os-theme", "graphite");
document.documentElement.style.colorScheme = mode;

createRoot(document.getElementById("root")!).render(<App />);
