import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten } from "../../kit/rows";

// The wire rows this app renders, projected into the shapes its surfaces read.
//
// PURE, and separate from every component, for the reason apps/users/rows.ts
// is: a projection asserted through render() is asserted through three layers
// that can each fail for unrelated reasons. Everything here is a function of a
// row, unit-testable with no browser, no cluster and no React -- which is what
// lets the LIST, the DETAIL and the MAP be checked against the same fixtures
// and therefore be checked against each other.

// `flatten` and `boolOr` moved to kit/rows.ts when the third app copied them
// (memql#4721); the reasoning they carried -- seed rows and CDC envelopes must
// project identically, and an absent boolean takes the concept's own default
// -- moved with them. `flatten` is re-exported because it was already part of
// this module's surface (the page's zip picker reads it).
export { flatten };

function objectOf(row: Row, key: string): Record<string, unknown> {
  const v = row[key];
  if (v && typeof v === "object" && !Array.isArray(v)) return v as Record<string, unknown>;
  return {};
}

/**
 * The values `v1:platform:site.status` declares.
 *
 * ONE LIST, and the type below is derived from it rather than restated. The
 * two used to be written separately -- a union type and a runtime allowlist in
 * `siteFromRow` -- and that is a shape that cannot survive the enum growing:
 * `archived` arrived with the packages epic (memql#4794), went into the type,
 * and was silently normalised to "" by the projection, so every archived row
 * reached its component with a BLANK status. The Archived filter would have
 * listed rows that did not say they were archived.
 *
 * It was inert until the fourth value existed, which is why it survived: with
 * exactly three members the allowlist dropped nothing. Deriving the type from
 * the list makes the next value one edit instead of two, and makes a missed
 * one a type error rather than an empty string.
 */
export const SITE_STATUSES = ["draft", "live", "disabled", "archived"] as const;

/** A declared status, or "" for a row the fold has not filled. `archived` is
 *  the end of the D10 lifecycle and the edge answers 404 for it -- to the
 *  internet an archived site is gone rather than paused. */
export type SiteStatus = (typeof SITE_STATUSES)[number] | "";

/** Narrows a wire string to a declared status. The ONE runtime gate, reading
 *  the same list the type is built from. */
export function isSiteStatus(value: string): value is SiteStatus {
  return (SITE_STATUSES as readonly string[]).includes(value);
}

export interface SiteRow {
  id: string;
  /** EMPTY means CLUSTER-OWNED -- the seeded portal row is the case. */
  ownerUserId: string;
  hostname: string;
  /** spa | static | shopify_storefront, or "" on a row the fold has not filled. */
  kind: string;
  status: SiteStatus;
  bundleRef: string;
  /** Provenance: the Library artifact this bundle was published from, if any. */
  artifactId: string;
  title: string;
  notes: string;
  apiProxy: boolean;
  /** Blocks deletion. The portal row only; it does NOT branch the serving path. */
  systemOwned: boolean;
  deleted: boolean;
  binding: Record<string, unknown>;
  /**
   * The client this deployable is FOR (epic memql#4800, D5). Optional, and a
   * plain reference with no read effect -- a site with no tie lists, resolves
   * and serves exactly as it always has.
   */
  accountId: string;
  /**
   * The package this site was deployed from (epic memql#4794, D7), or "" for
   * a hand-made site. The page joins a deployable to its SOURCE through it;
   * a site with none is its own source, and its bundle form says which.
   */
  packageId: string;
  /** The manifest deployable name this site serves; "" whenever packageId is. */
  packageDeployableName: string;
  createdAt: string;
}

export function siteFromRow(raw: Row): SiteRow {
  const row = flatten(raw);
  const status = rowString(row, "status");
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    hostname: rowString(row, "hostname"),
    kind: rowString(row, "kind"),
    status: isSiteStatus(status) ? status : "",
    bundleRef: rowString(row, "bundleRef"),
    artifactId: rowString(row, "artifactId"),
    title: rowString(row, "title"),
    notes: rowString(row, "notes"),
    apiProxy: boolOr(row, "apiProxy", false),
    systemOwned: boolOr(row, "systemOwned", false),
    // `deleted` DEFAULTS FALSE on the concept, so absent is not deleted --
    // reading absent as deleted would empty the list on the first folded event
    // that did not touch the field.
    deleted: boolOr(row, "deleted", false),
    binding: objectOf(row, "binding"),
    accountId: rowString(row, "accountId"),
    packageId: rowString(row, "packageId"),
    packageDeployableName: rowString(row, "packageDeployableName"),
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * What to call a deployable.
 *
 * The hostname, because that is what it IS -- an operator looking for a site
 * is looking for the host it answers at, and `title` is a label somebody may
 * never have set. NEVER blank: a nameless row is indistinguishable from a row
 * that failed to render.
 */
export function siteName(site: SiteRow): string {
  if (site.hostname.trim() !== "") return site.hostname;
  if (site.title.trim() !== "") return site.title;
  return site.id;
}

/**
 * Live now.
 *
 * `draft` resolves for nobody and `disabled` answers 503; only `live` is
 * being served, and that is what the row's dot is about.
 */
export function siteIsCurrent(site: SiteRow): boolean {
  return site.status === "live";
}

/**
 * The status trio (spec section F): live = the accent green, draft = muted,
 * disabled = pending amber.
 *
 * `disabled` gets the WARN tone rather than the error one on purpose: a
 * deliberately paused site answering 503 is a state somebody chose, not a
 * fault -- which is the same distinction that made `disabled` a separate value
 * from a deleted row in the first place.
 */
export type StatusTone = "ok" | "muted" | "warn";

export function statusTone(site: SiteRow): StatusTone {
  if (site.status === "live") return "ok";
  if (site.status === "disabled") return "warn";
  // draft and archived are both "not serving", and they read the same here
  // deliberately: the difference between them is HISTORY, which the archived
  // chip and the Archived filter carry, not liveness.
  return "muted";
}

/**
 * The same three states in the SHELL'S OWN dot language, which has three tones
 * and one of them is silence.
 *
 * The kit's `ProvenanceDot` is green = reachable now, amber = not reachable,
 * unknown = NO DOT, and it renders the dock's "running", the connection state
 * and the fleet's "online" -- so aliveness reads identically everywhere. This
 * maps onto it rather than inventing a fourth dot:
 *
 *   live      -> reachable.    It is being served.
 *   disabled  -> unreachable.  It WAS serving and somebody paused it; a 503
 *                              rather than a 404 exists precisely so that is
 *                              distinguishable, and amber is what the shell
 *                              says "not reachable" with.
 *   draft     -> unknown, so NO dot. A site nobody has published yet has never
 *                been reachable, and painting a screen of new deployables amber
 *                would alarm somebody about the normal case. The muted WORD
 *                beside it is the whole statement.
 */
export function statusDotTone(site: SiteRow): "reachable" | "unreachable" | "unknown" {
  if (site.status === "live") return "reachable";
  if (site.status === "disabled") return "unreachable";
  // draft and archived both get NO dot, which is what `unknown` renders. An
  // archived site is not "unreachable" in the sense the amber dot means --
  // nothing is wrong with it, it is filed -- and the chip beside it says which.
  return "unknown";
}

/** Cluster-owned rows carry no owner (the seeded portal is the one shipped). */
export function siteIsClusterOwned(site: SiteRow): boolean {
  return site.ownerUserId.trim() === "";
}

/**
 * Who a row belongs to, in a word.
 *
 * A cluster owner's list is every deployable in the cluster and an ordinary
 * caller's is their own (`sitesAll`'s filter is
 * `ownerUserId==actor.userId || actor.isClusterOwner==true`), so "yours" is
 * only informative in the first case. It is still rendered in both, because a
 * list that changes its columns with the reader's role is one that cannot be
 * described to somebody over a call.
 */
export function ownerLabel(site: SiteRow, viewerUserId: string): string {
  if (siteIsClusterOwned(site)) return "cluster-owned";
  if (viewerUserId !== "" && site.ownerUserId === viewerUserId) return "yours";
  return "another owner";
}

/**
 * What counts as a CHANGE to a deployable.
 *
 * ONE definition, read by the Sites list's `LiveList` and by the app-level
 * `useArrivals` the map draws from. Two literals would be two literals that can
 * disagree, and the disagreement is visible: the list pulses a row while the
 * map beside it does not, in an app whose whole point is that the two are the
 * same rows.
 *
 * A site row carries no liveness field -- there is no `lastSeenAt` here to turn
 * the surface into a strobe -- so the risk this guards against is the opposite
 * one: leaving `bundleRef` out would make the app's headline event, a publish,
 * arrive in silence.
 */
export function siteFingerprint(site: SiteRow): string {
  return `${site.hostname}|${site.kind}|${site.status}|${site.bundleRef}|${site.artifactId}|${site.title}`;
}

// ---------------------------------------------------------------------------
// The bundle reference, and its three usage forms
// ---------------------------------------------------------------------------

/**
 * `bundleRef` is a URI with three usages, and which one it is decides how a
 * deploy and a rollback WORK -- so the form is the useful reading and the URI
 * is the detail underneath it.
 *
 *   `file:///app/portal`        the platform's own portal, baked into the
 *                               image: deploy and rollback are an image roll.
 *   `file:///app/sites/<name>`  a site baked into the edge image at build
 *                               time: same image roll.
 *   `blob://sites/<id>/<v>/`    an uploaded bundle in object storage: deploy
 *                               is an upload plus a row flip, rollback is one
 *                               row write.
 *
 * A fourth answer is `other`, and it is deliberately not folded into one of
 * the three: a reference nobody recognises is a fact worth showing as itself
 * rather than being described as something it may not be.
 */
export type BundleForm = "none" | "baked-portal" | "baked-site" | "uploaded" | "other";

export const BAKED_PORTAL_REF = "file:///app/portal";

export function bundleForm(bundleRef: string): BundleForm {
  const ref = bundleRef.trim();
  if (ref === "") return "none";
  if (ref === BAKED_PORTAL_REF) return "baked-portal";
  if (ref.startsWith("file:///app/sites/")) return "baked-site";
  if (ref.startsWith("blob://sites/")) return "uploaded";
  return "other";
}

export function bundleFormLabel(form: BundleForm): string {
  switch (form) {
    case "none":
      return "no bundle";
    case "baked-portal":
      return "baked portal";
    case "baked-site":
      return "baked site";
    case "uploaded":
      return "uploaded bundle";
    default:
      return "unrecognised reference";
  }
}

/** What the form MEANS for the next deploy, in one sentence. */
export function bundleFormNote(form: BundleForm): string {
  switch (form) {
    case "none":
      return "This deployable names no bundle, so there is nothing to serve.";
    case "baked-portal":
      return "The platform's own console, baked into the edge image. Deploy and rollback are an image roll, not a row write.";
    case "baked-site":
      return "Baked into the edge image at build time. Deploy and rollback are an image roll, not a row write.";
    case "uploaded":
      return "An uploaded bundle in object storage. A deploy is an upload plus a row flip; a rollback is one row write.";
    default:
      return "Not one of the three references this cluster serves. It is shown as it is stored rather than described as something it may not be.";
  }
}

// ---------------------------------------------------------------------------
// The Shopify binding
// ---------------------------------------------------------------------------

export interface StorefrontBinding {
  storeDomain: string;
  /**
   * The NAME of a `v1:platform:globalSecret` row. The token itself is never
   * stored on the site row and is never fetched here: the edge dereferences it
   * at serve time into the site's runtime-config document, and that is the only
   * place it is resolved.
   */
  storefrontTokenRef: string;
}

export function storefrontBinding(site: SiteRow): StorefrontBinding {
  const b = site.binding;
  return {
    storeDomain: typeof b["storeDomain"] === "string" ? b["storeDomain"] : "",
    storefrontTokenRef:
      typeof b["storefrontTokenRef"] === "string" ? b["storefrontTokenRef"] : "",
  };
}

// ---------------------------------------------------------------------------
// Addresses and grouping
// ---------------------------------------------------------------------------

/**
 * Where the thing actually IS.
 *
 * ALWAYS https, never the shell's own protocol. Locally the front door
 * terminates TLS with an mkcert wildcard and in the cloud with a real
 * certificate, so a hosted site is https in both; deriving the scheme from
 * `window.location` would only ever be a way to be wrong on a dev server.
 * Returns "" for a blank hostname, so a caller renders nothing rather than a
 * link to `https:///`.
 */
export function liveUrlFor(hostname: string): string {
  const host = hostname.trim().toLowerCase();
  return host === "" ? "" : `https://${host}/`;
}

/**
 * The domain a hostname belongs to -- the map's grouping key.
 *
 * THREE OR MORE LABELS: drop the first, because every site this cluster serves
 * is a SINGLE label under the domain (memql#3767) and the wildcard Ingress rule
 * matches exactly one. So `shop.memql.example.com` groups under
 * `memql.example.com` alongside `www.memql.example.com`.
 *
 * TWO OR FEWER: the hostname IS the domain. An apex (`example.com`) has no
 * label to drop, and dropping one would group it under `com` -- which is not a
 * domain this or any cluster serves, and would put every unrelated apex in the
 * same box. A cluster-owner's custom apex therefore forms its own group, which
 * is what it is.
 */
export function domainOf(hostname: string): string {
  const host = hostname.trim().toLowerCase().replace(/\.$/, "");
  if (host === "") return "";
  const labels = host.split(".");
  if (labels.length <= 2) return host;
  return labels.slice(1).join(".");
}
