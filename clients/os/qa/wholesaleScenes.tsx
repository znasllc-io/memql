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
//   view=wholesale-add-buyer               the Add buyer wizard, its Company answered
//   view=wholesale-add-buyer-name          its second step, the Buyer's name answered
//   view=wholesale-add-buyer-email         its last step, Add and send the terms on the floor
//   view=wholesale-add-buyer-refused       the same, refused on the Email step: the address already applied
//   view=wholesale-add-buyer-no-email      the last step where the engine says no terms email would be sent
//   view=wholesale-add-buyer-no-terms      the last step on a store that requires no terms
//   view=wholesale-documents               the three documents, the price sheet not published
//   view=wholesale-publish                 the Publish wizard's Label step
//   view=wholesale-publish-text            its Text step, a file loaded
//   view=wholesale-publish-preview         its Preview step, Publish on the floor
//   view=wholesale-publish-refused         the same, the engine's refusal on the Preview step
//   view=wholesale-add-buyer-checking      Add buyer after a lost answer, while the store is read again
//   view=wholesale-add-buyer-there         ... the buyer is there (Done)
//   view=wholesale-add-buyer-absent        ... the buyer is not there (the act again)
//   view=wholesale-add-buyer-unread        ... the store could not be read (Check again)
//   view=wholesale-publish-checking        Publish after a lost answer, while the documents are read again
//   view=wholesale-publish-there           ... the version is there (Done)
//   view=wholesale-publish-absent          ... the version is not there (Publish again)
//   view=wholesale-publish-unread          ... the documents could not be read (Check again)
//   view=wholesale-documents-no-fingerprint an earlier version the engine recorded no fingerprint for
//   view=wholesale-emails                  an application's emails in every state, its resends in the section
//   view=wholesale-records-refused         the settings after a refused export, said in Records
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
  sender: { ready: true, fromName: "Example Cycles Trade", address: "trade@example.com", reason: "" },
  emailsWouldSend: {
    terms: { wouldSend: true, reason: "" }, approved: { wouldSend: true, reason: "" },
    rejected: { wouldSend: false, reason: "no rejected email template is chosen" }, revoked: { wouldSend: false, reason: "no revoked email template is chosen" },
  },
  templates: [{ id: "tpl-invite", name: "Trade terms invitation" }, { id: "tpl-welcome", name: "Trade welcome" }, { id: "tpl-closing", name: "Trade account closed" }],
  documents: { terms: { label: "Trade terms 2026", sha256: TERMS_SHA }, map: { label: "MAP policy 2026", sha256: MAP_SHA }, priceSheet: null },
  adapters: ["customerTag", "shopifyB2B"], may: { setUp: false, update: true, setSender: false, publish: true, export: true },
};

/** The Markdown `file:document` loads into the publish page. */
export const QA_DOCUMENT = "# Trade terms\n\nThese terms apply to every order placed at trade prices.\n\n## Payment\n\n- Net **30** days from the invoice date.\n- Orders under the minimum are charged at retail.\n\n## Shipping\n\nOrders ship within *two working days*. See [the shipping page](https://www.example.com/shipping).\n\n---\n\n## Returns\n\nUnopened stock may be returned within 60 days.\n";

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
    { applicationId: id("cedar"), companyName: "Cedar Spoke Studio", applicantEmail: "riley@cedarspoke.example.com", accessState: "failed", errorCode: "plan_ceiling", error: "The store's plan has reached its catalog limit.", grantedAt: "", shopifyCustomerId: "" },
    { applicationId: id("larkspur"), companyName: "Larkspur Mountain Bike Outfitters and Repair Collective", applicantEmail: "accounts.payable@larkspur-mountain-outfitters.example.com", accessState: "granted", errorCode: "", grantedAt: ago(12), shopifyCustomerId: "" },
    { applicationId: id("finch"), companyName: "Finch Road Bikes", applicantEmail: "finch@finchroad.example.com", accessState: "none", errorCode: "", error: "", grantedAt: "", shopifyCustomerId: "" },
  ],
  next: "",
  truncated: false,
  operator: false,
};

const ACCEPTED = {
  signedName: "Riley Chen", acceptedAt: ago(19), termsLabel: "Trade terms 2026", termsSha256: TERMS_SHA, mapLabel: "MAP policy 2026", mapSha256: MAP_SHA,
  clientAddress: "203.0.113.7", userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
};

type Lose = NonNullable<NonNullable<FakeSeed["wholesale"]>["lose"]>;

function seed(over: { operator?: boolean; settings?: Record<string, unknown>; buyers?: Record<string, unknown>; documents?: Record<string, unknown>; addError?: string; publishError?: string; exportError?: string; lose?: Lose; notSetUp?: boolean } = {}): FakeSeed {
  const operator = over.operator === true;
  const staffActs = (acts: string[]) => (operator ? acts.filter((a) => a === "retryAccess") : acts);
  return {
    sites: [SITE],
    wholesale: {
      list: {
        readyToReview: { rows: READY, counts: COUNTS, next: "", truncated: false, operator } as unknown as Row,
        approved: { rows: APPROVED, counts: COUNTS, next: "", truncated: false, operator } as unknown as Row,
        revoked: { rows: REMOVED, counts: COUNTS, next: "", truncated: false, operator } as unknown as Row,
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
      buyers: (over.buyers ?? { ...BUYERS, operator }) as unknown as Row,
      documents: (over.documents ?? DOCUMENTS) as unknown as Row,
      versions: VERSIONS as unknown as Record<string, Row>,
      ...(over.addError !== undefined ? { addError: over.addError } : {}),
      ...(over.publishError !== undefined ? { publishError: over.publishError } : {}),
      ...(over.exportError !== undefined ? { exportError: over.exportError } : {}),
      ...(over.lose !== undefined ? { lose: over.lose, addedId: id("harbor-added") } : {}),
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

/** One application whose emails stand in every state the page words, with its resends offered. */
function emailsSeed(): FakeSeed {
  const base = termsSeed();
  const held = base.wholesale!;
  const lane = (providerStatus: string, days: number) => ({ status: "sent", skipReason: "", providerStatus, sentAt: ago(days) });
  held.get = {
    ...held.get,
    [APPROVED[0]!.applicationId]: detail(APPROVED[0]!, {
      state: "approved", stage: "approved",
      entitlement: { state: "granted", errorCode: "", error: "" },
      acceptance: { ...ACCEPTED, clientAddress: "", userAgent: "" },
      termsLink: { expiresAt: ago(-11), expired: false },
      decisions: [{ transition: "approve", decidedBy: "v1:identity:user:u-carol", decidedByName: "Carol Reyes", note: "", createdAt: ago(18) }],
      emails: [
        { kind: "approved", attempt: 4, state: "deferred", reason: "The email service is waiting to send this email.", sentAt: "", requestedBy: "v1:identity:user:u-me", delivery: { status: "pending", skipReason: "", providerStatus: "", sentAt: "" } },
        { kind: "approved", attempt: 3, state: "held", reason: "The sender chosen for this store is turned off.", sentAt: "", requestedBy: "v1:identity:user:u-me", delivery: null },
        { kind: "approved", attempt: 2, state: "uncertain", reason: "The email service did not say whether it accepted this email; it may have been delivered, so it is not sent again automatically.", sentAt: "", requestedBy: "", delivery: null },
        { kind: "approved", attempt: 1, state: "failed", reason: "The email service recorded this email as failed.", sentAt: "", requestedBy: "", delivery: null },
        { kind: "terms", attempt: 8, state: "sent", reason: "", sentAt: ago(19), requestedBy: "", delivery: lane("failed", 19) },
        { kind: "terms", attempt: 7, state: "sent", reason: "", sentAt: ago(20), requestedBy: "", delivery: lane("filtered-spam", 20) },
        { kind: "terms", attempt: 6, state: "sent", reason: "", sentAt: ago(21), requestedBy: "", delivery: lane("quarantined", 21) },
        { kind: "terms", attempt: 5, state: "sent", reason: "", sentAt: ago(22), requestedBy: "", delivery: lane("suppressed", 22) },
        { kind: "terms", attempt: 4, state: "sent", reason: "", sentAt: ago(23), requestedBy: "", delivery: lane("bounced", 23) },
        { kind: "terms", attempt: 3, state: "sent", reason: "", sentAt: ago(24), requestedBy: "", delivery: lane("expanded", 24) },
        { kind: "terms", attempt: 2, state: "sent", reason: "", sentAt: ago(25), requestedBy: "", delivery: lane("delivered", 25) },
        { kind: "terms", attempt: 1, state: "sent", reason: "", sentAt: ago(26), requestedBy: "", delivery: null },
      ],
      acts: ["remove", "resendApproved"],
    }),
  };
  return base;
}

const NOT_SET_UP = (setUp: boolean) => ({ exists: false, applicationsOpen: false, entitlementAdapter: "", introText: "", staffEmail: "", adapters: ["customerTag", "shopifyB2B"], templates: [], may: { setUp, update: false, setSender: setUp, publish: false, export: false } });

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
  "wholesale-add-buyer": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative",
  "wholesale-add-buyer-name": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|Continue|fill:Name=Sam Okafor",
  "wholesale-add-buyer-email": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|Continue|fill:Name=Sam Okafor|Continue|fill:Email=sam.okafor@harbor-wheelworks.example.com",
  "wholesale-add-buyer-refused": "Buyers|Add buyer|fill:Company=Kestrel Cycles|Continue|fill:Name=Avery Stone|Continue|fill:Email=avery@kestrel.example.com|Add and send the terms",
  "wholesale-add-buyer-no-email": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|Continue|fill:Name=Sam Okafor|Continue|fill:Email=sam.okafor@harbor-wheelworks.example.com",
  "wholesale-add-buyer-no-terms": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|Continue|fill:Name=Sam Okafor|Continue|fill:Email=sam.okafor@harbor-wheelworks.example.com",
  "wholesale-documents": "Documents",
  "wholesale-publish": "Documents|Publish a new version of the wholesale terms|fill:Label=Trade terms 2027",
  "wholesale-publish-text": "Documents|Publish a new version of the wholesale terms|fill:Label=Trade terms 2027|Continue|file:document",
  "wholesale-publish-preview": "Documents|Publish a new version of the wholesale terms|fill:Label=Trade terms 2027|Continue|file:document|Continue",
  "wholesale-publish-refused": "Documents|Publish a new version of the wholesale terms|fill:Label=Trade terms 2027 for every account that buys at trade prices, with the spring shipping schedule and the new return window|Continue|fill:Text=# Trade terms\n\nThese terms apply to every order placed at trade prices.\n\n## Payment\n\nNet **30** days from the invoice date.|Continue|Publish",
  "wholesale-emails": "Refine applications|Approved|Open Cedar Spoke Studio",
  "wholesale-add-buyer-checking": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|Continue|fill:Name=Sam Okafor|Continue|fill:Email=sam.okafor@harbor-wheelworks.example.com|Add and send the terms",
  "wholesale-add-buyer-there": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|Continue|fill:Name=Sam Okafor|Continue|fill:Email=sam.okafor@harbor-wheelworks.example.com|Add and send the terms",
  "wholesale-add-buyer-absent": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|Continue|fill:Name=Sam Okafor|Continue|fill:Email=sam.okafor@harbor-wheelworks.example.com|Add and send the terms",
  "wholesale-add-buyer-unread": "Buyers|Add buyer|fill:Company=Harbor Wheelworks and Touring Bicycle Cooperative|Continue|fill:Name=Sam Okafor|Continue|fill:Email=sam.okafor@harbor-wheelworks.example.com|Add and send the terms",
  "wholesale-publish-checking": "Documents|Publish a new version of the wholesale terms|fill:Label=Trade terms 2027|Continue|fill:Text=# Trade terms\n\nThese terms apply to every order placed at trade prices.\n\n## Payment\n\nNet **30** days from the invoice date.|Continue|Publish",
  "wholesale-publish-there": "Documents|Publish a new version of the wholesale terms|fill:Label=Trade terms 2027|Continue|fill:Text=# Trade terms\n\nThese terms apply to every order placed at trade prices.\n\n## Payment\n\nNet **30** days from the invoice date.|Continue|Publish",
  "wholesale-publish-absent": "Documents|Publish a new version of the wholesale terms|fill:Label=Trade terms 2027|Continue|fill:Text=# Trade terms\n\nThese terms apply to every order placed at trade prices.\n\n## Payment\n\nNet **30** days from the invoice date.|Continue|Publish",
  "wholesale-publish-unread": "Documents|Publish a new version of the wholesale terms|fill:Label=Trade terms 2027|Continue|fill:Text=# Trade terms\n\nThese terms apply to every order placed at trade prices.\n\n## Payment\n\nNet **30** days from the invoice date.|Continue|Publish",
  "wholesale-documents-no-fingerprint": "Documents",
  "wholesale-records-refused": "Settings|Download every record",
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
    "wholesale-add-buyer-name": { seed: seed(), wrap: staff, render: page },
    "wholesale-add-buyer-email": { seed: seed(), wrap: staff, render: page },
    "wholesale-add-buyer-refused": { seed: seed({ addError: "wholesale: This address already has an application on this store." }), wrap: staff, render: page },
    "wholesale-add-buyer-no-email": { seed: seed({ settings: { ...SET_UP, emailsWouldSend: { ...SET_UP.emailsWouldSend, terms: { wouldSend: false, reason: "no sender is chosen for this store's emails; the cluster's operators choose it" } } } }), wrap: staff, render: page },
    "wholesale-add-buyer-no-terms": { seed: seed({ settings: { ...SET_UP, termsRequired: false, emailsWouldSend: { ...SET_UP.emailsWouldSend, terms: { wouldSend: false, reason: "terms are not required on this store" } } } }), wrap: staff, render: page },
    "wholesale-documents": { seed: seed(), wrap: staff, render: page },
    "wholesale-publish": { seed: seed(), wrap: staff, render: page },
    "wholesale-publish-text": { seed: seed(), wrap: staff, render: page },
    "wholesale-publish-preview": { seed: seed(), wrap: staff, render: page },
    "wholesale-publish-refused": { seed: seed({ publishError: "wholesale: a label is 1 to 120 characters; this one is 121" }), wrap: staff, render: page },
    "wholesale-emails": { seed: emailsSeed(), wrap: staff, render: page },
    "wholesale-add-buyer-checking": { seed: seed({ lose: { write: "add", landed: true, check: "hold" } }), wrap: staff, render: page },
    "wholesale-add-buyer-there": { seed: seed({ lose: { write: "add", landed: true, check: "answer" } }), wrap: staff, render: page },
    "wholesale-add-buyer-absent": { seed: seed({ lose: { write: "add", landed: false, check: "answer" } }), wrap: staff, render: page },
    "wholesale-add-buyer-unread": { seed: seed({ lose: { write: "add", landed: true, check: "fail" } }), wrap: staff, render: page },
    "wholesale-publish-checking": { seed: seed({ lose: { write: "publish", landed: true, check: "hold" } }), wrap: staff, render: page },
    "wholesale-publish-there": { seed: seed({ lose: { write: "publish", landed: true, check: "answer" } }), wrap: staff, render: page },
    "wholesale-publish-absent": { seed: seed({ lose: { write: "publish", landed: false, check: "answer" } }), wrap: staff, render: page },
    "wholesale-publish-unread": { seed: seed({ lose: { write: "publish", landed: true, check: "fail" } }), wrap: staff, render: page },
    "wholesale-documents-no-fingerprint": { seed: seed({ documents: { kinds: DOCUMENTS.kinds.map((k) => (k.kind === "terms" ? { ...k, history: k.history.map((v) => (v.id === "doc-terms-1" ? { ...v, sha256: "" } : v)) } : k)) } }), wrap: staff, render: page },
    "wholesale-records-refused": { seed: seed({ exportError: "wholesale: The export could not be read just now." }), wrap: staff, render: page },
    "wholesale-version": { seed: seed(), wrap: staff, render: page },
    "wholesale-accepted": { seed: termsSeed(), wrap: staff, render: page },
    "wholesale-waiting": { seed: termsSeed(), wrap: staff, render: page },
    "wholesale-expired": { seed: termsSeed(), wrap: staff, render: page },
    "wholesale-settings-admitted": { seed: seed({ settings: { ...SET_UP, staffEmailAdmitted: true } }), wrap: staff, render: page },
    "wholesale-not-set-up-buyers": { seed: seed({ settings: NOT_SET_UP(false), notSetUp: true }), wrap: staff, render: page },
    "wholesale-not-set-up-documents": { seed: seed({ settings: NOT_SET_UP(false), notSetUp: true }), wrap: staff, render: page },
  };
}
