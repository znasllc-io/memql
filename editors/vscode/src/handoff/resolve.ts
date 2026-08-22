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
  // AN EMPTY DOMAIN NAMES NOTHING, and saying so first is what stops it from
  // naming EVERYTHING. An entry registered before domains were recorded
  // normalises to "" too, and composeEndpointFromDomain("") is "" by the same
  // rule -- so both arms of the filter below match every such entry, and a
  // blank domain would "resolve" to whichever legacy cluster happened to be
  // first. Unreachable through parseOpenRequest, which refuses an empty
  // cluster; guarded here because this module trusts nothing it is handed.
  if (wanted === "") return { kind: "none" };
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

/**
 * Where a catalog path may sit inside a workspace folder: a checkout keeps the
 * tree under dsl/, so both layouts are tried.
 *
 * AN ESCAPING PATH IS REFUSED, NOT REPAIRED. The catalog is served by the
 * CLUSTER, so `originPath` is not this editor's own value, and `Uri.joinPath`
 * NORMALISES `..` -- which means a path like `../../.ssh/id_rsa` resolves
 * cleanly outside the workspace folder and stats successfully. Returning no
 * candidates at all is the honest answer ("this is not a path inside a
 * folder"), and it reads at the call site as the file simply not being here:
 * the handoff falls through to the cluster document and the construct page
 * says the file is not in this workspace. Rewriting the path instead would
 * invent a location the cluster never named.
 *
 * An empty path is refused for a quieter reason: it joins to the FOLDER, and a
 * directory stats successfully, so an empty originPath would otherwise report
 * the workspace root as the construct's file.
 *
 * `..` inside a segment (`a..b`) is an ordinary name and is left alone.
 */
export function workspaceCandidates(originPath: string): string[] {
  const normalized = originPath.replace(/\\/g, "/").replace(/^\.\//, "");
  if (normalized === "" || normalized === "/") return [];
  // Absolute in either spelling: a rooted POSIX path, or a Windows drive.
  if (normalized.startsWith("/") || /^[a-zA-Z]:\//.test(normalized)) return [];
  if (normalized.split("/").some((segment) => segment === "..")) return [];
  const bare = normalized.replace(/^dsl\//, "");
  return [`dsl/${bare}`, bare];
}
