import type { ComponentType, ReactElement, ReactNode } from "react";

import { OriginsSection } from "../src/apps/cluster/origins/OriginsSection";
import { dataOriginRow, fakeConnection, inboundRequestRow, syncStateRow, withSession } from "../test/cluster/harness";

// Cluster > Data, by store (U7), over the suite's own fixture connection.
// Fixtures only: nothing here backfills, pauses or requeues anything.
//
//   view=origins-by-store   a Shopify connector serving two stores -- the
//                           second with domains its connection never asked
//                           for and one failing domain -- beside a connector
//                           that keeps no stores apart. Add `click=Check` to
//                           open the changes that could not be recorded.
//   view=origins-by-store-admin
//                           the same page for a person who may open it and
//                           is not a cluster owner: every state, no act.
//
// With `mode=light|dark` and `width=narrow` as everywhere in the harness.

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

const STORE = "acme-widgets";
const DEV = "acme-widgets-dev";

const mirror = (conceptId: string, connector: string) =>
  dataOriginRow({ conceptId, dataState: "mirror", origin: connector, connectors: [connector] });

const SHOPIFY = ["order", "customer", "product", "abandonedCheckout", "company"].map((name) => mirror(`v1:shopify:${name}`, "shopify"));
const BOOKS = ["invoice", "payment"].map((name) => mirror(`v1:books:${name}`, "quickBooks"));

const NOT_REQUESTED = "connector: not requested: the grant does not hold the scope this domain reads";

function shopifyState(name: string, partitionKey: string, over: Record<string, unknown> = {}) {
  return syncStateRow({
    id: `v1:platform:syncState:${name}-${partitionKey}`,
    conceptId: `v1:shopify:${name}`,
    connector: "shopify",
    partitionKey,
    lagSeconds: 2,
    driftCount: 0,
    outboxDepth: 0,
    deadLetterCount: 0,
    backfillStatus: "complete",
    paused: false,
    ...over,
  });
}

const HEALTH = [
  ...["order", "customer", "product"].map((name) => shopifyState(name, STORE)),
  ...["abandonedCheckout", "company"].map((name) => shopifyState(name, STORE, { lastError: NOT_REQUESTED })),
  shopifyState("order", DEV, { lastError: "the Admin API answered 503" }),
  shopifyState("customer", DEV, { paused: true }),
  shopifyState("product", DEV),
  ...["abandonedCheckout", "company"].map((name) => shopifyState(name, DEV, { lastError: NOT_REQUESTED })),
  // Written before the connector kept its stores apart: history, not drawn.
  syncStateRow({ conceptId: "v1:shopify:order", connector: "shopify", lastError: "a failure from before the stores were kept apart" }),
  ...["invoice", "payment"].map((name) => syncStateRow({ conceptId: `v1:books:${name}`, connector: "quickBooks", lagSeconds: 4, driftCount: 0, outboxDepth: 0, deadLetterCount: 0 })),
];

const GIVEN_UP = [
  inboundRequestRow({ id: "in-4", source: `shopify-${DEV}`, lastError: "the Admin API answered 503" }),
  inboundRequestRow({ id: "in-3", source: `shopify-${STORE}`, lastError: "orders/edited: the order is not in the mirror yet" }),
  inboundRequestRow({ id: "in-2", source: `shopify-${DEV}`, lastError: "an older reason" }),
  inboundRequestRow({ id: "in-1", source: `shopify-${DEV}`, lastError: "an older reason" }),
];

export function originsByStoreScenes(Frame: Frame) {
  const scene = (role?: string) => ({
    connect: () => fakeConnection({ dataOrigins: [...SHOPIFY, ...BOOKS], syncStatesAll: HEALTH, inboundRequestsGivenUp: GIVEN_UP }),
    wrap: (element: ReactElement, role: string) => withSession(element, { role }),
    ...(role === undefined ? {} : { role }),
    render: () => <Frame fallback="Cluster"><OriginsSection /></Frame>,
  });
  return {
    "origins-by-store": scene(),
    "origins-by-store-admin": scene("admin"),
  };
}
