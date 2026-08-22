// A construct's file, served from the cluster that loaded it (design 4.5).
//
// THE FILE IS NOT ON THIS MACHINE, and this is the honest rendering of that:
// a read-only document whose bytes come from the cluster's own pack browser
// (ReadPackFile), opened at the construct's signature. It is distinct from
// `memql-catalog:` (catalogTarget.ts), which is a non-resolvable sentinel the
// run path uses and which nothing can open.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go);
// clusterDocuments.ts is the adapter.
//
// Refs: #4248

export const CLUSTER_DOCUMENT_SCHEME = "memql-cluster";

export interface ClusterDocumentRef {
  /** The registry name of the cluster the bytes come from. */
  cluster: string;
  /** Relative to the DSL tree root, as the catalog reports it. */
  originPath: string;
  kind: string;
  name: string;
}

export function clusterDocumentUri(ref: ClusterDocumentRef): string {
  const query = `kind=${encodeURIComponent(ref.kind)}&name=${encodeURIComponent(ref.name)}`;
  return `${CLUSTER_DOCUMENT_SCHEME}://${encodeURIComponent(ref.cluster)}/${ref.originPath}?${query}`;
}

export function parseClusterDocumentUri(uri: {
  authority: string;
  path: string;
  query: string;
}): ClusterDocumentRef | undefined {
  const cluster = safeDecode(uri.authority);
  const originPath = uri.path.replace(/^\/+/, "");
  if (cluster === "" || originPath === "") return undefined;
  const params = new URLSearchParams(uri.query);
  const kind = params.get("kind") ?? "";
  const name = params.get("name") ?? "";
  if (kind === "" || name === "") return undefined;
  return { cluster, originPath, kind, name };
}

/** The pack-browser coordinates of an origin path: the first segment is the domain. */
export function packLocator(originPath: string): { domain: string; path: string } | undefined {
  let p = originPath.replace(/\\/g, "/").replace(/^\.\//, "");
  if (p.startsWith("dsl/")) p = p.slice("dsl/".length);
  const slash = p.indexOf("/");
  if (slash <= 0 || slash === p.length - 1) return undefined;
  return { domain: p.slice(0, slash), path: p.slice(slash + 1) };
}

export function notConnectedNotice(cluster: string): string {
  return (
    `// Not connected to ${cluster}.\n` +
    `// This document is served from the cluster; reconnect to ${cluster} and reopen it.\n`
  );
}

export function notFoundNotice(cluster: string, originPath: string): string {
  return `// ${cluster} does not serve ${originPath}.\n// The catalog named this path, but the pack browser has no such file.\n`;
}

/**
 * What the document says when the cluster could not be read at all.
 *
 * NEVER THE RAW ERROR. This text is rendered INTO a .memql buffer, so every
 * line is a comment and none of it is the transport's own words -- those go to
 * the MemQL Connection channel, through the redactor, which is what this points
 * the reader at. See the information policy (memql#4194).
 */
export function fetchFailedNotice(cluster: string, originPath: string): string {
  return (
    `// ${cluster} could not be read for ${originPath}.\n` +
    `// Reconnect and reopen it; the failure is recorded in the MemQL Connection output channel.\n`
  );
}

/**
 * The refusal for opening a cluster document's construct details, or undefined
 * when the connection in hand IS the document's cluster.
 *
 * THE DOCUMENT NAMES ITS OWN CLUSTER, and the answer must come from that name
 * rather than from whatever is connected now. A cluster document outlives the
 * connection that produced it: its body is rewritten to the not-connected
 * notice while its header lens still says where it came from, so a click after
 * a switch would otherwise resolve the same {kind, name} against a DIFFERENT
 * cluster and render that one's construct with nothing saying so. This is the
 * same refusal the provider makes on the way in, said out loud.
 *
 * `connectedCluster` is the name of a CONNECTED stream, or undefined when there
 * is none -- the two produce different sentences because they ask the reader
 * for different things. An empty `documentCluster` is no claim at all and is
 * never refused: the argument arrives over the untrusted command boundary.
 */
export function detailsRefusal(
  documentCluster: string,
  connectedCluster: string | undefined,
): string | undefined {
  if (documentCluster === "" || connectedCluster === documentCluster) return undefined;
  if (connectedCluster === undefined) {
    return `MemQL: this document came from ${documentCluster}. Reconnect to ${documentCluster} to open its details.`;
  }
  return `MemQL: this document came from ${documentCluster}; you are connected to ${connectedCluster}. Reconnect to ${documentCluster} to open its details.`;
}

/**
 * The refusal for a CONSTRUCT PANEL action, or undefined when it may run.
 *
 * THE SAME DEFECT AS THE LENS, ONE SURFACE ALONG (memql#4253). The panel is a
 * singleton that outlives the connection its record was read over: open a
 * construct on `staging`, switch to `prod`, and its "view source" / "browse
 * rows" buttons would resolve against `prod` -- serving one cluster's bytes
 * under another's record, and opening one cluster's portal for a concept picked
 * from another's catalog. Nothing re-points the panel on a state change, so the
 * cluster has to travel WITH the record, exactly as it does through the lens.
 *
 * COMPOSED FROM detailsRefusal RATHER THAN REPEATING IT. There is one cluster
 * comparison in this tree and this is a caller of it, not a sibling: a second
 * copy would be free to disagree about what "" means or about which cluster to
 * name, and the disagreement would surface as one surface refusing what another
 * allowed.
 *
 * What it ADDS is the second question a panel button has to ask and the lens
 * does not: `detailsRefusal` answers "is this the right cluster", which is
 * vacuously fine when the panel makes no claim (`""`) and nothing is connected.
 * A button still cannot run there, so `need` names the action in the caller's
 * own words for that one case.
 */
export function panelClusterRefusal(
  panelCluster: string,
  connectedCluster: string | undefined,
  need: string,
): string | undefined {
  const mismatch = detailsRefusal(panelCluster, connectedCluster);
  if (mismatch !== undefined) return mismatch;
  if (connectedCluster === undefined) return `MemQL: connect to a cluster to ${need}.`;
  return undefined;
}

/**
 * `decodeURIComponent` that answers "" instead of throwing.
 *
 * EXPORTED because a percent sign in a cluster name is not this module's
 * problem alone (memql#4253): the read-only badge decodes the same authority
 * from the same uris, and a bare decodeURIComponent there throws `URIError`
 * INSIDE a FileDecorationProvider -- the badge silently vanishes and the only
 * trace is an extension-host log nobody is reading. One tolerant decoder, used
 * by everything that reads one of these uris.
 */
export function safeDecode(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return "";
  }
}
