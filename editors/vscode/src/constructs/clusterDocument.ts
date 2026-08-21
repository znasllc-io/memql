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

function safeDecode(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return "";
  }
}
