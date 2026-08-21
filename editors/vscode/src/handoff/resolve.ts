// Which registered cluster a link names, and where a construct lands (design 4.3-4.4).
//
// A link carries a DOMAIN, and the registry is keyed by NAME -- so matching is
// by the domain the add/edit flow stored, or by the endpoint that domain
// composes for an entry registered before domains were recorded. Several
// entries may name one domain (a developer with two tokens to one cluster);
// the selected one wins and the rest are named, not hidden.
//
// The landing is a pure decision over facts the adapter gathers: which
// workspace folder (if any) holds the file, whether the cluster is local, and
// where its checkout is. It never touches the filesystem itself.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4251

import type { ClusterConfig } from "../clusters/model.js";
import { composeEndpointFromDomain, normalizeDomain } from "../connection/endpoint.js";

export type ClusterMatch =
  | { kind: "none" }
  | { kind: "one"; cluster: ClusterConfig; alsoMatched: string[] };

export function matchCluster(clusters: readonly ClusterConfig[], domain: string, selected: string): ClusterMatch {
  const wanted = normalizeDomain(domain).toLowerCase();
  const endpoint = composeEndpointFromDomain(wanted);
  const matches = clusters.filter(
    (c) => normalizeDomain(c.domain ?? "").toLowerCase() === wanted || c.endpoint.trim().toLowerCase() === endpoint,
  );
  if (matches.length === 0) return { kind: "none" };
  const chosen = matches.find((c) => c.name === selected) ?? matches[0]!;
  return { kind: "one", cluster: chosen, alsoMatched: matches.filter((c) => c !== chosen).map((c) => c.name) };
}

export type Landing =
  | { kind: "notLoaded" }
  | { kind: "detailPage" }
  | { kind: "workspaceFile"; folder: string; relativePath: string }
  | { kind: "openCheckout"; checkout: string; mode: "thisWindow" | "ask" }
  | { kind: "clusterDocument" };

export function landingFor(input: {
  construct?: { origin: string; originPath: string };
  existingIn?: { folder: string; relativePath: string };
  clusterLocal: boolean;
  checkout: string;
  workspaceFolderCount: number;
}): Landing {
  if (input.construct === undefined) return { kind: "notLoaded" };
  if (input.construct.originPath === "") return { kind: "detailPage" };
  if (input.existingIn !== undefined) {
    return { kind: "workspaceFile", folder: input.existingIn.folder, relativePath: input.existingIn.relativePath };
  }
  if (input.clusterLocal && input.checkout !== "") {
    return { kind: "openCheckout", checkout: input.checkout, mode: input.workspaceFolderCount === 0 ? "thisWindow" : "ask" };
  }
  return { kind: "clusterDocument" };
}

/** Where a catalog path may sit inside a workspace folder: a checkout keeps the tree under dsl/. */
export function workspaceCandidates(originPath: string): string[] {
  const bare = originPath.replace(/\\/g, "/").replace(/^\.\//, "").replace(/^dsl\//, "");
  return [`dsl/${bare}`, bare];
}
