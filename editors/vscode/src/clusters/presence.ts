// Whether this machine already HAS a local MemQL cluster, and whether it answers.
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
//
//     THE FOURTH SIGNAL IS THE ONE EXCEPTION, and it is bounded to the one
//     path where being wrong destroys data (memql#5118, D8). When BOTH
//     evidence sources say nothing the menu offers Install -- and an install
//     over a k3d cluster somebody already had adopts its database. So before
//     offering that, and only then, presence asks `k3d cluster list` through
//     the detect capability. Every other verdict still opens the menu with no
//     shell-out at all, and this one adds no round trip on top of another:
//     `absent` is precisely the case that does not dial. A listing that fails
//     or hangs answers the same as an empty one, which keeps Install offered
//     on the machine that genuinely has nothing.
//  3. A SLOW PROBE DEGRADES, IT DOES NOT HANG. The deadline is raced HERE
//     rather than trusted to the probe, so an injected or future probe that
//     never settles still yields `installed-unreachable` and the menu still
//     opens. The verdict exists to choose between three menus; being wrong for
//     a second costs a menu item, whereas blocking the button costs the
//     feature.
//
//     BUT ONE MISS IS NOT A VERDICT. The first TLS handshake from the editor's
//     extension host can be slow (proxy and certificate patching), and a
//     1.5-second budget reported a cluster that answers in 42 ms from a shell
//     as "not answering". So the budget is about four seconds, a miss is tried
//     once more before it counts, and the reason it missed is logged
//     (`onProbeFailure`, the MemQL Connection output) -- a verdict nobody can
//     explain is one nobody can fix.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3412 #3401

import { WebSocket as NodeWebSocket } from "ws";

import { composeEndpointFromDomain, webSocketUrlFor } from "../connection/endpoint.js";
import { defaultReceiptPath, readReceipt, recordedStackTag, type Receipt } from "../install/receipt.js";
import { offersReconnect } from "./reconnect.js";
import { readClustersFileSafe } from "./file.js";
import type { ClusterConfig } from "./model.js";

/**
 * What the "+" is looking at.
 *
 * `absent` is the ONLY verdict that may offer an install. The other two both
 * mean "something is already here", and differ only in whether it answers --
 * which is the difference between offering a connection and offering a repair.
 */
export type PresenceVerdict =
  | "absent"
  | "installed-healthy"
  | "installed-unreachable"
  /**
   * A live k3d cluster named `memql` that THIS INSTALLER DID NOT CREATE
   * (memql#5118, D8).
   *
   * It is not `absent`, because something is here and installing over it
   * adopts its database. It is not `installed-*` either, because there is no
   * receipt: nothing knows what is on this machine, so repair and uninstall
   * have nothing to reverse. Its acts are adopt or delete, and the menu says
   * which cluster it is talking about.
   */
  | "present-unreceipted";

/** Which of the two independent sources said a local cluster exists. */
export interface PresenceEvidence {
  /** An install receipt on this machine records at least one executed step. */
  receipt: boolean;
  /** clusters.yaml carries an entry flagged `local: true`. */
  registry: boolean;
  /**
   * A k3d cluster named `memql` is running on this machine.
   *
   * ASKED ONLY WHEN THE OTHER TWO SAY NOTHING -- see property 2 above -- so it
   * is `false` on every path that already had evidence, and that `false` means
   * "not asked" rather than "not there". Nothing branches on it except
   * `verdictFor`, which reaches it only in that one case.
   */
  liveCluster: boolean;
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
  /**
   * The local cluster's recorded release: the install receipt's, else the
   * `version` clusters.yaml records for the registered entry. A cluster built
   * with `make up` has no receipt, and its version is still known.
   */
  version?: string;
}

/** What one probe learned: whether something answered, and why not when nothing did. */
export interface ProbeAnswer {
  answered: boolean;
  /** Why nothing answered: a timeout, a refused connection, a DNS failure. */
  reason?: string;
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
export type EndpointProbe = (endpoint: string, timeoutMs: number) => Promise<boolean | ProbeAnswer>;

/**
 * The dial deadline, per try.
 *
 * A local cluster on loopback answers in milliseconds from a shell, but the
 * FIRST TLS handshake from the editor's extension host can take far longer --
 * VS Code's proxy and certificate patching sit in front of it -- and the old
 * 1.5-second budget called a healthy cluster "not answering". Four seconds,
 * and a miss is tried once more (PROBE_TRIES) before it counts.
 */
export const PROBE_TIMEOUT_MS = 4_000;

/** A miss is tried this many times in all before the cluster is "not answering". */
export const PROBE_TRIES = 2;

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
export const DEFAULT_LOCAL_ENDPOINT = "api.memql.localhost:443";

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
    // AN ARTIFACT, not an entry (memql#3544). `receipt` on an entry names the
    // artifact CLASS that step left behind, and the read-only steps -- `detect`,
    // which only inspects the machine, and `providerFederation`, which verifies
    // a cluster's federated credential -- carry "" because they leave nothing.
    //
    // Counting entries therefore called a failed install an installed cluster:
    // a run that died at its first mutating step still recorded the read-only
    // ones ahead of it. That withdrew the Install card (offered for `absent` and
    // nothing else), so the operator could not retry the install that had just
    // failed; Repair re-ran the same graph into the same failure; and Uninstall
    // truthfully reported nothing to remove. A dead end, reached by the most
    // ordinary failure this wizard has.
    //
    // The test is the artifact rather than `changed`, because a step that FINDS
    // its artifact already present records `preExisting: true, changed: false`
    // -- it changed nothing and the thing is on the machine regardless, which is
    // exactly the case that must never be read as `absent`.
    return { present: receipt.entries.some((e) => e.receipt !== ""), receipt };
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
 *      one spelling of the `api.<domain>` convention (memql#3475), which
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

/**
 * The pure half: evidence plus reachability decide the verdict.
 *
 * THE RECEIPT AND THE REGISTRY OUTRANK THE LISTING, and the order is the
 * point. A cluster this installer DID create reads `installed-healthy` even
 * though k3d would also list it -- the receipt is what makes repair, rebuild
 * and uninstall mean anything, and demoting a receipted cluster to
 * `present-unreceipted` would take those acts away from the operator who has
 * them.
 */
export function verdictFor(evidence: PresenceEvidence, answered: boolean): PresenceVerdict {
  if (!evidence.receipt && !evidence.registry) {
    return evidence.liveCluster ? "present-unreceipted" : "absent";
  }
  return answered ? "installed-healthy" : "installed-unreachable";
}

/**
 * The k3d cluster name the install graph creates, and the only one this signal
 * recognises.
 *
 * A cluster by any other name is somebody else's project, and reading it as
 * MemQL's would withdraw Install from a machine that has never had one.
 */
export const LOCAL_CLUSTER_NAME = "memql";

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
  /** Told why the probe got no answer, after the last try (the MemQL Connection output). */
  onProbeFailure?: (endpoint: string, reason: string) => void;
  /**
   * The k3d cluster names on this machine, for the fourth signal.
   *
   * Injected rather than reached for, so this module keeps its property of
   * never requiring Docker in a test -- and so the ONE caller that supplies a
   * real implementation is the one that also knows where the scripts are.
   * Absent means the signal is not available, which reads exactly like an
   * empty list: Install stays offered.
   */
  listClusters?: () => Promise<string[]>;
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

  const evidence: PresenceEvidence = {
    receipt: fromReceipt.present,
    registry: local !== undefined,
    liveCluster: false,
  };
  if (!evidence.receipt && !evidence.registry) {
    // THE FOURTH SIGNAL, on the one path that would otherwise offer to install
    // over whatever is here. Never dialed, because there is no endpoint to
    // dial -- so this is the only round trip on this path, not a second one.
    //
    // A listing that throws answers the same as an empty one: k3d may not be
    // installed and Docker may be down, and neither is evidence of a cluster.
    // The direction that cannot destroy anything is the one to fail in.
    const names = opts.listClusters ? await opts.listClusters().catch(() => []) : [];
    evidence.liveCluster = names.some((n) => n.trim() === LOCAL_CLUSTER_NAME);
    return { verdict: verdictFor(evidence, false), evidence, endpoint: "" };
  }

  const endpoint = probeEndpointFor(local, fromReceipt.receipt);
  let answer: ProbeAnswer = { answered: false };
  for (let attempt = 0; attempt < PROBE_TRIES && !answer.answered; attempt += 1) {
    answer = await withDeadline(() => probe(endpoint, timeoutMs), timeoutMs);
  }
  if (!answer.answered) {
    opts.onProbeFailure?.(endpoint, answer.reason ?? `no answer within ${timeoutMs} ms, ${PROBE_TRIES} tries`);
  }
  const version = recordedStackTag(fromReceipt.receipt) || (local?.version ?? "").trim();
  return {
    verdict: verdictFor(evidence, answer.answered),
    evidence,
    endpoint,
    clusterName: local?.name,
    ...(version !== "" ? { version } : {}),
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
async function withDeadline(work: () => Promise<boolean | ProbeAnswer>, timeoutMs: number): Promise<ProbeAnswer> {
  let timer: NodeJS.Timeout | undefined;
  const expiry = new Promise<ProbeAnswer>((resolve) => {
    timer = setTimeout(() => resolve({ answered: false, reason: `no answer within ${timeoutMs} ms` }), timeoutMs);
  });
  const attempt = work().then(
    (result): ProbeAnswer => (typeof result === "boolean" ? { answered: result } : result),
    (err: unknown): ProbeAnswer => ({ answered: false, reason: err instanceof Error ? err.message : String(err) }),
  );
  try {
    return await Promise.race([attempt, expiry]);
  } catch {
    return { answered: false };
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
  new Promise<ProbeAnswer>((resolve) => {
    let url: string;
    try {
      url = webSocketUrlFor({ name: PROBE_CLUSTER_NAME, endpoint });
    } catch (err) {
      // An endpoint that cannot even be lifted to a URL answers nothing.
      resolve({ answered: false, reason: `not a dialable address (${err instanceof Error ? err.message : String(err)})` });
      return;
    }

    let settled = false;
    const socket = new NodeWebSocket(url, { handshakeTimeout: timeoutMs });
    const finish = (answer: ProbeAnswer): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        socket.terminate();
      } catch {
        // A socket that is already gone needs no tearing down.
      }
      resolve(answer);
    };
    // Cleared by finish(), which every exit path goes through.
    const timer = setTimeout(
      () => finish({ answered: false, reason: `${url} did not answer within ${timeoutMs} ms` }),
      timeoutMs,
    );

    socket.on("open", () => finish({ answered: true }));
    // ws only emits this when a listener exists; without one the HTTP response
    // arrives as a plain error and a live-but-unauthenticated server would read
    // as unreachable.
    socket.on("unexpected-response", () => finish({ answered: true }));
    socket.on("error", (err) =>
      finish(isCertificateFailure(err) ? { answered: true } : { answered: false, reason: probeErrorText(url, err) }),
    );
  });

/** A probe error in one line: the code when there is one, and the message. */
function probeErrorText(url: string, err: unknown): string {
  const e = err as { code?: unknown; message?: unknown };
  const code = typeof e?.code === "string" ? e.code : "";
  const message = typeof e?.message === "string" ? e.message : String(err);
  return code !== "" && !message.includes(code) ? `${url}: ${code} ${message}` : `${url}: ${message}`;
}

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
  | "reconnect"
  | "repair"
  | "uninstall"
  /** Register the cluster that is already here, without installing over it. */
  | "adopt";

export interface AddClusterChoice {
  action: AddClusterAction;
  label: string;
  detail: string;
}

/**
 * The card that consumes the verdict (memql#3741).
 *
 * Distinct from CONNECT, which is the REMOTE registration form. This one asks
 * nothing: the machine already knows the domain, either from the install
 * receipt or from the installer's own default, and the entry is composed from
 * it. See clusters/reconnect.ts.
 */
const RECONNECT: AddClusterChoice = {
  action: "reconnect",
  label: "Connect to the local cluster",
  detail: "It is already on this machine. MemQL uses what the install recorded -- nothing to type.",
};

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
 * What a live cluster with NO RECEIPT is offered (memql#5118, D8).
 *
 * NO INSTALL AND NO REPAIR. Installing would adopt this cluster's database
 * under a fresh receipt -- the exact failure the fourth signal exists to stop
 * -- and repair has nothing to reverse, because nothing recorded what is on
 * this machine. So there are two honest acts: use it as it is, or take it away
 * deliberately.
 */
const ADOPT: AddClusterChoice = {
  action: "adopt",
  label: "Connect to the cluster that is already here",
  detail:
    `A cluster named ${LOCAL_CLUSTER_NAME} exists that this installer did not create. ` +
    "Register it and use it as it is.",
};

/**
 * The delete, which is the uninstall form with its data box.
 *
 * It is the one path in the wizard that removes a cluster MemQL did not
 * create, and it asks for a typed phrase first -- which is why the label says
 * so rather than reading like the ordinary uninstall beside it.
 */
const DELETE_UNRECEIPTED: AddClusterChoice = {
  action: "uninstall",
  label: "Delete the cluster and its data...",
  detail:
    "Take it off this machine. You will be asked to type a phrase first, " +
    "because nothing here recorded what it holds.",
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
export function addClusterMenu(
  verdict: PresenceVerdict,
  registered: boolean,
): AddClusterChoice[] {
  const reconnect = offersReconnect(verdict, registered) ? [RECONNECT] : [];
  switch (verdict) {
    case "absent":
      return [
        {
          action: "install",
          label: "Install a local cluster",
          detail: "Recommended. Build a local MemQL cluster on this machine.",
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
      // Repair first: an operator looking at a cluster that will not answer
      // came to fix it. Reconnecting is what they do next, or instead.
      return [
        {
          action: "repair",
          label: "Repair the local cluster",
          detail: "A local cluster is installed but is not answering.",
        },
        ...reconnect,
        UNINSTALL,
        CONNECT,
      ];
    case "installed-healthy":
      // Reconnect FIRST when it is offered at all: a healthy cluster that is
      // not in the list is a cluster the operator removed the row for, and
      // putting it back is the only reason they opened this menu.
      return [...reconnect, CONNECT, UNINSTALL];
    case "present-unreceipted":
      // ADOPT FIRST, because it is almost always what they want: a developer
      // who ran `make up` before they ever opened this wizard has a working
      // cluster and came here to point the editor at it.
      return [ADOPT, CONNECT, DELETE_UNRECEIPTED];
  }
}
