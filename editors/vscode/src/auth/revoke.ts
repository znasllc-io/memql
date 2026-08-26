// Ending a session on the CLUSTER, not only in this editor (memql#4625).
//
// -----------------------------------------------------------------------------
// WHAT WAS WRONG
// -----------------------------------------------------------------------------
//
// Sign-out was local only. `signOut` cleared SecretStorage and blanked the file
// keys, the connection was dropped, and a toast said `signed out of "x"` --
// which reads, to anybody, as "the session is over". It was not. The refresh
// token stayed live on the cluster for its full 30 days
// (component/identity/config.go), redeemable by anyone who had obtained a copy.
//
// A sign-out that leaves a 30-day credential live is the one case where the
// reassuring message is the dangerous part: a user who believed they had
// revoked access takes no further action.
//
// -----------------------------------------------------------------------------
// WHY BEST-EFFORT, AND WHY THAT IS NOT A COP-OUT
// -----------------------------------------------------------------------------
//
// The local clear MUST happen whether or not the cluster can be reached --
// refusing to sign out of an unreachable cluster would leave the credential in
// SecretStorage as well as on the server, which is strictly worse. So the call
// is attempted first, and its outcome changes only what the user is TOLD.
//
// That is the whole design: the operation is unchanged, and the honesty of the
// report is what this adds. `describeRevocation` returns the sentence, and when
// revocation did not happen it names the portal's Devices page, which is where
// a session can be ended without this extension's help.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { FetchLike } from "../connection/credentials.js";

/** The identity route that revokes a refresh token. HTTP by necessity: it is
 *  the same endpoint a browser form posts (docs: Allowed HTTP Exceptions). */
export const LOGOUT_PATH = "/auth/logout";

/** How long to wait before giving up and clearing locally anyway. Short on
 *  purpose -- sign-out is a foreground action, and a user who has decided to
 *  leave should not watch a spinner because a cluster is down. */
export const REVOKE_TIMEOUT_MS = 5_000;

export type RevocationOutcome =
  /** The cluster confirmed the session is revoked. */
  | { kind: "revoked" }
  /** There was nothing to revoke -- no refresh token was held. */
  | { kind: "nothingToRevoke" }
  /** No issuer is known for this cluster, so there is nowhere to POST. */
  | { kind: "noIssuer" }
  /** The cluster was reached and refused, or could not be reached at all. */
  | { kind: "failed"; reason: string };

export interface RevokeRequest {
  /** The identity service base URL, e.g. `https://identity.example.com`. */
  issuer: string | undefined;
  /** The refresh token to revoke. `""`/undefined means there is nothing to do. */
  refreshToken: string | undefined;
  fetch: FetchLike;
}

/**
 * revokeRefreshToken ends the session on the cluster.
 *
 * NEVER THROWS. Every failure is a value, because the caller's next step --
 * clear the local credentials -- is unconditional, and an exception here would
 * put that step behind a try/catch somebody could later get wrong.
 */
export async function revokeRefreshToken(req: RevokeRequest): Promise<RevocationOutcome> {
  const refreshToken = (req.refreshToken ?? "").trim();
  if (refreshToken === "") return { kind: "nothingToRevoke" };

  const issuer = (req.issuer ?? "").trim().replace(/\/+$/, "");
  if (issuer === "") return { kind: "noIssuer" };

  try {
    const response = await req.fetch(`${issuer}${LOGOUT_PATH}`, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json" },
      // The JSON body form, which extractRefreshToken accepts alongside the
      // cookie and the Authorization header (component/identity/http/refresh.go).
      // The cookie is a browser's spelling and this is not a browser.
      body: JSON.stringify({ refresh_token: refreshToken }),
    });
    // 204 is the documented answer. The handler is IDEMPOTENT -- an unknown or
    // already-revoked token also answers 204 -- so any 2xx means the session is
    // not live, which is the question being asked.
    if (response.ok) return { kind: "revoked" };
    return { kind: "failed", reason: `the cluster answered ${response.status}` };
  } catch (err) {
    return { kind: "failed", reason: errorText(err) };
  }
}

/**
 * describeRevocation is the sentence the user sees, and the point of the whole
 * change: `signed out of "x"` was true about this editor and false about the
 * cluster, and nothing distinguished the two.
 */
export function describeRevocation(clusterName: string, outcome: RevocationOutcome): string {
  switch (outcome.kind) {
    case "revoked":
      return `Signed out of "${clusterName}". The session was revoked on the cluster.`;
    case "nothingToRevoke":
      return `Signed out of "${clusterName}".`;
    case "noIssuer":
      return (
        `Signed out of "${clusterName}" HERE ONLY. No identity service is known for this ` +
        `cluster, so its session could not be revoked; it stays valid until it expires. ` +
        `End it from the portal's Devices page.`
      );
    case "failed":
      return (
        `Signed out of "${clusterName}" HERE ONLY -- the session could not be revoked on the ` +
        `cluster (${outcome.reason}), so it stays valid until it expires. End it from the ` +
        `portal's Devices page.`
      );
  }
}

/** True when the user should be shown a warning rather than a status message. */
export function revocationNeedsAttention(outcome: RevocationOutcome): boolean {
  return outcome.kind === "noIssuer" || outcome.kind === "failed";
}

function errorText(err: unknown): string {
  if (err instanceof Error && err.message.trim() !== "") return err.message;
  const text = String(err ?? "").trim();
  return text === "" ? "unknown error" : text;
}
