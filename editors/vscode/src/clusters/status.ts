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
import { DEFAULT_STACK_TAG } from "../install/stackPin.js";
import { briefMessage } from "../state/diagnostics.js";
import { compareVersions } from "../version/compare.js";
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
 * The one-or-two-word verdict a row's dimmed description leads with.
 *
 * The clusters-first IA (memql#4195) reads the list as "which of my clusters
 * needs me", so the subtitle answers STATE, not address. `idle` answers
 * nothing: a configured cluster that simply is not the working one has no
 * state worth a word, and the version alone is the useful fact.
 */
export function rowVerdict(icon: ClusterRowIcon): string {
  switch (icon) {
    case "connected":
      return "connected";
    case "connecting":
      return "connecting...";
    case "failed":
      return "unreachable";
    case "credential":
      return "needs sign-in";
    case "unconfigured":
      return "no endpoint";
    case "idle":
      return "";
  }
}

/**
 * The one-sentence skew fact for a cluster whose recorded release is behind
 * the release this extension build is pinned to (src/install/stackPin.ts).
 * Empty when the versions are not comparable or the cluster is not behind --
 * an incomparable pair proves nothing, and saying "maybe" on every row is
 * noise. The transport-failure variant of the same fact lives in
 * version/skewHint.ts; this is the AT-REST rendering the tree can show
 * without any call having failed.
 */
export function recordedSkewSentence(recorded: string | undefined): string {
  const version = (recorded ?? "").trim();
  if (version === "") return "";
  if (compareVersions(version, DEFAULT_STACK_TAG) !== "behind") return "";
  return `This cluster records ${version}, older than the ${DEFAULT_STACK_TAG} this extension ships for -- it may not answer this editor's newer calls.`;
}

/**
 * The row's words: this cluster's STATE and release (memql#3995, memql#4195).
 *
 * THIS IS THE SURFACE THAT MAKES A DISCONNECTED OR NEVER-DIALLED CLUSTER'S
 * VERSION VISIBLE AT ALL. Every other place a version could appear needs a live
 * session; this one is read off clusters.yaml, so it answers with the cluster
 * switched off -- which is the situation the motivating incident happened in,
 * and the reason memql#3990 records the version rather than observing it.
 *
 * THE ENDPOINT IS NOT THE SUBTITLE ANY MORE (memql#4194, audit rows 10/42).
 * A `host:port` on every row put internal addresses on permanent display in
 * the sidebar -- in every screenshot and screen share -- to answer a question
 * nobody asks at a glance. It lives in the tooltip here and on the Connection
 * page, which is the surface FOR addresses.
 *
 * THE TOOLTIP IS BRIEF ON FAILURE. Raw transport errors go to the MemQL
 * Connection output channel (extension.ts records them as the state changes);
 * the hover carries the classified first line and names the channel, because a
 * hover can be neither scrolled nor copied and must never be the only home of
 * a diagnostic.
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
  // The synthetic error row (empty name -- a name the registry refuses to
  // store) says nothing here; the tree renders its own words for it.
  if (cluster.name === "") {
    return { description: "", tooltip: "" };
  }
  const status = clusterRowStatus(cluster, state);
  const version = describeVersion({ recorded: cluster.version, listing });
  const verdict = rowVerdict(status.icon);
  const description = [verdict, version.short].filter((part) => part !== "").join(" - ");

  const endpoint = cluster.endpoint.trim();
  // An idle row's status "verdict" is the bare endpoint (pinned by the status
  // tests); rendered here under its label so the hover has exactly one
  // address line whatever the state.
  const lines =
    status.icon === "idle" && endpoint !== ""
      ? [`Endpoint: ${endpoint}`]
      : [briefMessage(status.tooltip)];
  if (status.icon === "failed") {
    lines.push("Full error: the MemQL Connection output channel.");
  }
  if (status.icon !== "idle" && endpoint !== "") {
    lines.push(`Endpoint: ${endpoint}`);
  }
  if (version.state !== "unknown") {
    lines.push(version.sentence);
  }
  const skew = recordedSkewSentence(cluster.version);
  if (skew !== "") {
    lines.push(skew);
  }
  return { description, tooltip: lines.join("\n") };
}
