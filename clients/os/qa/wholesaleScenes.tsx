import type { ComponentType, ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablePage } from "../src/apps/deployables/page/DeployablePage";
import { ALL_PARTS } from "../src/apps/deployables/parts";
import { siteFromRow } from "../src/apps/deployables/rows";
import { siteRow, withSession, type FakeSeed } from "../test/deployables/harness";
import { seededAccessFor } from "../test/seededAccess";

// A store's Wholesale section, rendered (apps/deployables/wholesale). Fixtures
// only: invented companies, people and addresses.
//
//   view=wholesale                         the list ready to review, as the store's people read it
//   view=wholesale-operator                the same list, as a cluster operator reads it
//   view=wholesale-application             one application ready to review
//   view=wholesale-access-failed           one approved, its access refused by the store's plan
//   view=wholesale-removed                 one removed, its tag still on
//   view=wholesale-settings                the store's settings
//   view=wholesale-not-set-up              a store with no settings, as its people read it
//   view=wholesale-not-set-up-operator     the same store, as a cluster operator reads it
//
// With `mode=light|dark`, `theme=` and `width=narrow` as everywhere in the
// harness. The views past the list are reached by pressing (WHOLESALE_CLICKS).

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

const ACCOUNT = "acct-acme";
const SITE = siteRow({
  id: "site-shop", hostname: "shop.memql.example.com", title: "Example Storefront", kind: "shopify_storefront",
  binding: { storeId: "store-live" }, packageId: "pkg-shop", packageDeployableName: "storefront", accountId: ACCOUNT,
});
const ago = (days: number): string => new Date(Date.now() - days * 86_400_000).toISOString();
const id = (slug: string) => `v1:wholesale:application:${slug}`;

const READY = [
  { applicationId: id("north"), companyName: "Northwind Dental Supply", applicantName: "Avery Stone", applicantEmail: "avery@northwind.example.com", stage: "readyToReview", primary: "Dental practice", secondary: "12 locations", createdAt: ago(9) },
  { applicationId: id("harbor"), companyName: "Harbor Pediatric Dentistry and Orthodontics Group", applicantName: "Sam Okafor", applicantEmail: "sam.okafor@harbor-pediatric.example.com", stage: "readyToReview", primary: "Pediatric practice", secondary: "3 locations", createdAt: ago(4) },
  { applicationId: id("blue"), companyName: "Bluebird Clinic", applicantName: "Jordan Reyes", applicantEmail: "jordan@bluebird.example.com", stage: "readyToReview", primary: "", secondary: "", createdAt: ago(0.2) },
];
const APPROVED = [
  { applicationId: id("cedar"), companyName: "Cedar Smile Studio", applicantName: "Riley Chen", applicantEmail: "riley@cedarsmile.example.com", stage: "approved", primary: "Dental practice", secondary: "1 location", createdAt: ago(21) },
];
const REMOVED = [
  { applicationId: id("maple"), companyName: "Maple Street Dental", applicantName: "Casey Morgan", applicantEmail: "casey@maplestreet.example.com", stage: "revoked", primary: "Dental practice", secondary: "2 locations", createdAt: ago(60) },
];
const COUNTS = { waitingForTerms: 0, readyToReview: 3, approved: 1, rejected: 4, revoked: 1 };

function detail(row: (typeof READY)[number], over: Record<string, unknown> = {}): Row {
  return {
    application: { id: row.applicationId, companyName: row.companyName, applicantName: row.applicantName, applicantEmail: row.applicantEmail, source: "storefront", createdAt: row.createdAt },
    state: "submitted",
    stage: "readyToReview",
    details: {
      state: "shown",
      fields: row.primary
        ? [{ name: "practiceType", label: "Practice type", value: row.primary }, { name: "locations", label: "Locations", value: row.secondary }, { name: "website", label: "Website", value: "https://www.example.com" }]
        : [],
    },
    decisions: [],
    entitlement: null,
    operator: false,
    acts: ["reject", "approve"],
    ...over,
  } as unknown as Row;
}

const SET_UP = { exists: true, applicationsOpen: true, entitlementAdapter: "customerTag", introText: "", staffEmail: "", adapters: ["customerTag", "shopifyB2B"], may: { setUp: false, update: true } };

function seed(over: { operator?: boolean; settings?: Record<string, unknown> } = {}): FakeSeed {
  const operator = over.operator === true;
  const staffActs = (acts: string[]) => (operator ? acts.filter((a) => a === "retryAccess") : acts);
  return {
    sites: [SITE],
    wholesale: {
      list: {
        readyToReview: { rows: READY, counts: COUNTS, next: "", truncated: false } as unknown as Row,
        approved: { rows: APPROVED, counts: COUNTS, next: "", truncated: false } as unknown as Row,
        revoked: { rows: REMOVED, counts: COUNTS, next: "", truncated: false } as unknown as Row,
      },
      get: {
        ...Object.fromEntries(READY.map((r) => [r.applicationId, detail(r, { operator, acts: staffActs(["reject", "approve"]) })])),
        [APPROVED[0]!.applicationId]: detail(APPROVED[0]!, {
          state: "approved", stage: "approved", operator,
          entitlement: { state: "failed", errorCode: "plan_ceiling", error: "catalog limit reached" },
          decisions: [{ transition: "approve", decidedBy: "v1:identity:user:u-carol", decidedByName: "Carol Reyes", note: "Known clinic, verified by phone.", createdAt: ago(20) }],
          acts: staffActs(["remove", "retryAccess"]),
        }),
        [REMOVED[0]!.applicationId]: detail(REMOVED[0]!, {
          state: "revoked", stage: "revoked", operator,
          entitlement: { state: "granted", errorCode: "", error: "" },
          decisions: [
            { transition: "revoke", decidedBy: "v1:identity:user:u-me", decidedByName: "Dana Lee", note: "Closed the account at their request.", createdAt: ago(2) },
            { transition: "approve", decidedBy: "v1:identity:user:u-carol", decidedByName: "Carol Reyes", note: "", createdAt: ago(58) },
          ],
          acts: ["retryAccess"],
        }),
      },
      settings: (over.settings ?? SET_UP) as unknown as Row,
    },
  };
}

const NOT_SET_UP = (setUp: boolean) => ({ exists: false, applicationsOpen: false, entitlementAdapter: "", introText: "", staffEmail: "", adapters: ["customerTag", "shopifyB2B"], may: { setUp, update: false } });

/** One of the store's own people: a member of its organization holding the wholesale part. */
const staff = (el: JSX.Element) => withSession(el, {
  role: "writer", userId: "u-me", everyAccount: false, accountIds: [ACCOUNT],
  capabilities: [...seededAccessFor("writer"), { verb: "execute", resource: "app:deployables/wholesale", effect: "allow", source: "group" }],
});
/** A cluster operator, who reads every store's applications and decides none. */
const operator = (el: JSX.Element) => withSession(el, { role: "owner", userId: "u-me" });

export const WHOLESALE_CLICKS: Record<string, string> = {
  "wholesale-application": "Open Northwind Dental Supply",
  "wholesale-access-failed": "Refine applications|Approved|Open Cedar Smile Studio",
  "wholesale-removed": "Refine applications|Removed|Open Maple Street Dental",
  "wholesale-settings": "Settings",
  "wholesale-not-set-up": "Settings",
  "wholesale-not-set-up-operator": "Settings",
};

export function wholesaleScenes(WindowBody: Frame) {
  const page = () => (
    <WindowBody fallback="Deployables">
      <DeployablePage
        site={siteFromRow(SITE)}
        pkg={null}
        credentials={[]}
        viewerUserId="u-me"
        nameOf={() => ""}
        can={ALL_PARTS}
        clusterDomain="memql.example.com"
        onBack={() => {}}
        onOpenSource={() => {}}
        initialDetail="wholesale"
      />
    </WindowBody>
  );
  return {
    wholesale: { seed: seed(), wrap: staff, render: page },
    "wholesale-operator": { seed: seed({ operator: true }), wrap: operator, render: page },
    "wholesale-application": { seed: seed(), wrap: staff, render: page },
    "wholesale-access-failed": { seed: seed(), wrap: staff, render: page },
    "wholesale-removed": { seed: seed(), wrap: staff, render: page },
    "wholesale-settings": { seed: seed(), wrap: staff, render: page },
    "wholesale-not-set-up": { seed: seed({ settings: NOT_SET_UP(false) }), wrap: staff, render: page },
    "wholesale-not-set-up-operator": { seed: seed({ operator: true, settings: NOT_SET_UP(true) }), wrap: operator, render: page },
  };
}
