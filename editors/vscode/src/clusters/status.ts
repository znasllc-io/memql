// What a cluster's row, page and menus say about its state.
//
// ONE STATE MACHINE FOR EVERY SURFACE. The Clusters row, the cluster page, the
// row's menus and the `memql.connectionState` context key all read the state
// computed here, so they cannot disagree about one cluster -- which is what the
// old page did, saying "no credential", "did not answer" and "connected, but
// the access read produced no identity" at once.
//
//   connected      this editor holds a live session to it
//   connecting     a dial is in flight (or a dropped connection is retrying)
//   signIn         the credential is the problem: none, expired, refused, or
//                  of a class the mesh cannot verify
//   unreachable    the cluster is the problem: the dial failed, or a live
//                  connection dropped and stopped retrying
//   notConfigured  there is no address to dial
//   idle           configured, with a session this editor could use, and not
//                  the one it is connected to
//
// AT REST, "signIn" IS CLAIMED ONLY WHEN NOTHING COULD RENEW SILENTLY. It used
// to follow clusters.yaml alone, and the thirty-day refresh token lives in
// SecretStorage -- so a row said "needs sign-in" for a cluster a click would
// simply connect. The at-rest half reads `ClusterFacts` (facts.ts), which
// looks in both places.
//
// THE WORDS are the person's: "Connected", "Sign in", "Connecting", "Not
// running" (a local cluster), "Can't reach" (a remote one), "Not set up". The
// full reason a dial failed goes to the MemQL Connection output, never here.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { isUntrustedCertificate } from "../auth/errors.js";
import { classifyToken } from "../connection/credentials.js";
import { hostOf } from "../connection/endpoint.js";
import type { ConnectionErrorReason, ConnectionState } from "../connection/manager.js";
import { DEFAULT_STACK_TAG } from "../install/stackPin.js";
import { compareVersions } from "../version/compare.js";
import { describeVersion } from "../version/describe.js";
import type { ReleaseListing } from "../version/releaseCache.js";
import type { ClusterFacts } from "./facts.js";
import { displayLabel, type ClusterConfig } from "./model.js";

export type ClusterState = "connected" | "connecting" | "signIn" | "unreachable" | "notConfigured" | "idle";

/** Why a cluster is in the `signIn` state, which decides the one sentence said about it. */
export type SignInReason = "missing" | "expired" | "refused" | "wrongToken";

export interface ClusterStatus {
  state: ClusterState;
  /** Set in the `signIn` state. */
  signInReason?: SignInReason;
  /** Set in the `unreachable` state: a connection that dropped, rather than a dial that failed. */
  lost?: boolean;
  /**
   * Set in the `unreachable` state when the dial failed on a certificate this
   * computer does not trust -- the cluster answered, and Repair (for a local
   * cluster) is the fix, not a retry.
   */
  untrusted?: boolean;
}

export interface StatusInput {
  cluster: ClusterConfig;
  connection: ConnectionState;
  facts: ClusterFacts;
}

// The reasons that mean "look at your credential", not "look at your cluster".
const SIGN_IN_REASONS: Readonly<Partial<Record<ConnectionErrorReason, SignInReason>>> = {
  missingCredential: "missing",
  credentialExpired: "expired",
  reauthenticationRequired: "refused",
  wrongTokenClass: "wrongToken",
};

/** The state of one cluster, from the live connection when it names this cluster, else from what is stored. */
export function clusterStatus(input: StatusInput): ClusterStatus {
  const { cluster, connection } = input;
  const active = connection.status !== "disconnected" && connection.clusterName === cluster.name;
  if (active) {
    switch (connection.status) {
      case "connected":
        return { state: "connected" };
      case "connecting":
        return { state: "connecting" };
      case "error": {
        const reason = SIGN_IN_REASONS[connection.reason];
        if (reason !== undefined) return { state: "signIn", signInReason: reason };
        if (connection.reason === "notConfigured") return { state: "notConfigured" };
        // A dropped connection being retried reads as connecting, not as an
        // outage: another try is scheduled (ConnectionManager's ReconnectPolicy).
        if (connection.retrying === true) return { state: "connecting" };
        return {
          state: "unreachable",
          lost: connection.reason === "lost",
          ...(isUntrustedCertificate(connection.message) ? { untrusted: true } : {}),
        };
      }
    }
  }
  if (cluster.endpoint.trim() === "") return { state: "notConfigured" };
  const tokenClass = classifyToken(cluster.token);
  if (tokenClass === "pat" || tokenClass === "workerToken") return { state: "signIn", signInReason: "wrongToken" };
  if (!input.facts.session) return { state: "signIn", signInReason: input.facts.signedIn ? "expired" : "missing" };
  return { state: "idle" };
}

/** The state in the row's words. Empty for an idle cluster that is not the one in use. */
export function stateWord(status: ClusterStatus, cluster: ClusterConfig, inUse: boolean): string {
  switch (status.state) {
    case "connected":
      return "Connected";
    case "connecting":
      return "Connecting";
    case "signIn":
      return "Sign in";
    case "unreachable":
      // A certificate this computer does not trust is not a stopped cluster.
      return cluster.local === true && status.untrusted !== true ? "Not running" : "Can't reach";
    case "notConfigured":
      return "Not set up";
    case "idle":
      return inUse ? "Not connected" : "";
  }
}

/** One short sentence about the state, for a tooltip or the page's action bar detail. */
export function stateSentence(status: ClusterStatus, cluster: ClusterConfig): string {
  switch (status.state) {
    case "connected":
      return "Connected.";
    case "connecting":
      return "Connecting.";
    case "signIn":
      switch (status.signInReason) {
        case "expired":
        case "refused":
          return "Your session ended.";
        case "wrongToken":
          return "The saved token can't be used here.";
        default:
          return "Not signed in.";
      }
    case "unreachable":
      if (status.untrusted === true) return "This computer doesn't trust the cluster's certificate.";
      if (status.lost === true) return "The connection was lost.";
      return cluster.local === true ? "The local cluster isn't answering." : "This cluster isn't answering.";
    case "notConfigured":
      return "No address set.";
    case "idle":
      return "Not connected.";
  }
}

/**
 * The version, only when it tells the person something: a newer release
 * exists, or the cluster is older than this extension expects. A version that
 * is simply current (or `main`) is on the page and in the tooltip, not on
 * every row.
 */
export function versionNote(cluster: ClusterConfig, listing: ReleaseListing | undefined): string {
  const described = describeVersion({ recorded: cluster.version, listing });
  if (described.state === "behind" && described.latest !== undefined) return `${described.latest} available`;
  if (isOlderThanExtension(cluster.version)) return "Needs an update";
  return "";
}

/** Whether the recorded version is behind the release this extension build is pinned to. */
export function isOlderThanExtension(recorded: string | undefined): boolean {
  const version = (recorded ?? "").trim();
  return version !== "" && compareVersions(version, DEFAULT_STACK_TAG) === "behind";
}

/** The two pieces of text a Clusters row renders beside its icon, and what assistive tech reads. */
export interface ClusterRowText {
  /** The dimmed words after the name: state and a version note, joined by " · ". */
  description: string;
  /** Three short lines at most: the state, the address, the version. */
  tooltip: string;
  /** "memql.localhost, Connected, in use". */
  accessibilityLabel: string;
}

export function clusterRowText(
  cluster: ClusterConfig,
  status: ClusterStatus,
  inUse: boolean,
  listing: ReleaseListing | undefined,
): ClusterRowText {
  const word = stateWord(status, cluster, inUse);
  const description = [word, versionNote(cluster, listing)].filter((part) => part !== "").join(" · ");

  const lines = [stateSentence(status, cluster)];
  const host = hostOf(cluster.endpoint) ?? cluster.endpoint.trim();
  if (host !== "") lines.push(host);
  const recorded = (cluster.version ?? "").trim();
  if (recorded !== "") {
    const described = describeVersion({ recorded, listing });
    lines.push(
      described.state === "behind" && described.latest !== undefined
        ? `Version ${recorded} · ${described.latest} available`
        : `Version ${recorded}`,
    );
  }

  const spoken = [displayLabel(cluster), word === "" ? "not connected" : word];
  if (inUse) spoken.push("in use");
  return { description, tooltip: lines.join("\n"), accessibilityLabel: spoken.join(", ") };
}

/**
 * The row's contextValue: the kind and the flags the menus are gated on,
 * joined by ";" so a `when` clause matches one with `viewItem =~ /;flag(;|$)/`.
 *
 *   memqlCluster;<state>[;local][;signedIn][;ownerSetup][;os][;inUse]
 *
 * Every row starts with `memqlCluster`, so a clause keyed on the prefix
 * reaches every row, and the state token is always the second.
 */
export function clusterContextValue(
  cluster: ClusterConfig,
  status: ClusterStatus,
  facts: ClusterFacts,
  inUse: boolean,
): string {
  const parts = ["memqlCluster", status.state];
  if (cluster.local === true) parts.push("local");
  if (facts.signedIn) parts.push("signedIn");
  if (facts.ownerSetup && status.state !== "connected") parts.push("ownerSetup");
  if (facts.consoleUrl !== "") parts.push("os");
  if (inUse) parts.push("inUse");
  return parts.join(";");
}
