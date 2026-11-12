import { rowArray, rowBool, rowNumber, rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// A store's wholesale applications, as the wholesale pack's builtins answer
// them (packs/wholesalepack: wholesaleList, wholesaleGet in reads.go;
// wholesaleStoreSettings in settings.go). Pure: the engine's answers in, what
// the Wholesale section draws out, so every reading is a unit test.
//
// A STAGE IS THE ENGINE'S, folded from the application's whole decision log,
// and so are the acts: wholesaleGet answers the ones open to this person on
// this application, and the page draws those it has a name for and no others.
//
// AN APPLICATION'S ID IS THE ENGINE'S, as a reply gave it (v1:-prefixed). It is
// carried and passed back whole, never built, parsed, shortened or compared.

export type WholesaleStage = "waitingForTerms" | "readyToReview" | "approved" | "rejected" | "revoked";
/** What the list is asked for: one stage, or every one. */
export type WholesaleFilter = WholesaleStage | "all";

const STAGES: readonly WholesaleStage[] = ["waitingForTerms", "readyToReview", "approved", "rejected", "revoked"];

/**
 * The stages Refine offers, the default first. Waiting for terms is offered
 * only while the store requires its terms: on a store that requires none, no
 * application waits for them (wholesalepack StageOf).
 */
export function refineStages(termsRequired: boolean): readonly WholesaleFilter[] {
  return termsRequired
    ? ["readyToReview", "waitingForTerms", "approved", "rejected", "revoked", "all"]
    : ["readyToReview", "approved", "rejected", "revoked", "all"];
}

/** The engine's acts this page has a name for (wholesalepack ActsFor). */
type KnownAct = "reject" | "approve" | "remove" | "retryAccess" | "resendTerms" | "resendApproved";

const ACT_LABEL: Readonly<Record<KnownAct, string>> = {
  reject: "Reject",
  approve: "Approve",
  remove: "Remove",
  retryAccess: "Retry access",
  resendTerms: "Resend terms email",
  resendApproved: "Resend approval email",
};

/**
 * The store gate's refusal of a store that is not set up
 * (wholesalepack NotSetUpSentence): every store-wide read and act answers it,
 * and every tab draws it as a state, never as an error to try again.
 */
export const NOT_SET_UP = "This store's wholesale program is not set up yet. The cluster's operators set it up.";

/** Whether a read failed because the store is not set up. */
export const isNotSetUp = (error: string): boolean => error.trim() === NOT_SET_UP;

/** A stage in words. The stored `revoked` is "Removed" everywhere (X21). */
export function stageWords(stage: WholesaleFilter | ""): string {
  switch (stage) {
    case "waitingForTerms":
      return "Waiting for terms";
    case "readyToReview":
      return "Ready to review";
    case "approved":
      return "Approved";
    case "rejected":
      return "Rejected";
    case "revoked":
      return "Removed";
    case "all":
      return "All";
    default:
      return "";
  }
}

/** What a list holding nothing is called, after a read that found nothing. */
export function emptyWords(filter: WholesaleFilter): string {
  return filter === "all" ? "No applications" : `Nothing ${stageWords(filter).toLowerCase()}`;
}

/** Why an entitlement did not land, in a sentence, for each of the engine's error codes. */
export function accessSentence(errorCode: string, error: string): string {
  switch (errorCode) {
    case "no_route":
      return "No access route is chosen in Settings.";
    case "no_settings":
      return "This store has no wholesale settings.";
    case "scope_missing":
      return "The store has not allowed MemQL to change customers. Its owner approves the new permissions from the store's admin.";
    case "plan_ceiling":
      return "The store's plan allows no more catalogs.";
    case "customer_refused":
    case "shopify_error":
      return `Shopify refused: ${error}`;
    default:
      return error;
  }
}

/** The bar's word for one of the engine's acts, or null for one this page does not know. */
export function actLabel(act: string): string | null {
  return Object.prototype.hasOwnProperty.call(ACT_LABEL, act) ? ACT_LABEL[act as KnownAct] : null;
}

/** A decision in words: what it left the application as. */
export function transitionWords(transition: string): string {
  switch (transition) {
    case "approve":
      return "Approved";
    case "reject":
      return "Rejected";
    case "revoke":
      return "Removed";
    default:
      return transition;
  }
}

/** The route's name in Settings; "" is none. */
export function routeWords(route: string): string {
  switch (route) {
    case "customerTag":
      return "Customer tag";
    case "shopifyB2B":
      return "Shopify B2B";
    case "":
      return "None";
    default:
      return route;
  }
}

/** "waited 3 days": how long a submitted application has waited. */
export function waitedWords(since: string, now: Date): string {
  const at = Date.parse(since);
  if (Number.isNaN(at)) return "";
  const days = Math.floor(Math.max(0, now.getTime() - at) / 86_400_000);
  if (days < 1) return "waited under a day";
  return days === 1 ? "waited 1 day" : `waited ${days} days`;
}

/** A day, in the reader's own calendar: "Nov 4, 2026". */
export function dayWords(value: string): string {
  const at = new Date(value.trim());
  if (value.trim() === "" || Number.isNaN(at.getTime())) return "";
  return at.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

// ---------------------------------------------------------------------------
// The replies
// ---------------------------------------------------------------------------

export interface WholesaleRow {
  /** The engine's id, whole. */
  id: string;
  companyName: string;
  applicantName: string;
  applicantEmail: string;
  stage: WholesaleStage | "";
  /** The two facts a client's own fields show first; empty when there are none. */
  primary: string;
  secondary: string;
  createdAt: string;
}

export interface WholesalePage {
  rows: WholesaleRow[];
  counts: Partial<Record<WholesaleStage, number>>;
  next: string;
  /** The store has more applications than one read covers; its oldest are left out. */
  truncated: boolean;
}

export interface WholesaleDecision {
  transition: string;
  decidedByName: string;
  note: string;
  createdAt: string;
}

export interface WholesaleEntitlement {
  state: string;
  errorCode: string;
  error: string;
}

/** An applicant's acceptance of the terms, as wholesaleGet answers it. */
export interface WholesaleAcceptance {
  signedName: string;
  acceptedAt: string;
  termsLabel: string;
  termsSha256: string;
  mapLabel: string;
  mapSha256: string;
  clientAddress: string;
  userAgent: string;
}

/** One email sent to the applicant, with the lane's record of its delivery. */
export interface WholesaleEmail {
  kind: string;
  attempt: number;
  /** sent, skipped, failed or pending: what the lane answered when it was sent. */
  state: string;
  /** The engine's sentence for a send that did not go. */
  reason: string;
  sentAt: string;
  /** The lane's own record, or null when it has none to read. */
  delivery: { status: string; skipReason: string; providerStatus: string; sentAt: string } | null;
}

export interface WholesaleApplication {
  application: {
    id: string;
    companyName: string;
    applicantName: string;
    applicantEmail: string;
    createdAt: string;
  };
  acceptance: WholesaleAcceptance | null;
  /** The terms link's dates, or null when none was sent. */
  termsLink: { expiresAt: string; expired: boolean } | null;
  /** Newest first. */
  emails: WholesaleEmail[];
  state: string;
  stage: WholesaleStage | "";
  details: { state: "shown" | "none" | "unreadable"; fields: { label: string; value: string }[] };
  /** Newest first. */
  decisions: WholesaleDecision[];
  entitlement: WholesaleEntitlement | null;
  operator: boolean;
  /** The engine's acts, every one it answered, known or not. */
  acts: string[];
}

export interface WholesaleSettingsChanges {
  applicationsOpen: boolean;
  /** "" is none. */
  entitlementAdapter: string;
  /** The address the store's people are told at; "" is none. */
  staffEmail: string;
  termsRequired: boolean;
  linkLifetimeDays: number;
  /** The effective sentence: the pack's default when the store named none. */
  acceptanceText: string;
  termsTemplateId: string;
  approvedTemplateId: string;
  rejectedTemplateId: string;
  revokedTemplateId: string;
}

/** The four email kinds a store chooses a template for, as Settings names them. */
export const EMAIL_TEMPLATES: readonly (readonly [keyof WholesaleSettingsChanges & `${string}TemplateId`, string])[] = [
  ["termsTemplateId", "Terms email"],
  ["approvedTemplateId", "Approval email"],
  ["rejectedTemplateId", "Rejection email"],
  ["revokedTemplateId", "Removal email"],
];

export interface WholesaleSettings extends WholesaleSettingsChanges {
  /** False until the cluster's operators set the store up. */
  exists: boolean;
  /** Whether the cluster's outbound allowlist admits the stored staff address. */
  staffEmailAdmitted: boolean;
  senderIdentityId: string;
  /** The published transactional templates this person may choose. */
  templates: { id: string; name: string }[];
  /** The routes this engine has. */
  adapters: string[];
  may: { setUp: boolean; update: boolean; setSender: boolean };
}

const asRow = (value: unknown): Row | null => (value && typeof value === "object" && !Array.isArray(value) ? (value as Row) : null);

/** A nested value the wire may carry parsed or as JSON text. */
function nested(row: Row | null, key: string): unknown {
  const v = row?.[key];
  if (typeof v !== "string") return v;
  const text = v.trim();
  if (!text.startsWith("[") && !text.startsWith("{")) return v;
  try {
    return JSON.parse(text);
  } catch {
    return v;
  }
}

const list = (row: Row | null, key: string): Row[] => {
  const v = nested(row, key);
  return (Array.isArray(v) ? v : rowArray(row, key) ?? []).map(asRow).filter((r): r is Row => r !== null);
};

const strings = (row: Row | null, key: string): string[] => {
  const v = nested(row, key);
  return (Array.isArray(v) ? v : []).filter((s): s is string => typeof s === "string");
};

const object = (row: Row | null, key: string): Row | null => asRow(nested(row, key)) ?? rowObject(row, key);

const whole = (row: Row | null, key: string): number => {
  const v = row?.[key];
  if (typeof v === "number" && Number.isFinite(v)) return Math.trunc(v);
  if (typeof v === "string" && /^-?\d+$/.test(v.trim())) return Number(v.trim());
  return rowNumber(row, key);
};

const flag = (row: Row | null, key: string): boolean => {
  const v = row?.[key];
  return v === true || v === "true" || rowBool(row, key);
};

const stageOf = (value: string): WholesaleStage | "" => ((STAGES as readonly string[]).includes(value) ? (value as WholesaleStage) : "");

/** wholesaleList's answer. */
export function wholesalePageFrom(row: Row | null): WholesalePage {
  const counts: Partial<Record<WholesaleStage, number>> = {};
  const raw = object(row, "counts");
  for (const s of STAGES) {
    if (raw !== null && raw[s] !== undefined) counts[s] = whole(raw, s);
  }
  return {
    rows: list(row, "rows").map((r) => ({
      id: rowString(r, "applicationId"),
      companyName: rowString(r, "companyName"),
      applicantName: rowString(r, "applicantName"),
      applicantEmail: rowString(r, "applicantEmail"),
      stage: stageOf(rowString(r, "stage")),
      primary: rowString(r, "primary"),
      secondary: rowString(r, "secondary"),
      createdAt: rowString(r, "createdAt"),
    })).filter((r) => r.id !== ""),
    counts,
    next: rowString(row, "next"),
    truncated: flag(row, "truncated"),
  };
}

/** How many applications a filter holds, when the engine counted them. */
export function countFor(counts: Partial<Record<WholesaleStage, number>>, filter: WholesaleFilter): number | undefined {
  if (filter !== "all") return counts[filter];
  const held = STAGES.map((s) => counts[s]).filter((n): n is number => n !== undefined);
  return held.length === 0 ? undefined : held.reduce((a, b) => a + b, 0);
}

/** wholesaleGet's answer. */
export function wholesaleApplicationFrom(row: Row | null): WholesaleApplication {
  const app = object(row, "application");
  const details = object(row, "details");
  const detailState = rowString(details, "state");
  const ent = object(row, "entitlement");
  const accepted = object(row, "acceptance");
  const link = object(row, "termsLink");
  return {
    application: {
      id: rowString(app, "id"),
      companyName: rowString(app, "companyName"),
      applicantName: rowString(app, "applicantName"),
      applicantEmail: rowString(app, "applicantEmail"),
      createdAt: rowString(app, "createdAt"),
    },
    acceptance: accepted === null ? null : {
      signedName: rowString(accepted, "signedName"),
      acceptedAt: rowString(accepted, "acceptedAt"),
      termsLabel: rowString(accepted, "termsLabel"),
      termsSha256: rowString(accepted, "termsSha256"),
      mapLabel: rowString(accepted, "mapLabel"),
      mapSha256: rowString(accepted, "mapSha256"),
      clientAddress: rowString(accepted, "clientAddress"),
      userAgent: rowString(accepted, "userAgent"),
    },
    termsLink: link === null ? null : { expiresAt: rowString(link, "expiresAt"), expired: flag(link, "expired") },
    emails: list(row, "emails").map((e) => {
      const d = object(e, "delivery");
      return {
        kind: rowString(e, "kind"),
        attempt: whole(e, "attempt"),
        state: rowString(e, "state"),
        reason: rowString(e, "reason"),
        sentAt: rowString(e, "sentAt"),
        delivery: d === null ? null : { status: rowString(d, "status"), skipReason: rowString(d, "skipReason"), providerStatus: rowString(d, "providerStatus"), sentAt: rowString(d, "sentAt") },
      };
    }),
    state: rowString(row, "state"),
    stage: stageOf(rowString(row, "stage")),
    details: {
      state: detailState === "shown" || detailState === "unreadable" ? detailState : "none",
      fields: list(details, "fields").map((f) => ({ label: rowString(f, "label") || rowString(f, "name"), value: rowString(f, "value") })),
    },
    decisions: list(row, "decisions").map((d) => ({
      transition: rowString(d, "transition"),
      decidedByName: rowString(d, "decidedByName"),
      note: rowString(d, "note"),
      createdAt: rowString(d, "createdAt"),
    })),
    entitlement: ent === null ? null : { state: rowString(ent, "state"), errorCode: rowString(ent, "errorCode"), error: rowString(ent, "error") },
    operator: flag(row, "operator"),
    acts: strings(row, "acts"),
  };
}

/** wholesaleStoreSettings' answer. */
export function wholesaleSettingsFrom(row: Row | null): WholesaleSettings {
  const may = object(row, "may");
  return {
    exists: flag(row, "exists"),
    applicationsOpen: flag(row, "applicationsOpen"),
    entitlementAdapter: rowString(row, "entitlementAdapter"),
    staffEmail: rowString(row, "staffEmail"),
    staffEmailAdmitted: flag(row, "staffEmailAdmitted"),
    termsRequired: flag(row, "termsRequired"),
    linkLifetimeDays: whole(row, "linkLifetimeDays"),
    acceptanceText: rowString(row, "acceptanceText"),
    senderIdentityId: rowString(row, "senderIdentityId"),
    termsTemplateId: rowString(row, "termsTemplateId"),
    approvedTemplateId: rowString(row, "approvedTemplateId"),
    rejectedTemplateId: rowString(row, "rejectedTemplateId"),
    revokedTemplateId: rowString(row, "revokedTemplateId"),
    templates: list(row, "templates").map((t) => ({ id: rowString(t, "id"), name: rowString(t, "name") || rowString(t, "id") })).filter((t) => t.id !== ""),
    adapters: strings(row, "adapters"),
    may: { setUp: flag(may, "setUp"), update: flag(may, "update"), setSender: flag(may, "setSender") },
  };
}

/** Where a buyer's access stands, in one line. */
export function accessWords(entitlement: WholesaleEntitlement | null): string {
  if (entitlement === null) return "Not granted yet";
  if (entitlement.state === "granted") return "Trade prices on";
  if (entitlement.state === "revoked") return "Trade prices off";
  if (entitlement.errorCode !== "") return accessSentence(entitlement.errorCode, entitlement.error);
  return "Not granted yet";
}

/** Lower-cases a sentence's first letter, to stand after a colon. */
const afterColon = (sentence: string): string => sentence.charAt(0).toLowerCase() + sentence.slice(1);

// ---------------------------------------------------------------------------
// The buyers (wholesaleBuyers)
// ---------------------------------------------------------------------------

export interface WholesaleBuyer {
  /** The application's id, whole. */
  id: string;
  companyName: string;
  applicantEmail: string;
  /** The entitlement's state, or none when nothing was granted. */
  accessState: string;
  errorCode: string;
  grantedAt: string;
}

export interface WholesaleBuyersPage {
  rows: WholesaleBuyer[];
  next: string;
  truncated: boolean;
}

/** wholesaleBuyers' answer. */
export function wholesaleBuyersFrom(row: Row | null): WholesaleBuyersPage {
  return {
    rows: list(row, "rows").map((r) => ({
      id: rowString(r, "applicationId"),
      companyName: rowString(r, "companyName"),
      applicantEmail: rowString(r, "applicantEmail"),
      accessState: rowString(r, "accessState"),
      errorCode: rowString(r, "errorCode"),
      grantedAt: rowString(r, "grantedAt"),
    })).filter((r) => r.id !== ""),
    next: rowString(row, "next"),
    truncated: flag(row, "truncated"),
  };
}

/**
 * A buyer's access in words, on their row: on since the day it was granted,
 * or not on yet and why, the error code's sentence after the colon. The list
 * carries the code and not Shopify's own words, so a refusal by Shopify is
 * said without them; the buyer's page says them.
 */
export function buyerAccessWords(buyer: WholesaleBuyer): string {
  if (buyer.accessState === "granted") {
    const since = dayWords(buyer.grantedAt);
    return since ? `Trade prices on since ${since}` : "Trade prices on";
  }
  if (buyer.accessState === "revoked") return "Trade prices off";
  if (buyer.errorCode === "customer_refused" || buyer.errorCode === "shopify_error") return "Not on yet: Shopify refused.";
  if (buyer.errorCode !== "") return `Not on yet: ${afterColon(accessSentence(buyer.errorCode, ""))}`;
  return "Not on yet";
}

// ---------------------------------------------------------------------------
// The documents (wholesaleDocuments)
// ---------------------------------------------------------------------------

export type DocumentKind = "terms" | "map" | "priceSheet";

export const DOCUMENT_KINDS: readonly (readonly [DocumentKind, string])[] = [
  ["terms", "Wholesale terms"],
  ["map", "MAP policy"],
  ["priceSheet", "Price sheet"],
];

export const documentKindWords = (kind: string): string => DOCUMENT_KINDS.find(([k]) => k === kind)?.[1] ?? kind;

/**
 * The largest body a version may hold, in bytes (wholesalepack
 * maxDocumentBytes, wholesalePublishDocument's description): a file past it is
 * refused in the browser, as the engine would refuse it.
 */
export const MAX_DOCUMENT_BYTES = 204_800;

/** The engine's own sentence for a body it refuses by size (documents.go). */
export const documentSizeSentence = (bytes: number): string => `a document's body is 1 to ${MAX_DOCUMENT_BYTES} bytes; this one is ${bytes}`;

export interface DocumentVersion {
  id: string;
  kind: string;
  label: string;
  publishedAt: string;
  publishedByName: string;
  sha256: string;
  /** Empty in a history, which carries no body. */
  body: string;
}

export interface DocumentKindView {
  kind: DocumentKind;
  current: DocumentVersion | null;
  /** Every version, newest first, the current one included. */
  history: DocumentVersion[];
}

function versionFrom(v: Row | null, kind = ""): DocumentVersion {
  return {
    id: rowString(v, "id"),
    kind: rowString(v, "kind") || kind,
    label: rowString(v, "label"),
    publishedAt: rowString(v, "publishedAt"),
    publishedByName: rowString(v, "publishedByName"),
    sha256: rowString(v, "sha256"),
    body: rowString(v, "body"),
  };
}

/** wholesaleDocuments' answer: the three kinds, in the engine's order. */
export function wholesaleDocumentsFrom(row: Row | null): DocumentKindView[] {
  const kinds = list(row, "kinds");
  return DOCUMENT_KINDS.map(([kind]) => {
    const held = kinds.find((k) => rowString(k, "kind") === kind) ?? null;
    const current = object(held, "current");
    return {
      kind,
      current: current === null ? null : versionFrom(current, kind),
      history: list(held, "history").map((v) => versionFrom(v, kind)).filter((v) => v.id !== ""),
    };
  });
}

/** wholesaleDocuments' answer for one version. */
export function wholesaleDocumentVersionFrom(row: Row | null): DocumentVersion {
  return versionFrom(object(row, "version"));
}

/** The eight characters of a fingerprint shown on screen. */
export const fingerprint = (sha256: string): string => sha256.trim().slice(0, 8);

// ---------------------------------------------------------------------------
// One application's terms and emails
// ---------------------------------------------------------------------------

/** What the Terms section says first: accepted, waiting with the link's day, or expired. */
export function termsWords(acceptance: WholesaleAcceptance | null, link: { expiresAt: string; expired: boolean } | null): string {
  if (acceptance !== null) return `Accepted by ${acceptance.signedName} on ${dayWords(acceptance.acceptedAt)}`;
  if (link === null) return "";
  if (link.expired) return "The link has expired.";
  return `Not accepted yet. The link expires on ${dayWords(link.expiresAt)}.`;
}

const EMAIL_KINDS: readonly (readonly [string, string])[] = [
  ["terms", "Terms email"],
  ["approved", "Approval email"],
  ["rejected", "Rejection email"],
  ["revoked", "Removal email"],
];

/** Each kind's newest attempt, in the order an applicant meets them. */
export function latestEmails(emails: readonly WholesaleEmail[]): { kind: string; name: string; email: WholesaleEmail }[] {
  return EMAIL_KINDS.flatMap(([kind, name]) => {
    const newest = emails.filter((e) => e.kind === kind).reduce<WholesaleEmail | null>((a, e) => (a === null || e.attempt > a.attempt ? e : a), null);
    return newest === null ? [] : [{ kind, name, email: newest }];
  });
}

/**
 * An email's line: what the provider said of its delivery when it said
 * something, else when it was sent, and for one that did not go, the engine's
 * reason after "Not sent".
 */
export function emailWords(email: WholesaleEmail): string {
  const d = email.delivery;
  if (d !== null && d.status === "skipped") return d.skipReason ? `Not sent: ${d.skipReason}` : "Not sent";
  if (email.state !== "sent") return email.reason ? `Not sent: ${email.reason}` : "Not sent";
  if (d?.providerStatus === "delivered") return "Delivered";
  if (d?.providerStatus === "bounced") return "Bounced: the address does not accept mail";
  const at = dayWords(d?.sentAt || email.sentAt);
  return at ? `Sent on ${at}` : "Sent";
}
