// Whether this machine already HAS a local memQL cluster, and whether it answers.
//
// The "+" button in the Clusters view used to mean exactly one thing: register
// a remote cluster. It could not tell an operator who has no cluster at all
// that installing one is an option, and -- worse -- it could not tell one who
// already has a cluster that they do. This module is the evidence half of
// memql#3412; the menu the "+" renders from the verdict is at the bottom of
// this file, and the VS Code wiring is in src/extension.ts.
//
// THREE PROPERTIES ARE LOAD-BEARING.
//
//  1. TWO EVIDENCE SOURCES, EITHER SUFFICIENT. The install receipt
//     (~/.memql/install-receipt.json) records an install this extension drove.
//     A `local: true` entry in clusters.yaml records one the operator built by
//     hand with `make up` and registered themselves. Reading only the receipt
//     would miss the hand-built cluster and then cheerfully offer to install
//     over the top of it, which is how a working parity cluster gets replaced
//     by a fresh one.
//  2. THE HEALTH PROBE IS A DIAL, NOT A DOCKER QUERY. Rendering a menu must
//     not require Docker to be running, must not shell out, and must not wait
//     on `k3d cluster list`. It asks the front door whether it answers, over
//     the same WebSocket bridge the connection layer dials, with a short
//     deadline this module enforces itself.
//  3. A SLOW PROBE DEGRADES, IT DOES NOT HANG. The deadline is raced HERE
//     rather than trusted to the probe, so an injected or future probe that
//     never settles still yields `installed-unreachable` and the menu still
//     opens. The verdict exists to choose between three menus; being wrong for
//     a second costs a menu item, whereas blocking the button costs the
//     feature.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3412 #3401

import { WebSocket as NodeWebSocket } from "ws";

import { composeEndpointFromDomain, webSocketUrlFor } from "../connection/endpoint.js";
import { defaultReceiptPath, readReceipt, type Receipt } from "../install/receipt.js";
import { readClustersFileSafe } from "./file.js";
import type { ClusterConfig } from "./model.js";

/**
 * What the "+" is looking at.
 *
 * `absent` is the ONLY verdict that may offer an install. The other two both
 * mean "something is already here", and differ only in whether it answers --
 * which is the difference between offering a connection and offering a repair.
 */
export type PresenceVerdict = "absent" | "installed-healthy" | "installed-unreachable";

/** Which of the two independent sources said a local cluster exists. */
export interface PresenceEvidence {
  /** An install receipt on this machine records at least one executed step. */
  receipt: boolean;
  /** clusters.yaml carries an entry flagged `local: true`. */
  registry: boolean;
}

export interface PresenceResult {
  verdict: PresenceVerdict;
  evidence: PresenceEvidence;
  /**
   * The endpoint the probe dialed. Empty exactly when there was no evidence,
   * because nothing is dialed in that case -- see detectPresence.
   */
  endpoint: string;
  /** The registry name of the local cluster, when one is registered. */
  clusterName?: string;
}

/**
 * A reachability check for one endpoint: does the front door answer?
 *
 * Returns a boolean rather than throwing, and the boolean is deliberately NOT
 * "did we complete a session". A 401, a redirect, an untrusted certificate --
 * all of them are a server answering, which is what distinguishes "your
 * cluster is down" from "your token needs renewing". The latter is the
 * connection layer's story to tell (src/clusters/status.ts), not this one's.
 */
export type EndpointProbe = (endpoint: string, timeoutMs: number) => Promise<boolean>;

/**
 * The dial deadline.
 *
 * Short on purpose: this runs on the click of a toolbar button, and the cost
 * of being wrong is one extra menu item. A local cluster on loopback answers
 * in single-digit milliseconds; anything that takes longer than this is not a
 * cluster an operator would call healthy either.
 */
export const PROBE_TIMEOUT_MS = 1_500;

/**
 * How long a verdict is reused.
 *
 * Long enough that opening the menu twice in a row does not dial twice, short
 * enough that a cluster started (or stopped) in the meantime is noticed
 * without anyone reaching for Refresh. Install and uninstall do not wait for
 * it -- they call invalidate(), because they are the two events that change
 * the answer deterministically.
 */
export const PRESENCE_TTL_MS = 30_000;

/**
 * The front door of a locally-installed cluster, when nothing names it.
 *
 * The same hostname scripts/install/hosts-entries.sh writes into the hosts
 * file (DEFAULT_HOSTNAMES) and the local k8s overlay serves on 443. It is only
 * ever reached for a receipt that recorded no `--domain`, i.e. an install that
 * took every default.
 */
export const DEFAULT_LOCAL_ENDPOINT = "cockpit.local.znas.io:443";

/** The synthetic name the probe dials under; never written anywhere. */
const PROBE_CLUSTER_NAME = "local";

// ---------------------------------------------------------------------------
// evidence
// ---------------------------------------------------------------------------

/**
 * Whether the install receipt is evidence of a cluster on this machine.
 *
 * Three outcomes, and the middle one is the interesting one:
 *
 *   - no file            -> no evidence. Nothing was ever installed from here.
 *   - unreadable file    -> EVIDENCE. A receipt that will not parse cannot rule
 *                           an install out, and the only action gated on the
 *                           absence of evidence is one that installs OVER
 *                           whatever is there. Refusing to offer it is the
 *                           direction that cannot destroy anything.
 *   - parsed, no entries -> no evidence. An install that recorded zero steps
 *                           left nothing on the machine to protect.
 */
async function receiptEvidence(
  file: string,
  read: (f: string) => Promise<Receipt | null>,
): Promise<{ present: boolean; receipt: Receipt | null }> {
  try {
    const receipt = await read(file);
    if (receipt === null) return { present: false, receipt: null };
    return { present: receipt.entries.length > 0, receipt };
  } catch {
    return { present: true, receipt: null };
  }
}

/**
 * The `local: true` entry in clusters.yaml, if there is one.
 *
 * ABSENT MEANS NOT LOCAL (see ClusterConfig.local), so this is a strict `===
 * true` and never a truthiness test: every cluster registered before that
 * field existed carries no flag, and reading those as local would report an
 * operator's staging cluster as the local install.
 *
 * A clusters.yaml that cannot be read yields NO registry evidence rather than
 * an error. The file is shared with the Cockpit and the tree already renders
 * that failure as a row of its own; a broken file must not also decide what
 * the "+" offers, and the receipt is still free to supply evidence on its own.
 */
async function registryEvidence(
  file: string,
  read: (f: string) => ReturnType<typeof readClustersFileSafe>,
): Promise<ClusterConfig | undefined> {
  const result = await read(file);
  if (!result.ok) return undefined;
  return result.file.clusters.find((c) => c.local === true);
}

/**
 * Which endpoint the health probe should dial.
 *
 * In preference order, because each source is more specific than the next:
 *
 *   1. the registered local cluster's endpoint -- an operator who wrote one
 *      down is naming the front door they actually use;
 *   2. the domain the install recorded (`seedBootstrap` carries `--domain`),
 *      lifted to the front-door hostname by composeEndpointFromDomain -- the
 *      one spelling of the `cockpit.<domain>` convention (memql#3475), which
 *      this function used to carry a private copy of;
 *   3. the installer's own default.
 */
export function probeEndpointFor(
  local: ClusterConfig | undefined,
  receipt: Receipt | null,
): string {
  const registered = (local?.endpoint ?? "").trim();
  if (registered !== "") return registered;

  for (const entry of receipt?.entries ?? []) {
    // An entry that recorded no domain composes to "", which is this loop's
    // "keep looking" -- the emptiness check the copy here used to make.
    const composed = composeEndpointFromDomain(entry.params.domain ?? "");
    if (composed !== "") return composed;
  }
  return DEFAULT_LOCAL_ENDPOINT;
}

/** The pure half: evidence plus reachability decide the verdict. */
export function verdictFor(evidence: PresenceEvidence, answered: boolean): PresenceVerdict {
  if (!evidence.receipt && !evidence.registry) return "absent";
  return answered ? "installed-healthy" : "installed-unreachable";
}

// ---------------------------------------------------------------------------
// detection
// ---------------------------------------------------------------------------

export interface PresenceOptions {
  /** ~/.memql/clusters.yaml. */
  clustersPath: string;
  /** ~/.memql/install-receipt.json. */
  receiptPath?: string;
  probe?: EndpointProbe;
  probeTimeoutMs?: number;
  ttlMs?: number;
  /** Injectable clock, so the memo window is testable without waiting. */
  now?: () => number;
  readReceiptFile?: (file: string) => Promise<Receipt | null>;
  readClusters?: (file: string) => ReturnType<typeof readClustersFileSafe>;
}

/**
 * Reads both evidence sources and, if either fired, probes the endpoint.
 *
 * NO PROBE WITHOUT EVIDENCE. There is nothing to dial when nothing is
 * installed, the verdict is `absent` either way, and dialing anyway would put
 * a network round trip in front of the menu for the one operator who most
 * needs it to open promptly -- the one with no cluster at all.
 *
 * Never rejects. Every caller is a UI affordance whose alternative to a
 * verdict is a button that does nothing.
 */
export async function detectPresence(opts: PresenceOptions): Promise<PresenceResult> {
  const readReceiptFile = opts.readReceiptFile ?? readReceipt;
  const readClusters = opts.readClusters ?? readClustersFileSafe;
  const probe = opts.probe ?? defaultEndpointProbe;
  const timeoutMs = opts.probeTimeoutMs ?? PROBE_TIMEOUT_MS;
  const receiptPath = opts.receiptPath ?? defaultReceiptPath();

  const [fromReceipt, local] = await Promise.all([
    receiptEvidence(receiptPath, readReceiptFile),
    registryEvidence(opts.clustersPath, readClusters).catch(() => undefined),
  ]);

  const evidence: PresenceEvidence = { receipt: fromReceipt.present, registry: local !== undefined };
  if (!evidence.receipt && !evidence.registry) {
    return { verdict: "absent", evidence, endpoint: "" };
  }

  const endpoint = probeEndpointFor(local, fromReceipt.receipt);
  const answered = await withDeadline(() => probe(endpoint, timeoutMs), timeoutMs);
  return {
    verdict: verdictFor(evidence, answered),
    evidence,
    endpoint,
    clusterName: local?.name,
  };
}

/**
 * Runs `work`, answering false if it rejects or outlives the deadline.
 *
 * The deadline is enforced here rather than left to the probe because "never
 * hangs the menu" is a property of this module, not a promise extracted from
 * whatever function was injected. The abandoned promise is left to settle on
 * its own -- the default probe tears its socket down on the same deadline, and
 * an injected one has nothing to tear down.
 *
 * The timer is NOT unref'd. It is always cleared once the race settles, so it
 * outlives nothing; unref'ing it instead would let a process whose only
 * remaining work is this deadline decide it has nothing left to do, which is
 * how a probe with nothing else pending resolves as "the event loop drained"
 * rather than as a verdict.
 */
async function withDeadline(work: () => Promise<boolean>, timeoutMs: number): Promise<boolean> {
  let timer: NodeJS.Timeout | undefined;
  const expiry = new Promise<boolean>((resolve) => {
    timer = setTimeout(() => resolve(false), timeoutMs);
  });
  try {
    return await Promise.race([work().catch(() => false), expiry]);
  } catch {
    return false;
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

/**
 * A failure that could only happen AFTER a server answered.
 *
 * TLS verification stays ON in the probe -- a reachability check is no reason
 * to disable certificate checking, and a probe that trusted anything would be
 * a probe an attacker on the path could answer for. But a certificate that
 * fails to verify is still a certificate, and a certificate can only have been
 * presented by something serving that address. A local cluster is fronted by a
 * mkcert CA the extension host's Node has no reason to trust, so treating that
 * class as "unreachable" would report a perfectly healthy local cluster as
 * down and offer to repair it.
 *
 * The distinction is exactly "did something respond", which is the only
 * question this module asks. Nothing is read off the socket, so classifying
 * the failure grants no trust to whatever produced it.
 */
function isCertificateFailure(err: unknown): boolean {
  const e = err as { code?: unknown; message?: unknown };
  const code = typeof e?.code === "string" ? e.code : "";
  const message = typeof e?.message === "string" ? e.message : "";
  // Unanchored on purpose, and written that way rather than left to
  // precedence. `/^ERR_TLS|CERT|_SIGNED_/` parses as
  // `(^ERR_TLS)|(CERT)|(_SIGNED_)` -- the anchor binds to the first
  // alternative alone, so two of the three branches were already substring
  // matches while the shape of the expression implied all three were anchored.
  // Substring is the behaviour this wants: these are Node TLS error codes
  // (ERR_TLS_CERT_ALTNAME_INVALID, DEPTH_ZERO_SELF_SIGNED_CERT,
  // UNABLE_TO_VERIFY_LEAF_SIGNATURE, CERT_HAS_EXPIRED), and the marker can sit
  // anywhere in the code. Dropping the anchor makes the three branches agree.
  return /ERR_TLS|CERT|_SIGNED_/i.test(code) || /certificate|self[- ]signed/i.test(message);
}

/**
 * The default probe: open the bridge URL, and treat any ANSWER as an answer.
 *
 * Deliberately not a Connection.dial. This asks one question -- is something
 * serving the front door -- and a real dial would additionally resolve a
 * credential, possibly REFRESH it, and fail on a cluster that is up but whose
 * token expired. That is the wrong verdict from the wrong side effect. So:
 *
 *   - no bearer is sent, so an unauthenticated rejection is still an answer
 *     (`unexpected-response` is an HTTP status from a live server);
 *   - a certificate that does not verify is an answer too (see above), while
 *     verification itself stays on;
 *   - the socket is torn down the moment the question is answered.
 */
export const defaultEndpointProbe: EndpointProbe = (endpoint, timeoutMs) =>
  new Promise<boolean>((resolve) => {
    let url: string;
    try {
      url = webSocketUrlFor({ name: PROBE_CLUSTER_NAME, endpoint });
    } catch {
      // An endpoint that cannot even be lifted to a URL answers nothing.
      resolve(false);
      return;
    }

    let settled = false;
    const socket = new NodeWebSocket(url, { handshakeTimeout: timeoutMs });
    const finish = (answered: boolean): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        socket.terminate();
      } catch {
        // A socket that is already gone needs no tearing down.
      }
      resolve(answered);
    };
    // Cleared by finish(), which every exit path goes through.
    const timer = setTimeout(() => finish(false), timeoutMs);

    socket.on("open", () => finish(true));
    // ws only emits this when a listener exists; without one the HTTP response
    // arrives as a plain error and a live-but-unauthenticated server would read
    // as unreachable.
    socket.on("unexpected-response", () => finish(true));
    socket.on("error", (err) => finish(isCertificateFailure(err)));
  });

/**
 * The memoized front door: what the "+" command asks.
 *
 * One in-flight detection is shared rather than duplicated, so a double click
 * on the button is one dial. The window is otherwise plain TTL -- see
 * PRESENCE_TTL_MS for why it is not longer, and invalidate() for what does not
 * wait for it.
 */
export class ClusterPresence {
  private cached: { at: number; result: PresenceResult } | undefined;
  private inflight: Promise<PresenceResult> | undefined;

  constructor(private readonly opts: PresenceOptions) {}

  /**
   * Drops the memo.
   *
   * MUST be called when an install or an uninstall completes: those are the
   * two events that change the answer deterministically, and waiting out a
   * 30-second window afterwards would show an operator who just installed a
   * cluster the menu for someone who has none.
   */
  invalidate(): void {
    this.cached = undefined;
  }

  async get(): Promise<PresenceResult> {
    const now = (this.opts.now ?? Date.now)();
    const ttl = this.opts.ttlMs ?? PRESENCE_TTL_MS;
    if (this.cached !== undefined && now - this.cached.at < ttl) {
      return this.cached.result;
    }
    if (this.inflight !== undefined) return this.inflight;

    this.inflight = detectPresence(this.opts)
      .then((result) => {
        this.cached = { at: (this.opts.now ?? Date.now)(), result };
        return result;
      })
      .finally(() => {
        this.inflight = undefined;
      });
    return this.inflight;
  }
}

// ---------------------------------------------------------------------------
// the menu the verdict produces
// ---------------------------------------------------------------------------

/**
 * What the "+" can offer.
 *
 * `install` and `installGuided` are separate actions rather than one action
 * with a mode flag, because the choice is made BEFORE any work starts and
 * changes what the first screen asks for: Automatic runs each step's capability
 * and then verifies it, Guided renders the command, waits, and polls the same
 * verify. Same graph, same verdict of done -- different screen.
 *
 * `uninstall` is the gap memql#3471 closes. `cli.js uninstall` has existed
 * since the substrate epic (#3357); nothing in the editor could reach it, so a
 * local cluster could be repaired but never removed from the machine.
 */
export type AddClusterAction =
  | "install"
  | "installGuided"
  | "connect"
  | "repair"
  | "uninstall";

export interface AddClusterChoice {
  action: AddClusterAction;
  label: string;
  detail: string;
}

const CONNECT: AddClusterChoice = {
  action: "connect",
  label: "Connect to an existing cluster...",
  detail: "Register a cluster you already have -- local, staging or production.",
};

const UNINSTALL: AddClusterChoice = {
  action: "uninstall",
  label: "Uninstall the local cluster...",
  detail: "Remove it from this machine. You will see exactly what goes first.",
};

/**
 * What the "+" offers, given the verdict.
 *
 * INSTALL APPEARS FOR `absent` AND FOR NOTHING ELSE. That is the whole point
 * of the evidence pass: an install run over a cluster that already exists is
 * not a wasted click, it is a k3d cluster and a hosts block and a trust-store
 * CA rebuilt underneath a working parity stack. Both install variants obey it.
 *
 * UNINSTALL IS THE EXACT COMPLEMENT: offered for both verdicts that say
 * something is here, and never for `absent`, where there is nothing to remove.
 *
 * ORDER IS THE ONLY RECOMMENDATION A CARD LIST CAN MAKE, so the first entry is
 * what the operator most likely came for. On `installed-unreachable` that is
 * repair -- they came to fix a broken cluster, not to register a second one.
 *
 * The one-item-means-no-picker rule this function used to carry is gone with
 * the quick pick that needed it: `installed-healthy` now has two entries, and
 * the caller renders cards rather than a picker either way (memql#3472).
 */
export function addClusterMenu(verdict: PresenceVerdict): AddClusterChoice[] {
  switch (verdict) {
    case "absent":
      return [
        {
          action: "install",
          label: "Install a local cluster",
          detail: "Recommended. Build a local memQL cluster on this machine.",
        },
        {
          action: "installGuided",
          label: "Install a local cluster -- guided",
          detail:
            "Same steps, but each command is shown for you to run yourself. " +
            "Use this when a step needs elevation you would rather grant by hand.",
        },
        CONNECT,
      ];
    case "installed-unreachable":
      return [
        {
          action: "repair",
          label: "Repair the local cluster",
          detail: "A local cluster is installed but is not answering.",
        },
        UNINSTALL,
        CONNECT,
      ];
    case "installed-healthy":
      return [CONNECT, UNINSTALL];
  }
}
