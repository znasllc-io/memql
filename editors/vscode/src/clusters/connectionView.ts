// What the connection page says about one cluster.
//
// THIS IS THE QUESTION THE DELETED PANEL NEVER ANSWERED. The topology view drew
// a pod grid, orphan verdicts and under-replica alarms -- cluster state, which
// the portal owns and already draws. What no surface answered was the one an
// operator actually arrives with when a cluster will not come up: WHAT IS THIS
// EDITOR DIALLING, AS WHOM, AND WHAT HAPPENED. Every field below is on this
// side of the boundary -- clusters.yaml, VS Code SecretStorage, the live
// connection -- and none of it is anything the portal knows or could show.
//
// The wording and the choice of state live here rather than in the panel, so
// they are testable under bare `node --test` -- the same split
// clusters/status.ts makes for the tree row.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3742 #3733

import { classifyToken } from "../connection/credentials.js";
import { identityBaseUrlFor } from "../connection/endpoint.js";
import type { ConnectionState } from "../connection/manager.js";
import type { ImageSource } from "../install/receipt.js";
import { briefMessage } from "../state/diagnostics.js";
import type { ClusterConfig } from "./model.js";
import { describeVersion } from "../version/describe.js";
import type { ReleaseListing } from "../version/releaseCache.js";
import { clusterRowStatus, type ClusterRowIcon } from "./status.js";

/** One label/value line on the page. */
export interface ConnectionFact {
  key: string;
  value: string;
  /** A short verdict rendered beside the value, or "" when there is none. */
  note: string;
}

export interface ConnectionViewInput {
  cluster: ClusterConfig;
  state: ConnectionState;
  /** The signed-in identity, when a live read produced one. */
  identity?: { email: string; role: string };
  /** The access token's absolute expiry, in epoch seconds. */
  expiresAtEpochSeconds?: number;
  /** Now, in epoch milliseconds. Injected so the duration is testable. */
  nowMs: number;
  /**
   * The release listing, when one has been fetched (memql#3992).
   *
   * Optional because ACTIVATION DOES NO NETWORK WORK: the first render of this
   * page happens before anything has been fetched, and an absent listing is an
   * ordinary state that renders as "we do not know what exists" rather than as
   * an error or, worse, as "up to date".
   */
  listing?: ReleaseListing;
  /**
   * The local checkout the receipt records, when there is one (memql#4246).
   *
   * Absent for a remote cluster and for a local one whose receipt records no
   * checkout -- the two facts below render only when this is present, because
   * there is nothing honest to say about a checkout this editor has no
   * evidence of.
   */
  checkout?: { path: string; ref: string; imageSource: ImageSource | "" };
}

export interface ConnectionView {
  title: string;
  icon: ClusterRowIcon;
  /** The one-line verdict, from the same module the tree row reads. */
  summary: string;
  connection: ConnectionFact[];
  identity: ConnectionFact[];
}

export function connectionView(input: ConnectionViewInput): ConnectionView {
  const { cluster, state } = input;
  const status = clusterRowStatus(cluster, state);
  const active = state.status !== "disconnected" && state.clusterName === cluster.name;
  const version = describeVersion({ recorded: cluster.version, listing: input.listing });

  const connection: ConnectionFact[] = [
    {
      key: "endpoint",
      value: cluster.endpoint.trim() === "" ? "not configured" : cluster.endpoint,
      note: reachabilityNote(state, active),
    },
    {
      key: "issuer",
      // Derived rather than stored when the entry carries no explicit one --
      // `identity.<domain>` is the other half of the same convention that
      // composes the endpoint, and showing the DERIVED value is the point: it
      // is what a refresh will actually POST to.
      value: identityBaseUrlFor(cluster) ?? "cannot be derived (no issuer and no domain)",
      note: (cluster.issuer ?? "").trim() === "" ? "derived from the domain" : "",
    },
    {
      key: "local",
      value: cluster.local === true ? "yes" : "no",
      // The sentence that makes Remove safe to press, said where the operator
      // is looking at the cluster rather than only in the dialog.
      note:
        cluster.local === true
          ? "uninstalling it is a Deployments action, not a Clusters one"
          : "",
    },
    {
      key: "version",
      // ALWAYS PRESENT, including with nothing recorded (memql#3995). On a tree
      // row "unknown" on every cluster would be noise; here it is the answer to
      // a question the operator asked by opening the page, and the alternative
      // -- omitting the fact -- reads as "this cluster has no version" rather
      // than "nobody has learned one".
      //
      // Read off clusters.yaml, so it answers with the cluster DISCONNECTED.
      // That is the property this surface exists for: the motivating incident
      // happened on a session that had just been severed, and a version only a
      // live connection could report would have been missing exactly then.
      value: version.state === "unknown" ? "unknown" : (cluster.version ?? "").trim(),
      note: version.sentence,
    },
  ];

  // THE RECORDED CHECKOUT, two facts appended after version (memql#4246).
  // Where it is, and which lane -- released or checkout -- actually set the
  // images this cluster is running right now. The second is not implied by
  // the first: a checkout can sit there unused while the cluster runs a
  // released install, and this is the row that says so rather than letting
  // the directory's mere presence be read as "your edits are live".
  if (input.checkout !== undefined) {
    const { checkout } = input;
    connection.push(
      { key: "checkout", value: checkout.path, note: checkout.ref },
      {
        key: "image source",
        value:
          checkout.imageSource === "checkout"
            ? "checkout (built locally)"
            : checkout.imageSource === "released"
              ? "released"
              : "unknown",
        note:
          checkout.imageSource === "checkout"
            ? "an install, upgrade or repair returns it to released images"
            : "",
      },
    );
  }

  return {
    title: cluster.name,
    icon: status.icon,
    summary: status.tooltip,
    connection,
    identity: identityFacts(input, active),
  };
}

/**
 * What the live connection says about this endpoint.
 *
 * "reachable" is claimed only while a session is actually up. A cluster this
 * editor has not dialled is not known to be unreachable -- it is UNKNOWN, and
 * saying so is the difference between "your cluster is down" and "nobody has
 * asked it", which are the two answers an operator is trying to tell apart.
 */
function reachabilityNote(state: ConnectionState, active: boolean): string {
  if (!active) return "not dialled from this editor";
  switch (state.status) {
    case "connected":
      return `reachable (node ${state.nodeId})`;
    case "connecting":
      return "dialling";
    case "error":
      // Brief, per the information policy (memql#4194, audit 23): the raw
      // transport error is in the MemQL Connection output channel.
      return `did not answer: ${briefMessage(state.message)} (full error: the MemQL Connection output channel)`;
    default:
      return "";
  }
}

function identityFacts(input: ConnectionViewInput, active: boolean): ConnectionFact[] {
  const { cluster } = input;
  const facts: ConnectionFact[] = [];

  facts.push({
    key: "signed in as",
    value:
      input.identity?.email !== undefined && input.identity.email !== ""
        ? input.identity.email
        : active
          ? "connected, but the access read produced no identity"
          : "not signed in",
    note: "",
  });

  if (input.identity?.role !== undefined && input.identity.role !== "") {
    facts.push({ key: "role", value: input.identity.role, note: "" });
  }

  facts.push({
    key: "token",
    value: tokenValue(cluster, input.expiresAtEpochSeconds, input.nowMs),
    // The reason an expiry is not an alarm: identity issues 900-second access
    // tokens by design, and the resolver renews them from the refresh token it
    // holds in SecretStorage. An operator who reads "expires in 3m" without
    // this reads a countdown to being logged out.
    note:
      input.expiresAtEpochSeconds === undefined ? "" : "renews automatically from the refresh token",
  });

  return facts;
}

function tokenValue(
  cluster: ClusterConfig,
  expiresAt: number | undefined,
  nowMs: number,
): string {
  const kind = classifyToken(cluster.token);
  if (kind === "pat" || kind === "workerToken") {
    // Named rather than described: a PAT here fails exactly like a forged
    // token, and it is the one credential problem an operator cannot diagnose
    // from the failure (memql#3383).
    return `a ${kind === "pat" ? "personal access token" : "worker token"}, which the mesh cannot verify`;
  }
  if ((cluster.token ?? "").trim() === "") return "none stored";
  if (expiresAt === undefined) return "stored, with no recorded expiry";
  return `expires ${formatExpiry(expiresAt, nowMs)}`;
}

/**
 * How long a token has left, as a duration.
 *
 * A DURATION, not a timestamp: "expires in 11m" is read at a glance and
 * "expires at 14:32:07Z" is arithmetic the operator has to do while
 * diagnosing something else. Coarse for the same reason -- the exact second is
 * never the question.
 *
 * An expiry in the PAST says so rather than counting negatively, because an
 * expired token and a token with four seconds left ask for different things.
 */
export function formatExpiry(expiresAtEpochSeconds: number, nowMs: number): string {
  const remainingMs = expiresAtEpochSeconds * 1000 - nowMs;
  if (remainingMs <= 0) return "already -- it will renew on the next call";
  const minutes = Math.floor(remainingMs / 60_000);
  if (minutes < 1) return "in under a minute";
  if (minutes < 60) return `in ${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return hours < 24 ? `in ${hours}h` : `in ${Math.floor(hours / 24)}d`;
}
