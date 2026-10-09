// Combine stored credentials and the install receipt with current owner/passkey state.
// The server wins after a reinstall, even if this editor remembers an older session.
import { classifyToken, jwtExpirySeconds } from "../connection/credentials.js";
import { recordedDomain, recordedOwner, type Receipt } from "../install/receipt.js";
import { composeConsoleUrl } from "./consoleUrl.js";
import type { ClusterConfig } from "./model.js";
import { ownerSetupPending, receiptNamesAnotherCluster } from "./ownershipRoute.js";

export interface ClusterFacts {
  /**
   * A connect could authenticate without a new sign-in, as far as this machine
   * can tell: a refresh token is stored (SecretStorage or the file), or the
   * stored access token has not expired. Only a server can say it is still
   * accepted; this says there is something worth presenting.
   */
  session: boolean;
  /** A credential of any kind is stored here, so Sign out has something to end. */
  signedIn: boolean;
  /** The first owner passkey is required before the cluster can be used. */
  ownerSetup: boolean;
  /** MemQL OS, composed from the domain; "" when nothing names it. */
  consoleUrl: string;
}

/**
 * What clusters.yaml alone says, for a caller with no SecretStorage or receipt
 * to hand. It can under-report a session (a refresh token in SecretStorage is
 * invisible to it), never over-report one.
 */
export function fileOnlyFacts(cluster: ClusterConfig, nowMs: number = Date.now()): ClusterFacts {
  return factsFrom(cluster, { secretRefresh: "", receipt: null, signedInBefore: false, nowMs });
}

export interface ClusterFactDeps {
  /** SecretStorage's refresh token for a cluster (ClusterCredentialStore.readRefreshToken). */
  readRefreshToken(clusterName: string): Promise<string | undefined>;
  /** The install receipt, or null when there is none or it cannot be read. */
  readReceipt(): Promise<Receipt | null>;
  /** Whether a sign-in to this cluster has completed on this machine before. */
  signedInBefore(cluster: ClusterConfig): boolean;
  /** Server state overrides receipt/history, including after a reinstall. */
  ownerState?(cluster: ClusterConfig): Promise<import("./claimState.js").ClaimState>;
  /** Epoch milliseconds. */
  now(): number;
}

/** The facts for one cluster. Never rejects: an unreadable source reads as "not there". */
export async function gatherClusterFacts(cluster: ClusterConfig, deps: ClusterFactDeps): Promise<ClusterFacts> {
  const secretRefresh = ((await deps.readRefreshToken(cluster.name).catch(() => undefined)) ?? "").trim();
  const receipt = cluster.local === true ? await deps.readReceipt().catch(() => null) : null;
  const facts = factsFrom(cluster, { secretRefresh, receipt, signedInBefore: deps.signedInBefore(cluster), nowMs: deps.now() });
  const state = await deps.ownerState?.(cluster).catch(() => "unknown");
  if (state === "unclaimed") return { ...facts, ownerSetup: true, session: false };
  if (state === "claimed") return { ...facts, ownerSetup: false };
  return facts;
}

/** The pure half, for tests and for a caller that already holds the inputs. */
export function factsFrom(
  cluster: ClusterConfig,
  input: { secretRefresh: string; receipt: Receipt | null; signedInBefore: boolean; nowMs: number },
): ClusterFacts {
  const token = (cluster.token ?? "").trim();
  const fileRefresh = (cluster.refreshToken ?? "").trim();
  const tokenClass = classifyToken(token);
  const usableToken = tokenClass === "jwt" || tokenClass === "opaque";
  const expiry = usableToken ? jwtExpirySeconds(token) : undefined;
  const tokenLive = usableToken && (expiry === undefined || expiry * 1000 > input.nowMs);
  const refreshable = input.secretRefresh !== "" || fileRefresh !== "";
  const signedIn = token !== "" || refreshable;

  const owner = recordedOwner(input.receipt).email;
  const ownerRecorded =
    owner !== "" && !receiptNamesAnotherCluster(cluster.domain ?? "", recordedDomain(input.receipt));

  return {
    // A PAT or worker token is refused by class before any dial, so it is
    // never a session, whatever else is stored beside it.
    session: tokenClass !== "pat" && tokenClass !== "workerToken" && (refreshable || tokenLive),
    signedIn,
    ownerSetup: ownerSetupPending({
      local: cluster.local === true,
      ownerRecorded,
      enrolled: signedIn || input.signedInBefore,
    }),
    consoleUrl: composeConsoleUrl(cluster),
  };
}

/** The key a completed sign-in is remembered under: the slot and the domain it named. */
export function signedInKey(cluster: ClusterConfig): string {
  return `${cluster.name}|${(cluster.domain ?? "").trim().toLowerCase()}`;
}
