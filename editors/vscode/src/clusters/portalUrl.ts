// Where this cluster's portal is.
//
// THE ROW IS ASKED FIRST, AND THE PATH IS THE FALLBACK. When connected, the
// portal is a `v1:platform:site` row like any other hosted surface -- site #1,
// no special case -- and its hostname is what the cluster itself says the
// portal is served at. Composing `https://api.<domain>/portal/` is what
// `component/genesis/domain.go` puts there TODAY, and memql#3711 moves it to
// its own origin; a hard-coded path would be silently wrong the day that lands,
// on every cluster that had already moved.
//
// So: read the row when there is a connection to read it over, and compose only
// when there is not. The composed answer is not a guess -- it is the current
// convention -- but it is the older of the two answers, and the cluster's own
// row outranks it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3742 #3711 #3733

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flattenForList } from "../state/rowProjection.js";
import { normalizeDomain } from "../connection/endpoint.js";
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
 * `https://api.<domain>/portal/`, where the bff serves it today.
 *
 * Falls back to the ENDPOINT's host when the entry records no domain -- an
 * operator who registered a cluster by endpoint alone still has a front door,
 * and the port is dropped because the portal is served over 443 whatever the
 * gRPC endpoint names.
 *
 * Empty when there is nothing to compose from, which the caller renders as "no
 * portal address can be worked out" rather than opening `https:///portal/`.
 */
export function composePortalUrl(cluster: ClusterConfig): string {
  const domain = normalizeDomain(cluster.domain ?? "");
  if (domain !== "") return `https://api.${domain}/portal/`;
  const host = (cluster.endpoint ?? "").trim().split(":")[0] ?? "";
  return host === "" ? "" : `https://${host}/portal/`;
}
