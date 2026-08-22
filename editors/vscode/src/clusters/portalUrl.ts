// Where this cluster's portal is.
//
// THE ROW IS ASKED FIRST, AND THE COMPOSITION IS THE FALLBACK. When connected,
// the portal is a `v1:platform:site` row like any other hosted surface -- site
// #1, no special case -- and its hostname is what the cluster itself says the
// portal is served at.
//
// THE PREDICTION IN THIS COMMENT CAME TRUE, AND THE FALLBACK HAD NOT MOVED.
// It used to say that composing `https://api.<domain>/portal/` was the current
// convention and that memql#3711 would move the portal to its own origin, so
// "a hard-coded path would be silently wrong the day that lands". #3711 landed;
// the composition did not follow it, and every cluster reached that address
// until memql#3906. Preferring the row was the right design and it did not
// save anyone, because the composition is what runs when there is no connection
// -- which is exactly the first-run case where the portal is being opened for
// the first time.
//
// So: read the row when there is a connection to read it over, and compose only
// when there is not. The composed answer is not a guess -- it is the same rule
// `component/envregistry/domain.go` composes the portal's redirect URI and CORS
// origin from -- but it is the older of the two answers, and the cluster's own
// row outranks it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3742 #3711 #3733 #3906

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flattenForList } from "../state/rowProjection.js";
import { hostOf, normalizeDomain } from "../connection/endpoint.js";
import type { ClusterConfig } from "./model.js";

/** The concept every hosted surface on a cluster is a row of. */
export const SITE_CONCEPT = "v1:platform:site";

export interface PortalTarget {
  url: string;
  /** True when the cluster's own site row supplied it. */
  fromSiteRow: boolean;
}

/**
 * The portal's URL for a cluster, from its site rows when they were readable.
 *
 * `rows` is what a browse of `v1:platform:site` returned, or an empty list when
 * nothing could be read -- there is no third case, because a failed read and a
 * cluster with no rows both mean "compose it", and distinguishing them would
 * only let the page report a problem it has already worked around.
 */
export function portalTarget(cluster: ClusterConfig, rows: readonly Row[]): PortalTarget {
  const hostname = systemOwnedHostname(rows);
  if (hostname !== "") {
    return { url: `https://${hostname}/`, fromSiteRow: true };
  }
  return { url: composePortalUrl(cluster), fromSiteRow: false };
}

/**
 * The hostname of the SYSTEM-OWNED site, which is the portal.
 *
 * `systemOwned` rather than a name match: the flag is what the engine stamps on
 * the platform's own surface, and matching on a name would pick a customer's
 * site the day somebody calls one "portal". A cluster hosting several
 * system-owned sites is not a shape that exists; the first is taken and the
 * choice is stable because the rows arrive in a defined order.
 */
function systemOwnedHostname(rows: readonly Row[]): string {
  for (const raw of rows) {
    const row = flattenForList(raw);
    if (row["systemOwned"] !== true) continue;
    const hostname = typeof row["hostname"] === "string" ? row["hostname"].trim() : "";
    if (hostname !== "") return hostname;
  }
  return "";
}

/**
 * `https://portal.<domain>/`, where the edge node serves it.
 *
 * THE DAY THE COMMENT ABOVE PREDICTED HAS LANDED (memql#3711, fixed in
 * memql#3906). This composed `https://api.<domain>/portal/` -- the sub-path the
 * bff used to mount the bundle on -- and the portal has since moved to its own
 * origin as site #1. Verified against a running cluster:
 *
 *     https://api.<domain>/portal/   -> 415
 *     https://portal.<domain>/       -> 200
 *     v1:platform:site               -> hostname=portal.<domain>, systemOwned
 *
 * The 415 is worth naming, because it is not the 404 a stale path usually
 * gives. `/portal/` has no Ingress rule of its own, so it falls through to the
 * `/` catch-all, which forwards h2c to the bff's gRPC edge -- an HTTP/1.1
 * request handed to an h2c backend. The engine-side rule this now mirrors is
 * `component/envregistry/domain.go`, which composes the same `https://portal` +
 * site suffix for the portal's OAuth redirect URI and CORS origin.
 *
 * WHY THIS MATTERED MORE THAN THE ROW PATH SUGGESTS. `portalTarget` prefers the
 * cluster's own site row and is right whenever it can read one -- but reading
 * rows needs a live connection, which needs a credential. The composed answer
 * is therefore exactly what a first-run operator gets, which is the one moment
 * a wrong address is least recoverable.
 *
 * Falls back to the ENDPOINT when the entry records no domain, deriving the
 * domain from an `api.<domain>` front door exactly as `identityBaseUrlFor`
 * derives its own sibling. An endpoint that names no such front door composes
 * nothing: the portal is a DIFFERENT host from the gRPC endpoint now, so there
 * is no host left to reuse, and inventing one would open a browser at an
 * address nobody nominated.
 *
 * Empty when there is nothing to compose from, which the caller renders as "no
 * portal address can be worked out" rather than opening `https:///`.
 */
export function composePortalUrl(cluster: ClusterConfig): string {
  const domain = normalizeDomain(cluster.domain ?? "");
  if (domain !== "") return `https://portal.${domain}/`;
  const host = hostOf(cluster.endpoint ?? "");
  const API = "api.";
  if (host !== undefined && host.startsWith(API) && host.length > API.length) {
    return `https://portal.${host.slice(API.length)}/`;
  }
  return "";
}

/**
 * The portal's own path-segment escaping, mirrored (memql#4252).
 *
 * The extension cannot import `clients/portal/src/concepts/urls.ts` -- that is
 * a browser module, and this is a `vscode` extension host -- so this is a
 * second copy of ONE rule, tested against the portal's own fixtures rather
 * than trusted to agree by construction. A colon is a legal URL path-segment
 * character (RFC 3986 §3.3) and a MemQL id is colon-delimited, so escaping it
 * would turn every id in a link into a `%3A` thicket nobody recognises;
 * everything else a segment cannot carry (`/`, spaces, `?`, `#`, a literal
 * `%`) still gets escaped.
 */
export function encodePortalSegment(value: string): string {
  return encodeURIComponent(value).replace(/%3A/g, ":");
}

/**
 * The portal's concept-rows page for one concept, under the root `portalTarget`
 * found.
 *
 * `conceptId` is a `CatalogConstruct.name` for a `kind === "concept"` row --
 * the catalog's name for a concept IS its id (`v1:cognition:space`), so
 * nothing here looks it up or translates it.
 */
export function portalConceptUrl(root: string, conceptId: string): string {
  const base = root.endsWith("/") ? root : `${root}/`;
  return `${base}concepts/${encodePortalSegment(conceptId)}`;
}
