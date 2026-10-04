// What this machine knows about a cluster's sign-in, without asking the cluster.
//
// WHY THIS EXISTS. The Clusters row and the cluster page used to decide "needs
// sign-in" from clusters.yaml alone (`needsAuth`). But the thirty-day refresh
// token lives in SecretStorage once a sign-in has happened, and the access
// token in the file is cleared on a terminal refresh -- or by the Cockpit
// rewriting the shared file. So a row said "needs sign-in" for a cluster a
// click would simply connect, and sent the person through a browser for
// nothing. These facts read every place a credential can be (the file and
// SecretStorage), once, asynchronously, for the synchronous renderers to use.
//
// THE PASSKEY OFFER is decided here too, because it rests on the same kind of
// evidence: the install receipt names this cluster's owner, and nobody has
// signed in to it from this machine yet (clusters/ownershipRoute.ts).
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

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
  /** "Create the owner passkey" may be offered (ownershipRoute.ts ownerSetupPending). */
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
  /** Epoch milliseconds. */
  now(): number;
}

/** The facts for one cluster. Never rejects: an unreadable source reads as "not there". */
export async function gatherClusterFacts(cluster: ClusterConfig, deps: ClusterFactDeps): Promise<ClusterFacts> {
  const secretRefresh = ((await deps.readRefreshToken(cluster.name).catch(() => undefined)) ?? "").trim();
  const receipt = cluster.local === true ? await deps.readReceipt().catch(() => null) : null;
  return factsFrom(cluster, { secretRefresh, receipt, signedInBefore: deps.signedInBefore(cluster), nowMs: deps.now() });
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
