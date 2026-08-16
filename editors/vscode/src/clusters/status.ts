// What a Clusters-tree row says about a cluster.
//
// Split out of views/clustersTree.ts so the WORDING and the CHOICE OF ICON are
// unit-testable, the way deploy/clusterView.ts holds the Cluster tab's. The
// tree is left as a mapping from the names below onto ThemeIcons.
//
// The distinction this module exists to draw is memql#3385's second acceptance
// item. A red error dot used to mean everything: an expired access token and a
// cluster that had gone away rendered identically, and the two have completely
// different next actions. `credential` is therefore its own state, at rest as
// well as on failure -- a cluster carrying a PAT (memql#3383) can be flagged
// before anyone tries to connect with it, because nothing about that credential
// needs a round trip to diagnose.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import {
  classifyToken,
  missingCredentialMessage,
  notConfiguredMessage,
  wrongTokenClassMessage,
} from "../connection/credentials.js";
import type { ConnectionState } from "../connection/manager.js";
import { describeVersion } from "../version/describe.js";
import type { ReleaseListing } from "../version/releaseCache.js";
import { needsAuth, type ClusterConfig } from "./model.js";

export type ClusterRowIcon =
  /** The working cluster, live. */
  | "connected"
  /** Handshake in flight. */
  | "connecting"
  /** The cluster itself is the problem: unreachable, or a connection that died. */
  | "failed"
  /** The CREDENTIAL is the problem: expired, missing, or of a class the mesh rejects. */
  | "credential"
  /** Nothing to dial. */
  | "unconfigured"
  /** Configured, not the working cluster. */
  | "idle";

export interface ClusterRowStatus {
  icon: ClusterRowIcon;
  tooltip: string;
}

// The reasons that mean "look at your token", not "look at your cluster".
// `reauthenticationRequired` is one of them: the stored credentials were
// refused and cleared (memql#3404), which is a credential problem even though
// nothing about the cluster changed.
const CREDENTIAL_REASONS = new Set([
  "credentialExpired",
  "wrongTokenClass",
  "missingCredential",
  "reauthenticationRequired",
]);

export function clusterRowStatus(
  cluster: ClusterConfig,
  state: ConnectionState,
): ClusterRowStatus {
  const isActive = state.status !== "disconnected" && state.clusterName === cluster.name;

  if (isActive && state.status === "connected") {
    return { icon: "connected", tooltip: `Connected (node ${state.nodeId})` };
  }
  if (isActive && state.status === "connecting") {
    return { icon: "connecting", tooltip: "Connecting..." };
  }
  if (isActive && state.status === "error") {
    if (state.reason === "credentialExpired") {
      return { icon: "credential", tooltip: `CREDENTIAL EXPIRED: ${state.message}` };
    }
    if (CREDENTIAL_REASONS.has(state.reason)) {
      return { icon: "credential", tooltip: `CREDENTIAL: ${state.message}` };
    }
    return { icon: "failed", tooltip: `ERROR: ${state.message}` };
  }

  // At rest. The same three sentences the connection attempt would produce, so
  // an operator reads one explanation of a condition rather than two.
  if (cluster.endpoint.trim() === "") {
    return { icon: "unconfigured", tooltip: notConfiguredMessage(cluster.name) };
  }
  const tokenClass = classifyToken(cluster.token);
  if (tokenClass === "pat" || tokenClass === "workerToken") {
    return { icon: "credential", tooltip: wrongTokenClassMessage(cluster.name, tokenClass) };
  }
  if (needsAuth(cluster)) {
    return { icon: "credential", tooltip: missingCredentialMessage(cluster.name) };
  }
  return { icon: "idle", tooltip: cluster.endpoint };
}

/** The two pieces of text a Clusters-tree row renders beside its icon. */
export interface ClusterRowText {
  /** The dimmed text after the cluster's name. */
  description: string;
  tooltip: string;
}

/**
 * The row's words: what this editor is dialling, and what release it is
 * (memql#3995).
 *
 * THIS IS THE SURFACE THAT MAKES A DISCONNECTED OR NEVER-DIALLED CLUSTER'S
 * VERSION VISIBLE AT ALL. Every other place a version could appear needs a live
 * session; this one is read off clusters.yaml, so it answers with the cluster
 * switched off -- which is the situation the motivating incident happened in,
 * and the reason memql#3990 records the version rather than observing it.
 *
 * Two decisions are COMPOSED here rather than taken here: the connection
 * verdict above, and the version verdict in `version/describe.ts`. Keeping the
 * composition in this module is what leaves views/clustersTree.ts as a mapping
 * onto VS Code's icon vocabulary.
 *
 * An unknown version adds NOTHING. A row that appended "unknown" to every
 * cluster on a fresh install would be noise, and the connection page says the
 * word where there is room to explain it.
 */
export function clusterRowText(
  cluster: ClusterConfig,
  state: ConnectionState,
  listing: ReleaseListing | undefined,
): ClusterRowText {
  const status = clusterRowStatus(cluster, state);
  const version = describeVersion({ recorded: cluster.version, listing });
  return {
    description:
      version.short === "" ? cluster.endpoint : `${cluster.endpoint} - ${version.short}`,
    // The connection verdict stays FIRST. It is what an operator opened the
    // tooltip for; the version is context beneath it, and a tooltip that led
    // with the version would bury the reason the row is red.
    tooltip:
      version.state === "unknown" ? status.tooltip : `${status.tooltip}\n${version.sentence}`,
  };
}
