// Where this cluster's graphical console is.
//
// THE ROW IS ASKED FIRST, AND THE COMPOSITION IS THE FALLBACK. When connected,
// the console is a `v1:platform:site` row like any other hosted surface -- no
// special case -- and its hostname is what the cluster itself says the console
// is served at.
//
// THE PREDICTION THIS COMMENT USED TO CARRY CAME TRUE, AND THE FALLBACK HAD
// NOT MOVED. It said that composing `https://api.<domain>/portal/` was the
// convention and that memql#3711 would move the console to its own origin, so
// "a hard-coded path would be silently wrong the day that lands". #3711
// landed; the composition did not follow it, and every cluster reached that
// address until memql#3906. Preferring the row was the right design and it did
// not save anyone, because the composition is what runs when there is no
// connection -- which is exactly the first-run case where the console is being
// opened for the first time.
//
// IT HAPPENED A SECOND TIME, WHICH IS WHY THE COMPOSITION IS WORTH READING
// TWICE. Epic memql#4984 retired the portal and made MemQL OS the console, so
// `https://portal.<domain>/` became a host nothing serves. The ROW path
// survived that change untouched -- it asks the cluster, and the cluster's
// answer moved with it -- and the composition needed an edit again. Two moves,
// two times the composed half was the half that broke.
//
// So: read the row when there is a connection to read it over, and compose
// only when there is not. The composed answer is not a guess -- it is the same
// rule `component/envregistry/domain.go` composes the console's redirect URI
// and CORS origin from -- but it is the older of the two answers, and the
// cluster's own row outranks it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3742 #3711 #3733 #3906 #4984

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flattenForList } from "../state/rowProjection.js";
import { hostOf, normalizeDomain } from "../connection/endpoint.js";
import type { ClusterConfig } from "./model.js";

/** The concept every hosted surface on a cluster is a row of. */
export const SITE_CONCEPT = "v1:platform:site";

export interface ConsoleTarget {
  url: string;
  /** True when the cluster's own site row supplied it. */
  fromSiteRow: boolean;
}

/**
 * The console's URL for a cluster, from its site rows when they were readable.
 *
 * `rows` is what a browse of `v1:platform:site` returned, or an empty list when
 * nothing could be read -- there is no third case, because a failed read and a
 * cluster with no rows both mean "compose it", and distinguishing them would
 * only let the page report a problem it has already worked around.
 */
export function consoleTarget(cluster: ClusterConfig, rows: readonly Row[]): ConsoleTarget {
  const hostname = systemOwnedHostname(rows);
  if (hostname !== "") {
    return { url: `https://${hostname}/`, fromSiteRow: true };
  }
  return { url: composeConsoleUrl(cluster), fromSiteRow: false };
}

/**
 * The hostname of the SYSTEM-OWNED site, which is the console.
 *
 * `systemOwned` rather than a name match: the flag is what the engine stamps on
 * the platform's own surface, and matching on a name would pick a customer's
 * site the day somebody calls one "os". This is also what made the portal's
 * retirement a no-op on this path -- the flag moved to the row that replaced
 * it, and nothing here had to know. A cluster hosting several system-owned
 * sites is not a shape that exists; the first is taken and the choice is
 * stable because the rows arrive in a defined order.
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
 * `https://os.<domain>/`, where the edge node serves it.
 *
 * IT HAS BEEN WRONG TWICE, and both times for the same reason: it is the
 * answer a caller gets when there is NO connection, which is exactly the
 * first-run moment a wrong address is least recoverable. It composed
 * `https://api.<domain>/portal/` until memql#3906 (the bundle had moved to its
 * own origin in memql#3711 and this had not followed), and `portal.<domain>`
 * until epic memql#4984 (the portal was retired and MemQL OS is the console).
 *
 * The engine-side rule it mirrors is `component/envregistry/domain.go`, which
 * composes the same `https://os` + domain for the console's OAuth redirect URI
 * and CORS origin.
 *
 * Falls back to the ENDPOINT when the entry records no domain, deriving the
 * domain from an `api.<domain>` front door exactly as `identityBaseUrlFor`
 * derives its own sibling. An endpoint that names no such front door composes
 * nothing: the console is a DIFFERENT host from the gRPC endpoint, so there is
 * no host left to reuse, and inventing one would open a browser at an address
 * nobody nominated.
 *
 * Empty when there is nothing to compose from, which the caller renders as "no
 * console address can be worked out" rather than opening `https:///`.
 */
export function composeConsoleUrl(cluster: ClusterConfig): string {
  const domain = normalizeDomain(cluster.domain ?? "");
  if (domain !== "") return `https://os.${domain}/`;
  const host = hostOf(cluster.endpoint ?? "");
  const API = "api.";
  if (host !== undefined && host.startsWith(API) && host.length > API.length) {
    return `https://os.${host.slice(API.length)}/`;
  }
  return "";
}

/**
 * The console's address for ONE concept's rows.
 *
 * THE FOLLOW-UP THIS FILE ASKED FOR (epic memql#5009, memql#5010). The note
 * that stood here said there was no concept deep-link, because
 * `portalConceptUrl` composed `<root>/concepts/<id>` and epic memql#4984 left
 * no page answering that route. MemQL OS now has a Concepts app, so the link
 * is back -- as an ADD pointed at the surface that answers, never as a revert
 * of that removal. The reasoning it recorded still holds and is the reason
 * this was not restored earlier: a menu item that opens a page which does not
 * answer is worse than an absent one.
 *
 * A QUERY PARAMETER RATHER THAN A PATH SEGMENT, and that is the shell's shape
 * rather than a preference. MemQL OS is a desktop shell with no router: a
 * window carries an app and a section, not a path, so `/concepts/<id>` would
 * be served the shell's own index by the edge and land on the desk with the
 * marker lost. `?concept=<id>` is read once at boot, scrubbed from the address
 * bar, and turned into an open intent
 * (`clients/os/src/apps/concepts/openConcept.ts`).
 *
 * `URLSearchParams` does the escaping, which is also why the old
 * `encodePortalSegment` has no successor here: it existed to restore `%3A` to
 * `:` inside a PATH segment, and a query value has no such convention to
 * mirror. A concept id's colons survive either way; the difference is only
 * how they are spelled.
 *
 * Empty in, empty out: `consoleTarget` returns "" when no address can be
 * worked out, and composing a link onto nothing would open `https:///?...`.
 */
export function consoleConceptUrl(root: string, conceptId: string): string {
  const id = conceptId.trim();
  if (root === "" || id === "") return "";
  const base = root.endsWith("/") ? root : `${root}/`;
  const params = new URLSearchParams({ concept: id });
  return `${base}?${params.toString()}`;
}
