import type { ComponentType, ReactElement, ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablePage } from "../src/apps/deployables/page/DeployablePage";
import { ALL_PARTS } from "../src/apps/deployables/parts";
import { siteFromRow } from "../src/apps/deployables/rows";
import { ShopifyStorePanel } from "../src/apps/deployables/store/ShopifyStorePanel";
import { ShopifyConnections } from "../src/modules/connections/ShopifyConnections";
import { fakeConnection, previewReadinessRow, SHOP, siteRow, storeVerdictRow, verdictReasonRow, withSession, type FakeSeed } from "../test/deployables/harness";

// The Store page saying whether a store is being recorded (U7), one view per
// state, over the suite's own fixture connection. Fixtures only: nothing here
// registers, retries, pauses or emails anything.
//
//   view=store-verdict-recording    recorded, with what is recorded, withheld and outside
//   view=store-verdict-starting     a backfill running
//   view=store-verdict-paused       paused, with Resume
//   view=store-verdict-attention    every reason and its act, and a failed alert
//   view=store-verdict-readonly     the same, for somebody who may not act
//   view=store-verdict-uninstalled  Shopify uninstalled the app: not connected
//   view=store-verdict-unknown      the verdict could not be read
//   view=store-verdict-nostate      the reply named no state (no Shopify connector)
//   view=store-verdict-two-stores   two different stores, each notice naming its own
//   view=store-verdict-own-app      a store on its own app that grants less: a reason with no act
//   view=store-verdict-slot         the deployable's page, its Store slot
//   view=store-verdict-slot-unavailable
//                                   the slot when the bound store does not read back
//   view=settings-shopify-alert     Settings > Shopify with the alert wait
//
// With `mode=light|dark` and `width=narrow` as everywhere in the harness.

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

const ID = "acme-widgets";
const OWN: Row = {
  id: ID,
  domain: "acme-widgets.myshopify.com",
  adminTokenRef: "SHOPIFY_ACME-WIDGETS_ADMIN_TOKEN",
  storefrontTokenRef: "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN",
  status: "live",
  isDevelopment: false,
} as unknown as Row;
const DEV_ID = "acme-widgets-dev";
const DEV: Row = { ...OWN, id: DEV_ID, domain: "acme-widgets-dev.myshopify.com", isDevelopment: true } as unknown as Row;
const TWO_SITE = siteRow({ ...SHOP, binding: { storeId: ID }, previewBinding: { storeId: DEV_ID } } as never);
const SITE = siteRow({ ...SHOP, binding: { storeId: ID }, previewBinding: { storeId: ID } } as never);

const RECORDED = [
  { kind: "catalog", lastChangeAt: "2026-11-11T09:12:00Z", lastCheckedAt: "2026-11-11T09:15:00Z" },
  { kind: "orders", lastChangeAt: "2026-11-11T08:58:00Z", lastCheckedAt: "2026-11-11T09:15:00Z" },
  { kind: "customers", lastChangeAt: "", lastCheckedAt: "2026-11-11T09:15:00Z" },
  { kind: "discounts", lastChangeAt: "2026-11-02T16:40:00Z", lastCheckedAt: "2026-11-11T09:10:00Z" },
];

const REASONS = [
  verdictReasonRow({ code: "data_not_declared", domain: "customers", sentence: "Shopify refuses customers for this app, because the app has not declared the customer data it uses.", act: "declare", actLabel: "Declare the customer data", actUrl: "https://shopify.dev/docs/apps/launch/protected-customer-data" }),
  verdictReasonRow({ code: "approval_outdated", sentence: "This store approved an older list of permissions.", detail: "read_returns", act: "approve", actLabel: "Open MemQL OS from the store's Shopify admin and approve", actUrl: "https://acme-widgets.myshopify.com/admin/apps" }),
  verdictReasonRow({ code: "events_not_registered", sentence: "Shopify is not sending every change.", detail: "orders/edited was not registered", act: "register", actLabel: "Register again" }),
  verdictReasonRow({ code: "check_failing", domain: "orders", sentence: "The last check of orders failed: the Admin API answered 503", detail: "The next check runs within 5 minutes." }),
  verdictReasonRow({ code: "changes_not_recorded", sentence: "At least 3 changes could not be recorded.", count: 3, atLeast: true, act: "retry", actLabel: "Try again" }),
];

function seed(verdict: Partial<Row> | null, over: Partial<FakeSeed> = {}, store: Row = OWN): FakeSeed {
  return {
    sites: [SITE],
    stores: [store],
    previewReadiness: { [SITE["id"] as string]: previewReadinessRow({ siteId: String(SITE["id"]), storeId: ID, storeReadable: true, storeHasStorefrontToken: true, previewStoreHasStorefrontToken: true } as never) },
    ...(verdict === null ? {} : { storeVerdict: { [ID]: storeVerdictRow({ storeId: ID, domain: "acme-widgets.myshopify.com", ...verdict }) } }),
    ...over,
  };
}

export function storeVerdictScenes(Frame: Frame) {
  const page = (s: FakeSeed) => ({
    connect: () => fakeConnection(s),
    wrap: (element: ReactElement, role: string) => withSession(element, { role, userId: "u-me" }),
    render: () => <Frame fallback="Deployables"><ShopifyStorePanel site={siteFromRow(SITE)} canBind trail={[{ label: "Deployables" }, { label: "shop.memql.example.com" }, { label: "Store" }]} back={{ label: "shop.memql.example.com", onSelect: () => {} }} onWritten={() => {}} /></Frame>,
  });
  return {
    "store-verdict-recording": page(seed({ recorded: RECORDED, ordersFrom: "2026-08-13", withheld: ["product variants: delivery profile"], outside: ["abandoned checkouts", "companies", "company locations"] })),
    "store-verdict-starting": page(seed({ state: "starting", recorded: RECORDED.map((k) => ({ ...k, lastChangeAt: "" })) })),
    "store-verdict-paused": page(seed({ state: "paused", recorded: RECORDED })),
    "store-verdict-attention": page(seed({ state: "needs attention", reasons: REASONS, recorded: RECORDED, alert: { state: "failed", reason: "the mail server refused the message" } })),
    "store-verdict-own-app": page(seed({ state: "needs attention", reasons: [verdictReasonRow({ code: "approval_outdated", sentence: "This store's own app does not allow everything MemQL records.", detail: "read_returns" })], recorded: RECORDED })),
    "store-verdict-readonly": page(seed({ state: "needs attention", reasons: REASONS, recorded: RECORDED, may: { act: false } })),
    "store-verdict-uninstalled": page(seed({ state: "not connected", alert: { state: "failed", reason: "the store has no owner with an address" } }, {}, { ...OWN, adminTokenRef: "", uninstalledAt: "2026-11-10T08:00:00Z" } as unknown as Row)),
    "store-verdict-unknown": page(seed(null, { storeVerdictError: "shopify: the store's sync state could not be read" })),
    "store-verdict-nostate": page(seed(null)),
    "store-verdict-two-stores": {
      connect: () => fakeConnection({
        sites: [TWO_SITE],
        stores: [OWN, DEV],
        previewReadiness: { [TWO_SITE["id"] as string]: previewReadinessRow({ siteId: String(TWO_SITE["id"]), storeId: ID, storeReadable: true, storeHasStorefrontToken: true, previewStoreHasStorefrontToken: true } as never) },
        storeVerdict: {
          [ID]: storeVerdictRow({ storeId: ID, state: "needs attention", reasons: REASONS.slice(3, 4), recorded: RECORDED.slice(0, 2) }),
          [DEV_ID]: storeVerdictRow({ storeId: DEV_ID, state: "needs attention", reasons: REASONS.slice(2, 3), recorded: RECORDED.slice(0, 2), alert: { state: "failed", reason: "the store has no owner with an address" } }),
        },
      }),
      wrap: (element: ReactElement, role: string) => withSession(element, { role, userId: "u-me" }),
      render: () => <Frame fallback="Deployables"><ShopifyStorePanel site={siteFromRow(TWO_SITE)} canBind trail={[{ label: "Deployables" }, { label: "shop.memql.example.com" }, { label: "Store" }]} back={{ label: "shop.memql.example.com", onSelect: () => {} }} onWritten={() => {}} /></Frame>,
    },
    "store-verdict-slot": {
      connect: () => fakeConnection(seed({ state: "needs attention", reasons: REASONS.slice(2, 3) })),
      wrap: (element: ReactElement, role: string) => withSession(element, { role, userId: "u-me" }),
      render: () => <Frame fallback="Deployables"><DeployablePage site={siteFromRow(SITE)} pkg={null} credentials={[]} viewerUserId="u-me" nameOf={() => ""} can={ALL_PARTS} clusterDomain="memql.example.com" onBack={() => {}} onOpenSource={() => {}} onOpenHistory={() => {}} /></Frame>,
    },
    "store-verdict-slot-unavailable": {
      connect: () => fakeConnection(seed(null, { stores: [] })),
      wrap: (element: ReactElement, role: string) => withSession(element, { role, userId: "u-me" }),
      render: () => <Frame fallback="Deployables"><DeployablePage site={siteFromRow(SITE)} pkg={null} credentials={[]} viewerUserId="u-me" nameOf={() => ""} can={ALL_PARTS} clusterDomain="memql.example.com" onBack={() => {}} onOpenSource={() => {}} onOpenHistory={() => {}} /></Frame>,
    },
    "settings-shopify-alert": {
      connect: () => fakeConnection({ storeAlertWait: { minutes: 30, isDefault: true, maySet: true } as unknown as Row }),
      wrap: (element: ReactElement, role: string) => withSession(element, { role, userId: "u-me" }),
      render: () => <Frame fallback="Settings"><ShopifyConnections header={<h3 className="os-settings-title">Settings</h3>} appId="settings" /></Frame>,
    },
  };
}
