import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// Storefront Testing shares the production bundle and has its own hostname
// and store. Candidate previews remain available for other deployable kinds.

/** One preview an operator opened. */
export interface PreviewGrantRow {
  id: string;
  siteId: string;
  ownerUserId: string;
  /** The version this preview was issued FOR. A preview stops resolving when it moves. */
  candidateRef: string;
  previewStoreId: string;
  issuedAt: string;
  expiresAt: string;
  /** Non-empty once somebody ended it. */
  revokedAt: string;
  /** EMPTY MEANS NOBODY OPENED IT, never "a long time ago". */
  lastSeenAt: string;
}

export function previewGrantFromRow(row: Row): PreviewGrantRow {
  return {
    id: rowString(row, "id"),
    siteId: rowString(row, "siteId"),
    ownerUserId: rowString(row, "ownerUserId"),
    candidateRef: rowString(row, "candidateRef"),
    previewStoreId: rowString(row, "previewStoreId"),
    issuedAt: rowString(row, "issuedAt"),
    expiresAt: rowString(row, "expiresAt"),
    revokedAt: rowString(row, "revokedAt"),
    lastSeenAt: rowString(row, "lastSeenAt"),
  };
}

/** The four things the engine can observe, in the order they happen. */
export const OBSERVATION_ORDER = [
  "catalog_read",
  "cart_accepted",
  "checkout_url",
  "order_mirrored",
] as const;

export type ObservationKind = (typeof OBSERVATION_ORDER)[number];

/** One observation the engine recorded. */
export interface PreviewObservationRow {
  id: string;
  grantId: string;
  siteId: string;
  storeId: string;
  kind: string;
  observedAt: string;
  ok: boolean;
  detail: string;
  failure: string;
  durationMs: number;
}

export function previewObservationFromRow(row: Row): PreviewObservationRow {
  const ms = row["durationMs"];
  return {
    id: rowString(row, "id"),
    grantId: rowString(row, "grantId"),
    siteId: rowString(row, "siteId"),
    storeId: rowString(row, "storeId"),
    kind: rowString(row, "kind"),
    observedAt: rowString(row, "observedAt"),
    ok: row["ok"] === true,
    detail: rowString(row, "detail"),
    failure: rowString(row, "failure"),
    durationMs: typeof ms === "number" ? ms : 0,
  };
}

/**
 * What each observation is called, and WHAT IT WOULD ASK, in a reader's words.
 *
 * THE BLURB IS A QUESTION, NOT A CLAIM, and the difference is the one this
 * surface cannot afford to blur. It is shown beneath "not measured yet", so a
 * blurb in the past tense -- "an order arrived back in MemQL" -- reads as the
 * very fact the row above it is saying nobody has established. A measured row
 * shows the engine's own detail instead and never this.
 */
export const OBSERVATION_WORDS: Readonly<Record<ObservationKind, { label: string; blurb: string }>> = {
  catalog_read: { label: "Catalog", blurb: "whether the store answers a product read" },
  cart_accepted: { label: "Cart", blurb: "whether a cart accepts a line" },
  checkout_url: { label: "Checkout", blurb: "whether a checkout address comes back" },
  order_mirrored: { label: "Order", blurb: "whether an order finds its way back to MemQL" },
};

/**
 * The newest observation of each kind, from a list that is newest first.
 *
 * ONE READING PER KIND, because the panel answers "does this storefront work",
 * not "how many times has it been tried". The full history stays on the rows
 * for anyone who wants it; this is the answer a person came for.
 *
 * A KIND WITH NO ROW IS ABSENT FROM THE MAP, deliberately -- the caller must
 * handle the absence as its own state rather than being handed a zero.
 */
export function latestByKind(
  rows: readonly PreviewObservationRow[],
): Partial<Record<ObservationKind, PreviewObservationRow>> {
  const out: Partial<Record<ObservationKind, PreviewObservationRow>> = {};
  for (const row of rows) {
    const kind = row.kind as ObservationKind;
    if (!OBSERVATION_ORDER.includes(kind)) continue;
    if (out[kind] === undefined) out[kind] = row;
  }
  return out;
}

/** A refusal the engine would make, with the act that clears it. */
export interface PreviewRefusal {
  code: string;
  message: string;
  remedy: string;
}

export const NO_REFUSAL: PreviewRefusal = { code: "", message: "", remedy: "" };

export function refusalFrom(value: unknown): PreviewRefusal {
  if (value === null || typeof value !== "object") return NO_REFUSAL;
  const v = value as Record<string, unknown>;
  const str = (key: string): string => (typeof v[key] === "string" ? (v[key] as string) : "");
  return { code: str("code"), message: str("message"), remedy: str("remedy") };
}

/**
 * What is legal for this deployable right now, as the engine answers it.
 *
 * THE SHELL DOES NOT DECIDE ANY OF THIS. Whether the bound store is a
 * development store is a field on a cluster-owner-tier row the browser cannot
 * read, so the three booleans come from `sitePreviewReadiness` -- the same
 * functions the write guard refuses with. A second copy of the rule here would
 * drift into the worst possible shape: a control offered because the shell says
 * yes, refused by a guard that says no.
 */
export interface PreviewReadiness {
  siteId: string;
  hostname: string;
  storefront: boolean;
  bundleRef: string;
  candidateRef: string;
  hasCandidate: boolean;
  storeId: string;
  storeDomain: string;
  storeReadable: boolean;
  storeIsDevelopment: boolean;
  previewStoreId: string;
  previewStoreDomain: string;
  testingUrl: string;
  canPreview: boolean;
  canPromote: boolean;
  canGoLive: boolean;
  previewRefusal: PreviewRefusal;
  promoteRefusal: PreviewRefusal;
  goLiveRefusal: PreviewRefusal;
}

export function readinessFromRow(row: Row): PreviewReadiness {
  const bool = (key: string): boolean => row[key] === true;
  return {
    siteId: rowString(row, "siteId"),
    hostname: rowString(row, "hostname"),
    storefront: bool("storefront"),
    bundleRef: rowString(row, "bundleRef"),
    candidateRef: rowString(row, "candidateRef"),
    hasCandidate: bool("hasCandidate"),
    storeId: rowString(row, "storeId"),
    storeDomain: rowString(row, "storeDomain"),
    storeReadable: bool("storeReadable"),
    storeIsDevelopment: bool("storeIsDevelopment"),
    previewStoreId: rowString(row, "previewStoreId"),
    previewStoreDomain: rowString(row, "previewStoreDomain"),
    testingUrl: rowString(row, "testingUrl"),
    canPreview: bool("canPreview"),
    canPromote: bool("canPromote"),
    canGoLive: bool("canGoLive"),
    previewRefusal: refusalFrom(row["previewRefusal"]),
    promoteRefusal: refusalFrom(row["promoteRefusal"]),
    goLiveRefusal: refusalFrom(row["goLiveRefusal"]),
  };
}

/** A minted preview: the URL comes back once and is never readable again. */
export interface OpenedPreview {
  grantId: string;
  hostname: string;
  candidateRef: string;
  previewStoreDomain: string;
  expiresAt: string;
  ttlMinutes: number;
  /** The one-time link. Held in memory only; nothing stores it. */
  url: string;
}

export function openedPreviewFromRow(row: Row): OpenedPreview {
  const ttl = row["ttlMinutes"];
  return {
    grantId: rowString(row, "grantId"),
    hostname: rowString(row, "hostname"),
    candidateRef: rowString(row, "candidateRef"),
    previewStoreDomain: rowString(row, "previewStoreDomain"),
    expiresAt: rowString(row, "expiresAt"),
    ttlMinutes: typeof ttl === "number" ? ttl : 0,
    url: rowString(row, "url"),
  };
}

/**
 * A bundle version in the short form a person reads.
 *
 * The same reduction the Versions section already makes on `bundleRef`, so the
 * candidate and the serving version are named the same way on one screen.
 */
export function shortRef(ref: string): string {
  const trimmed = ref.replace(/\/$/, "");
  const tail = trimmed.split("/").pop() ?? "";
  return tail === "" ? ref : tail;
}

/** Whether a preview is still usable: not revoked, and not past its expiry. */
export function grantIsOpen(grant: PreviewGrantRow, now: number): boolean {
  if (grant.revokedAt.trim() !== "") return false;
  const expires = Date.parse(grant.expiresAt);
  return Number.isFinite(expires) && expires > now;
}
