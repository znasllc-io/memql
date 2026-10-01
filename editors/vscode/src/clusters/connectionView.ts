// The cluster page: one cluster, its state, and the acts legal from it.
//
// WHAT IT ANSWERS. Where the cluster is, whether this editor is connected to
// it and as whom, and the one next step. Everything here is on this side of
// the boundary -- clusters.yaml, what this machine has stored, the live
// connection. What runs inside the cluster is MemQL OS's to show, one click
// away.
//
// ONE STATE, SAID ONCE. The page reads the same state machine as the Clusters
// row (status.ts). The old page computed three verdicts from three sources and
// could say "no credential", "did not answer" and "connected, but the access
// read produced no identity" at the same moment; now the action bar carries the
// state in words, the facts carry only facts, and "Signed in as" appears only
// while a connection is actually up.
//
// THE ANATOMY is the kit's (docs/internal/design/2026-09-28-vscode-extension-
// ux.md): a head with the name and version and the record acts (Edit, Remove
// from list); facts; and the action bar -- state on the left, at most three
// legal acts on the right, the one button last. An act that is not legal is
// absent.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { hostOf } from "../connection/endpoint.js";
import type { ConnectionState } from "../connection/manager.js";
import { describeVersion } from "../version/describe.js";
import type { ReleaseListing } from "../version/releaseCache.js";
import {
  actionBar,
  emptyState,
  facts as factsList,
  head,
  notice,
  skeleton,
  type Act,
  type FactRow,
} from "../webview/ui/kit.js";
import type { ClusterFacts } from "./facts.js";
import { displayLabel, type ClusterConfig } from "./model.js";
import { clusterStatus, stateSentence, stateWord, type ClusterStatus } from "./status.js";

/** Where a sign-in to this cluster stands, while one is running. */
export interface SignInFlight {
  /** opening / waiting / finishing a browser sign-in, or showing a device code. */
  phase: "opening" | "waiting" | "finishing" | "code";
  /** Whether "Use a code instead" is on offer (the browser has been waited on long enough). */
  codeOffered: boolean;
}

export interface ClusterPageInput {
  /** The registry name the page is for. */
  clusterName: string;
  /** The entry, or undefined when it is no longer in the list. */
  cluster: ClusterConfig | undefined;
  /**
   * The label the page last showed for this cluster. A cluster removed while
   * its page is open keeps the name the person knew it by, rather than
   * turning into its registry key (`local` for `memql.localhost`).
   */
  knownLabel?: string;
  /** Set when clusters.yaml could not be read at all. */
  registryError?: string;
  /** The facts (facts.ts), once read; undefined while the first read is in flight. */
  facts: ClusterFacts | undefined;
  connection: ConnectionState;
  /**
   * Who the live connection is signed in as. `loading` while the read is in
   * flight, `unavailable` when it failed. Only consulted while connected.
   */
  identity: { email: string; role: string } | "loading" | "unavailable";
  /** MemQL OS for this cluster: its own site row when connected, else composed. "" when unknown. */
  consoleUrl: string;
  listing?: ReleaseListing;
  /** A sign-in to this cluster in flight, if any. */
  signingIn?: SignInFlight;
}

export interface ClusterPage {
  /** The LiveView screen key: one per cluster, so a state change patches rather than repaints. */
  screen: string;
  /** The panel's tab title. */
  title: string;
  head: string;
  body: string;
  actions: string;
}

/** Every act the page can post. The panel maps each to a command. */
export type ClusterPageAct =
  | "edit"
  | "remove"
  | "signIn"
  | "signInWithCode"
  | "takeOwnership"
  | "signOut"
  | "disconnect"
  | "openConsole"
  | "connect"
  | "repair"
  | "cancel"
  | "useCode"
  | "showDetails"
  | "close"
  | "openFile";

const act = (a: ClusterPageAct, label: string, tone: Act["tone"] = "text"): Act => ({ act: a, label, tone });

export function clusterPage(input: ClusterPageInput): ClusterPage {
  const screen = `cluster:${input.clusterName}`;

  if (input.registryError !== undefined) {
    return {
      screen: "cluster:registry-error",
      title: "Clusters",
      head: head({ title: "Clusters" }),
      body: notice({
        tone: "error",
        line: "Can't read your cluster list.",
        acts: [act("openFile", "Open file", "secondary")],
      }),
      actions: "",
    };
  }

  const cluster = input.cluster;
  if (cluster === undefined) {
    const known = (input.knownLabel ?? "").trim() || input.clusterName;
    return {
      screen,
      title: known,
      head: head({ title: known }),
      body: emptyState({ line: "This cluster is no longer in your list.", acts: [act("close", "Close", "secondary")] }),
      actions: "",
    };
  }

  const label = displayLabel(cluster);
  if (input.facts === undefined) {
    return {
      screen,
      title: label,
      head: head({ title: label, meta: versionMeta(cluster, input.listing) }),
      body: skeleton({ shape: "facts", rows: 3, label: "Loading cluster details" }),
      actions: "",
    };
  }

  const status = clusterStatus({ cluster, connection: input.connection, facts: input.facts });
  const notConfigured = status.state === "notConfigured";
  return {
    screen,
    title: label,
    head: head({
      title: label,
      meta: versionMeta(cluster, input.listing),
      // Edit is the action bar's primary when there is no address, and one act
      // is offered in one place.
      asideActs: notConfigured ? [act("remove", "Remove from list")] : [act("edit", "Edit"), act("remove", "Remove from list")],
    }),
    body: factsList(pageFacts(cluster, status, input)),
    actions: pageActionBar(cluster, status, input),
  };
}

/** The version beside the title: what the cluster records, and a newer release when one exists. */
export function versionMeta(cluster: ClusterConfig, listing: ReleaseListing | undefined): string {
  const recorded = (cluster.version ?? "").trim();
  if (recorded === "") return "";
  const described = describeVersion({ recorded, listing });
  return described.state === "behind" && described.latest !== undefined
    ? `${recorded} · ${described.latest} available`
    : recorded;
}

function pageFacts(cluster: ClusterConfig, status: ClusterStatus, input: ClusterPageInput): FactRow[] {
  const rows: FactRow[] = [];
  const address = hostOf(cluster.endpoint) ?? cluster.endpoint.trim();
  rows.push(address === "" ? { label: "Address", value: "Not set", muted: true } : { label: "Address", value: address, mono: true });
  const os = input.consoleUrl === "" ? "" : (hostOf(input.consoleUrl) ?? input.consoleUrl);
  if (os !== "") rows.push({ label: "MemQL OS", value: os, mono: true });
  // ONLY WHILE CONNECTED. Who a session belongs to is a fact about a live
  // connection; in any other state the action bar already says there is none.
  if (status.state === "connected") {
    const identity = input.identity;
    if (identity === "loading") {
      rows.push({
        label: "Signed in as",
        valueHtml:
          `<span class="mq-skel" data-w="m" aria-hidden="true"></span>` +
          `<span class="mq-sr" role="status">Loading your account</span>`,
      });
    } else if (identity === "unavailable") {
      rows.push({ label: "Signed in as", value: "Unknown", muted: true });
    } else {
      const role = identity.role.trim();
      rows.push({ label: "Signed in as", value: role === "" ? identity.email : `${identity.email} · ${role}` });
    }
  }
  if (cluster.local === true) rows.push({ label: "Installed", value: "On this computer" });
  return rows;
}

const PHASE_WORDS: Readonly<Record<SignInFlight["phase"], string>> = {
  opening: "Opening your browser",
  waiting: "Waiting for you in the browser",
  finishing: "Finishing sign-in",
  code: "Enter the code in your browser",
};

function pageActionBar(cluster: ClusterConfig, status: ClusterStatus, input: ClusterPageInput): string {
  const flight = input.signingIn;
  if (flight !== undefined && status.state !== "connected") {
    const acts = [act("cancel", "Cancel")];
    if (flight.codeOffered && flight.phase === "waiting") acts.unshift(act("useCode", "Use a code instead"));
    return actionBar({ state: "Signing in", detail: PHASE_WORDS[flight.phase], tone: "busy", acts });
  }

  const word = stateWord(status, cluster, true);
  const facts = input.facts;
  switch (status.state) {
    case "connected": {
      const acts = [act("signOut", "Sign out"), act("disconnect", "Disconnect")];
      if (input.consoleUrl !== "") acts.push(act("openConsole", "Open MemQL OS", "primary"));
      return actionBar({ state: word, tone: "live", acts });
    }
    case "connecting":
      return actionBar({ state: word, tone: "busy", acts: [act("cancel", "Cancel")] });
    case "signIn": {
      const acts: Act[] = [];
      if (facts?.ownerSetup === true) acts.push(act("takeOwnership", "Create owner passkey"));
      acts.push(act("signInWithCode", "Sign in with a code"), act("signIn", "Sign in", "primary"));
      return actionBar({ state: stateSentence(status, cluster).replace(/\.$/, ""), tone: "warn", acts });
    }
    case "unreachable": {
      if (status.untrusted === true) {
        // The cluster answered; this computer does not trust its certificate.
        // For a local cluster Repair sets the trust up again, so it leads.
        const acts = [act("showDetails", "Show details")];
        if (cluster.local === true) acts.push(act("connect", "Retry"), act("repair", "Repair", "primary"));
        else acts.push(act("connect", "Retry", "primary"));
        return actionBar({ state: "Certificate not trusted", tone: "error", acts });
      }
      const acts = [act("showDetails", "Show details")];
      if (cluster.local === true) acts.push(act("repair", "Repair"));
      acts.push(act("connect", "Retry", "primary"));
      return actionBar({ state: status.lost === true ? "Connection lost" : word, tone: "error", acts });
    }
    case "notConfigured":
      return actionBar({ state: word, tone: "warn", acts: [act("edit", "Edit", "primary")] });
    case "idle": {
      const acts: Act[] = [];
      if (facts?.signedIn === true) acts.push(act("signOut", "Sign out"));
      acts.push(act("connect", "Connect", "primary"));
      return actionBar({ state: word, tone: "idle", acts });
    }
  }
}
