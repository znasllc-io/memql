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
//   view=wholesale-buyers                  the buyers, one on and one not on yet
//   view=wholesale-buyers-empty            a store with no buyers yet
//   view=wholesale-add-buyer               adding a buyer by hand, filled in
//   view=wholesale-add-buyer-refused       the same, refused: the address already applied
//   view=wholesale-documents               the three documents, the price sheet not published
//   view=wholesale-publish                 a new version loaded from a file, with its preview
//   view=wholesale-version                 an earlier version, read-only
//   view=wholesale-accepted                an approved application: its terms accepted and its emails
//   view=wholesale-waiting                 one waiting for terms: its link, an email not sent, Resend
//   view=wholesale-expired                 one whose terms link expired, the email bounced
//   view=wholesale-settings-admitted       the settings with a staff address the outbox delivers to
//   view=wholesale-not-set-up-buyers       the not-set-up state on the Buyers tab
//   view=wholesale-not-set-up-documents    the not-set-up state on the Documents tab
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
  { applicationId: id("kestrel"), companyName: "Kestrel Cycles", applicantName: "Avery Stone", applicantEmail: "avery@kestrel.example.com", stage: "readyToReview", primary: "Repair workshop", secondary: "12 locations", createdAt: ago(9) },
  { applicationId: id("harbor"), companyName: "Harbor Wheelworks and Touring Bicycle Cooperative", applicantName: "Sam Okafor", applicantEmail: "sam.okafor@harbor-wheelworks.example.com", stage: "readyToReview", primary: "Retail shop", secondary: "3 locations", createdAt: ago(4) },
  { applicationId: id("blue"), companyName: "Bluebird Bikes", applicantName: "Jordan Reyes", applicantEmail: "jordan@bluebird.example.com", stage: "readyToReview", primary: "", secondary: "", createdAt: ago(0.2) },
];
const APPROVED = [
  { applicationId: id("cedar"), companyName: "Cedar Spoke Studio", applicantName: "Riley Chen", applicantEmail: "riley@cedarspoke.example.com", stage: "approved", primary: "Repair workshop", secondary: "1 location", createdAt: ago(21) },
];
const REMOVED = [
  { applicationId: id("maple"), companyName: "Maple Street Bicycles", applicantName: "Casey Morgan", applicantEmail: "casey@maplestreet-bikes.example.com", stage: "revoked", primary: "Repair workshop", secondary: "2 locations", createdAt: ago(60) },
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
        ? [{ name: "shopType", label: "Shop type", value: row.primary }, { name: "locations", label: "Locations", value: row.secondary }, { name: "website", label: "Website", value: "https://www.example.com" }]
        : [],
    },
    decisions: [],
    entitlement: null,
    operator: false,
    acts: ["reject", "approve"],
    ...over,
  } as unknown as Row;
}

const sha = (head: string) => head + "0123456789abcdef".repeat(4).slice(0, 56);
const TERMS_SHA = sha("3f2a9c1e");
const MAP_SHA = sha("a1b2c3d4");
const OLD_SHA = sha("77aa01cd");

const SET_UP = {
  exists: true, applicationsOpen: true, entitlementAdapter: "customerTag", introText: "",
  staffEmail: "orders@kestrel-trade.example.com", staffEmailAdmitted: false, termsRequired: true, linkLifetimeDays: 30,
  acceptanceText: "I have read and accept the Wholesale Terms and Conditions and the MAP Policy shown above.",
  senderIdentityId: "", termsTemplateId: "tpl-invite", approvedTemplateId: "tpl-welcome", rejectedTemplateId: "", revokedTemplateId: "",
  templates: [{ id: "tpl-invite", name: "Trade terms invitation" }, { id: "tpl-welcome", name: "Trade welcome" }, { id: "tpl-closing", name: "Trade account closed" }],
  documents: { terms: { label: "Trade terms 2026", sha256: TERMS_SHA }, map: { label: "MAP policy 2026", sha256: MAP_SHA }, priceSheet: null },
  adapters: ["customerTag", "shopifyB2B"], may: { setUp: false, update: true, setSender: false },
};

/** The Markdown `file:document` loads into the publish page. */
export const QA_DOCUMENT = "# Trade terms\n\nThese terms apply to every order placed at trade prices.\n\n## Payment\n\n- Net **30** days from the invoice date.\n- Orders under the minimum are charged at retail.\n\n## Shipping\n\nOrders ship within *two working days*. See [the shipping page](https://www.example.com/shipping).\n";

const DOCUMENTS = {
  kinds: [
    {
      kind: "terms",
      current: { id: "doc-terms-2", label: "Trade terms 2026", publishedAt: ago(9), publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: TERMS_SHA, body: "# Trade terms" },
      history: [
        { id: "doc-terms-2", label: "Trade terms 2026", publishedAt: ago(9), publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: TERMS_SHA },
        { id: "doc-terms-1", label: "Trade terms 2025, with the spring shipping schedule and returns window", publishedAt: ago(320), publishedBy: "v1:identity:user:u-carol", publishedByName: "Carol Reyes", sha256: OLD_SHA },
      ],
    },
    {
      kind: "map",
      current: { id: "doc-map-1", label: "MAP policy 2026", publishedAt: ago(9), publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: MAP_SHA, body: "No advertised price below MAP." },
      history: [{ id: "doc-map-1", label: "MAP policy 2026", publishedAt: ago(9), publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: MAP_SHA }],
    },
    { kind: "priceSheet", current: null, history: [] },
  ],
};

const VERSIONS = {
  "doc-terms-1": {
    id: "doc-terms-1", kind: "terms", label: "Trade terms 2025, with the spring shipping schedule and returns window", publishedAt: ago(320),
    publishedBy: "v1:identity:user:u-carol", publishedByName: "Carol Reyes", sha256: OLD_SHA,
    body: "# Trade terms\n\nThese terms apply to every order placed at trade prices.\n\n## Payment\n\nNet **45** days from the invoice date.\n\n## Returns\n\nUnopened stock may be returned within 60 days.\n",
  },
};

const BUYERS = {
  rows: [
    { applicationId: id("cedar"), companyName: "Cedar Spoke Studio", applicantEmail: "riley@cedarspoke.example.com", accessState: "failed", errorCode: "plan_ceiling", grantedAt: "", shopifyCustomerId: "" },
    { applicationId: id("larkspur"), companyName: "Larkspur Mountain Bike Outfitters and Repair Collective", applicantEmail: "accounts.payable@larkspur-mountain-outfitters.example.com", accessState: "granted", errorCode: "", grantedAt: ago(12), shopifyCustomerId: "" },
    { applicationId: id("finch"), companyName: "Finch Road Bikes", applicantEmail: "finch@finchroad.example.com", accessState: "none", errorCode: "", grantedAt: "", shopifyCustomerId: "" },
  ],
  next: "",
  truncated: false,
};

const ACCEPTED = {
  signedName: "Riley Chen", acceptedAt: ago(19), termsLabel: "Trade terms 2026", termsSha256: TERMS_SHA, mapLabel: "MAP policy 2026", mapSha256: MAP_SHA,
  clientAddress: "203.0.113.7", userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
};

function seed(over: { operator?: boolean; settings?: Record<string, unknown>; buyers?: Record<string, unknown>; addError?: string; notSetUp?: boolean } = {}): FakeSeed {
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
          decisions: [{ transition: "approve", decidedBy: "v1:identity:user:u-carol", decidedByName: "Carol Reyes", note: "Known shop, verified by phone.", createdAt: ago(20) }],
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
      buyers: (over.buyers ?? BUYERS) as unknown as Row,
      documents: DOCUMENTS as unknown as Row,
      versions: VERSIONS as unknown as Record<string, Row>,
      ...(over.addError !== undefined ? { addError: over.addError } : {}),
      ...(over.notSetUp ? { buyersError: NOT_SET_UP_SENTENCE, documentsError: NOT_SET_UP_SENTENCE, listError: NOT_SET_UP_SENTENCE } : {}),
    },
  };
}

const NOT_SET_UP_SENTENCE = "wholesale: This store's wholesale program is not set up yet. The cluster's operators set it up.";

/** The three applications the terms and emails views open, keyed into the approved list. */
function termsSeed(): FakeSeed {
  const base = seed();
  const held = base.wholesale!;
  const waiting = { applicationId: id("wren"), companyName: "Wren Cycle Works", applicantName: "Morgan Ellis", applicantEmail: "morgan@wrencycles.example.com", stage: "waitingForTerms", primary: "Retail shop", secondary: "1 location", createdAt: ago(3) };
  const expired = { applicationId: id("heron"), companyName: "Heron Bicycle Supply", applicantName: "Quinn Patel", applicantEmail: "quinn@heron-supply.example.com", stage: "waitingForTerms", primary: "Retail shop", secondary: "2 locations", createdAt: ago(40) };
  held.list = { ...held.list, waitingForTerms: { rows: [waiting, expired], counts: { ...COUNTS, waitingForTerms: 2 }, next: "", truncated: false } as unknown as Row };
  held.get = {
    ...held.get,
    [APPROVED[0]!.applicationId]: detail(APPROVED[0]!, {
      state: "approved", stage: "approved",
      entitlement: { state: "granted", errorCode: "", error: "" },
      acceptance: ACCEPTED,
      termsLink: { expiresAt: ago(-11), expired: false },
      decisions: [{ transition: "approve", decidedBy: "v1:identity:user:u-carol", decidedByName: "Carol Reyes", note: "Known shop, verified by phone.", createdAt: ago(18) }],
      emails: [
        { kind: "approved", attempt: 1, state: "sent", reason: "", sentAt: ago(18), requestedBy: "", delivery: { status: "sent", skipReason: "", providerStatus: "delivered", sentAt: ago(18) } },
        { kind: "terms", attempt: 1, state: "sent", reason: "", sentAt: ago(21), requestedBy: "", delivery: { status: "sent", skipReason: "", providerStatus: "delivered", sentAt: ago(21) } },
      ],
      acts: ["remove", "resendApproved"],
    }),
    [waiting.applicationId]: detail(waiting as never, {
      stage: "waitingForTerms",
      termsLink: { expiresAt: ago(-27), expired: false },
      emails: [
        { kind: "terms", attempt: 2, state: "skipped", reason: "\u201cTrade terms invitation\u201d is not a published transactional template.", sentAt: "", requestedBy: "v1:identity:user:u-me", delivery: null },
        { kind: "terms", attempt: 1, state: "sent", reason: "", sentAt: ago(3), requestedBy: "", delivery: { status: "sent", skipReason: "", providerStatus: "", sentAt: ago(3) } },
      ],
      acts: ["reject", "resendTerms"],
    }),
    [expired.applicationId]: detail(expired as never, {
      stage: "waitingForTerms",
      termsLink: { expiresAt: ago(10), expired: true },
      emails: [{ kind: "terms", attempt: 1, state: "sent", reason: "", sentAt: ago(40), requestedBy: "", delivery: { status: "sent", skipReason: "", providerStatus: "bounced", sentAt: ago(40) } }],
      acts: ["reject", "resendTerms"],
    }),
  };
  return base;
}

const NOT_SET_UP = (setUp: boolean) => ({ exists: false, applicationsOpen: false, entitlementAdapter: "", introText: "", staffEmail: "", adapters: ["customerTag", "shopifyB2B"], templates: [], may: { setUp, update: false, setSender: setUp } });

/** One of the store's own people: a member of its organization holding the wholesale part. */
const staff = (el: JSX.Element) => withSession(el, {
  role: "writer", userId: "u-me", everyAccount: false, accountIds: [ACCOUNT],
  capabilities: [...seededAccessFor("writer"), { verb: "execute", resource: "app:deployables/wholesale", effect: "allow", source: "group" }],
});
/** A cluster operator, who reads every store's applications and decides none. */
const operator = (el: JSX.Element) => withSession(el, { role: "owner", userId: "u-me" });

export const WHOLESALE_CLICKS: Record<string, string> = {
  "wholesale-application": "Open Kestrel Cycles",
  "wholesale-access-failed": "Refine applications|Approved|Open Cedar Spoke Studio",
  "wholesale-removed": "Refine applications|Removed|Open Maple Street Bicycles",
  "wholesale-settings": "Settings",
  "wholesale-not-set-up": "Settings",
  "wholesale-not-set-up-operator": "Settings",
  "wholesale-buyers": "Buyers",
  "wholesale-buyers-empty": "Buyers",
  "wholesale-add-buyer": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|fill:Name=Sam Okafor|fill:Email=sam.okafor@harbor-wheelworks.example.com",
  "wholesale-add-buyer-refused": "Buyers|Add buyer|fill:Company=Kestrel Cycles|fill:Name=Avery Stone|fill:Email=avery@kestrel.example.com|Add and send the terms",
  "wholesale-documents": "Documents",
  "wholesale-publish": "Documents|Publish a new version|fill:Label=Trade terms 2027|file:document",
  "wholesale-version": "Documents|Open Trade terms 2025, with the spring shipping schedule and returns window",
  "wholesale-accepted": "Refine applications|Approved|Open Cedar Spoke Studio",
  "wholesale-waiting": "Refine applications|Waiting for terms|Open Wren Cycle Works",
  "wholesale-expired": "Refine applications|Waiting for terms|Open Heron Bicycle Supply",
  "wholesale-settings-admitted": "Settings",
  "wholesale-not-set-up-buyers": "Buyers",
  "wholesale-not-set-up-documents": "Documents",
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
    "wholesale-buyers": { seed: seed(), wrap: staff, render: page },
    "wholesale-buyers-empty": { seed: seed({ buyers: { rows: [], next: "", truncated: false } }), wrap: staff, render: page },
    "wholesale-add-buyer": { seed: seed(), wrap: staff, render: page },
    "wholesale-add-buyer-refused": { seed: seed({ addError: "wholesale: This address already has an application on this store." }), wrap: staff, render: page },
    "wholesale-documents": { seed: seed(), wrap: staff, render: page },
    "wholesale-publish": { seed: seed(), wrap: staff, render: page },
    "wholesale-version": { seed: seed(), wrap: staff, render: page },
    "wholesale-accepted": { seed: termsSeed(), wrap: staff, render: page },
    "wholesale-waiting": { seed: termsSeed(), wrap: staff, render: page },
    "wholesale-expired": { seed: termsSeed(), wrap: staff, render: page },
    "wholesale-settings-admitted": { seed: seed({ settings: { ...SET_UP, staffEmailAdmitted: true } }), wrap: staff, render: page },
    "wholesale-not-set-up-buyers": { seed: seed({ settings: NOT_SET_UP(false), notSetUp: true }), wrap: staff, render: page },
    "wholesale-not-set-up-documents": { seed: seed({ settings: NOT_SET_UP(false), notSetUp: true }), wrap: staff, render: page },
  };
}
