import { useState, type ComponentType, type ReactElement, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablePage } from "../src/apps/deployables/page/DeployablePage";
import type { WorkspaceDetail } from "../src/apps/deployables/page/DeployableWorkspace";
import { ALL_PARTS, partsWithout, type PartsHeld } from "../src/apps/deployables/parts";
import { siteFromRow } from "../src/apps/deployables/rows";
import { DEFAULT_DEPLOYABLES_SETTINGS, type DeployablesSettings } from "../src/apps/deployables/settings";
import { DeployablesSettingsProvider } from "../src/apps/deployables/settingsContext";
import { fakeConnection, siteRow, storeStatisticsRow, withSession, type FakeSeed } from "../test/deployables/harness";

// A storefront's Store statistics: the button beside Statistics, and the
// page's head and context in each state (the 2026-11-11 store statistics
// design, Task 21). The figures, the chart and the lists are not drawn yet.
// Fixtures only: an invented bicycle shop.
//
//   view=storefront-with-store-statistics      the storefront's page: Store statistics after Statistics
//   view=storefront-without-sales              the same page to a person without the sales part: no button
//   view=storefront-draft-store-statistics     a draft storefront: no Statistics, Store statistics after Logs
//   view=store-statistics                      two stores, Production read; development, tax and older-orders lines
//   view=store-statistics-both                 both destinations name one store: "Production and Testing", no control
//   view=store-statistics-testing              only Testing has a store: opened on it, no control
//   view=store-statistics-no-zone              the answer names no zone: "Days in UTC"
//   view=store-statistics-starting             the walk holds no row for the store yet
//   view=store-statistics-pending              the read not answered yet: the context line's shape
//   view=store-statistics-refused              a refused read, in the engine's own sentence
//
// With `mode=light|dark`, `theme=` and `width=narrow` as everywhere in the
// harness.

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

const HOST = "bikes.memql.example.com";
const BIKES = siteRow({
  id: "site-bikes", hostname: HOST, kind: "shopify_storefront", status: "live", title: "City bikes",
  bundleRef: "blob://sites/site-bikes/v1/", binding: { storeId: "store-bikes" }, previewBinding: { storeId: "store-bikes-dev" },
});
const ONE_STORE = { ...BIKES, previewBinding: { storeId: "store-bikes" } };
const TESTING_ONLY = { ...BIKES, binding: {} };
const DRAFT = { ...BIKES, status: "draft" };

const STORES: Row[] = [
  { id: "store-bikes", domain: "acme-widgets.myshopify.com", name: "A city bike shop", status: "live", isDevelopment: false, developmentOfStoreId: "" } as unknown as Row,
  { id: "store-bikes-dev", domain: "acme-widgets-dev.myshopify.com", name: "A city bike shop (development)", status: "configured", isDevelopment: true, developmentOfStoreId: "store-bikes" } as unknown as Row,
];

const NAMES = { siteId: "site-bikes", storeName: "A city bike shop", storeDomain: "acme-widgets.myshopify.com" };
const CURRENT = storeStatisticsRow({ storeId: "store-bikes", ...NAMES, historySince: "2026-09-11T22:00:00Z", lastChangeAt: "2026-11-08T10:13:00+01:00", taxesIncluded: true, olderOrderChangesNotRecorded: true });
const DEV = storeStatisticsRow({ storeId: "store-bikes-dev", siteId: "site-bikes", storeName: "A city bike shop (development)", storeDomain: "acme-widgets-dev.myshopify.com", destination: "testing", development: true });
const NO_ZONE = storeStatisticsRow({ storeId: "store-bikes", ...NAMES, timezone: null, start: "2026-11-02T00:00:00Z", end: "2026-11-09T00:00:00Z", historySince: "2026-09-12T00:00:00Z" });
const STARTING = storeStatisticsRow({
  storeId: "store-bikes", siteId: "site-bikes", storeName: null, storeDomain: null, currency: null, timezone: null, development: null, taxesIncluded: null,
  olderOrderChangesNotRecorded: null, bucket: null, start: null, end: null, prevStart: null, historySince: null, lastChangeAt: null,
  recording: { state: "starting", reasons: [], since: null, blocked: false }, rebuilding: null, series: [],
});

/** A connection whose store statistics read never answers. */
function pendingConnection(seed: FakeSeed) {
  const connection = fakeConnection(seed);
  const execute = connection.query.executeNamed.bind(connection.query) as (...args: unknown[]) => unknown;
  connection.query.executeNamed = ((name: string, ...rest: unknown[]) =>
    name === "storeStatistics" ? new Promise(() => {}) : execute(name, ...rest)) as typeof connection.query.executeNamed;
  return connection;
}

/** A deployable's page, under the app's settings so its period control works. */
function DeployableOn({ site, detail, can }: { site: Row; detail?: WorkspaceDetail; can: PartsHeld }) {
  const [settings, setSettings] = useState<DeployablesSettings>({ ...DEFAULT_DEPLOYABLES_SETTINGS, statisticsPeriod: "7d" });
  return (
    <DeployablesSettingsProvider value={{ settings, update: (patch) => setSettings((held) => ({ ...held, ...patch })), toggleSource: () => {} }}>
      <DeployablePage site={siteFromRow(site)} pkg={null} credentials={[]} viewerUserId="u-me" nameOf={() => ""} can={can} clusterDomain="memql.example.com" onBack={() => {}} onOpenSource={() => {}} initialDetail={detail} />
    </DeployablesSettingsProvider>
  );
}

export function storeStatisticsScenes(Frame: Frame) {
  const scene = (site: Row, seed: Omit<FakeSeed, "sites" | "stores">, detail?: WorkspaceDetail, can: PartsHeld = ALL_PARTS, connect?: () => unknown) => ({
    connect: connect ?? (() => fakeConnection({ sites: [site], stores: STORES, ...seed })),
    wrap: (element: ReactElement, role: string) => withSession(element, { role, userId: "u-me" }),
    render: () => <Frame fallback="Deployables"><DeployableOn site={site} detail={detail} can={can} /></Frame>,
  });
  const both = { storeStatistics: { "store-bikes": CURRENT, "store-bikes-dev": DEV } };
  return {
    "storefront-with-store-statistics": scene(BIKES, both),
    "storefront-without-sales": scene(BIKES, both, undefined, partsWithout("sales")),
    "storefront-draft-store-statistics": scene(DRAFT, both),
    "store-statistics": scene(BIKES, both, "storeStatistics"),
    "store-statistics-both": scene(ONE_STORE, { storeStatistics: { "store-bikes": { ...CURRENT, destination: "both" } } }, "storeStatistics"),
    "store-statistics-testing": scene(TESTING_ONLY, { storeStatistics: { "store-bikes-dev": DEV } }, "storeStatistics"),
    "store-statistics-no-zone": scene(BIKES, { storeStatistics: { "store-bikes": NO_ZONE } }, "storeStatistics"),
    "store-statistics-starting": scene(BIKES, { storeStatistics: { "store-bikes": STARTING } }, "storeStatistics"),
    "store-statistics-pending": scene(BIKES, {}, "storeStatistics", ALL_PARTS, () => pendingConnection({ sites: [BIKES], stores: STORES })),
    "store-statistics-refused": scene(BIKES, { storeStatisticsError: "This deployable does not serve that store for you." }, "storeStatistics"),
  };
}
