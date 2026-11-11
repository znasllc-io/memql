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
 * The stages Refine offers in this release, the default first. Waiting for
 * terms is not among them: no application can wait for terms before a store
 * has published any.
 */
export const REFINE_STAGES: readonly WholesaleFilter[] = ["readyToReview", "approved", "rejected", "revoked", "all"];

/** The engine's acts this page has a name for (wholesalepack ActsFor). */
type KnownAct = "reject" | "approve" | "remove" | "retryAccess";

const ACT_LABEL: Readonly<Record<KnownAct, string>> = {
  reject: "Reject",
  approve: "Approve",
  remove: "Remove",
  retryAccess: "Retry access",
};

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

export interface WholesaleApplication {
  application: {
    id: string;
    companyName: string;
    applicantName: string;
    applicantEmail: string;
    createdAt: string;
  };
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
}

export interface WholesaleSettings extends WholesaleSettingsChanges {
  /** False until the cluster's operators set the store up. */
  exists: boolean;
  /** The routes this engine has. */
  adapters: string[];
  may: { setUp: boolean; update: boolean };
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
  return {
    application: {
      id: rowString(app, "id"),
      companyName: rowString(app, "companyName"),
      applicantName: rowString(app, "applicantName"),
      applicantEmail: rowString(app, "applicantEmail"),
      createdAt: rowString(app, "createdAt"),
    },
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
    adapters: strings(row, "adapters"),
    may: { setUp: flag(may, "setUp"), update: flag(may, "update") },
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
