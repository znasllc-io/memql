// Sign-out that reaches the cluster (znasllc-io/memql#4625).
//
// WHAT WAS WRONG. Signing out was purely local: SecretStorage was cleared, the
// file keys were blanked, the connection was dropped, and a toast said
// `signed out of "x"`. Nothing ever told the cluster. The refresh token stayed
// live for its full thirty days (component/identity/config.go), so anyone
// holding a copy -- a synced settings file, a backup, a shared machine, a
// laptop being handed on -- could still mint access tokens against an account
// whose owner had been told the session was over.
//
// The toast is the part that makes it a defect rather than a gap. "Signed out"
// is a claim about the SESSION, and it was only ever true of this editor.
//
// BEST EFFORT, AND THE LOCAL CLEAR IS UNCONDITIONAL. A person signing out on a
// plane, behind a captive portal, or against a cluster that is down must still
// end up signed out HERE -- refusing to forget the credential because the
// server could not be reached would leave it on disk, which is worse in every
// direction. So revocation is attempted first, its outcome is carried back,
// and the wording changes rather than the behaviour.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// the decision -- what to POST, what counts as revoked, what to say when it did
// not -- is the part worth testing, and the command supplies the fetch.

/** The subset of `fetch` this module needs; the same shape credentials.ts uses. */
export type RevokeResponseLike = { ok: boolean; status: number };
export type RevokeFetch = (
  url: string,
  init: { method: string; headers: Record<string, string>; body: string },
) => Promise<RevokeResponseLike>;

/**
 * What became of the attempt.
 *
 * `attempted: false` is NOT a failure -- it is "there was nothing to revoke, or
 * nowhere to send it", which is the ordinary case for a cluster signed in with
 * a bare token and no refresh credential. It gets its own value so the caller
 * can stay quiet about it: telling somebody their session "was only forgotten
 * locally" when no server-side session ever existed would be alarming and
 * false.
 */
export type RevocationOutcome =
  | { attempted: true; revoked: true }
  | { attempted: true; revoked: false; reason: string }
  | { attempted: false };

/**
 * Revoke one refresh token at its issuer.
 *
 * `POST <issuer>/auth/logout` with `{refresh_token}`, which is the endpoint's
 * documented JSON carrier (component/identity/http/refresh.go's
 * extractRefreshToken). The handler is idempotent by design: an unknown or
 * already-revoked token answers 204 exactly as a live one does, so a second
 * sign-out is not an error and a stale token in a file does not produce a
 * frightening message.
 *
 * ANY 2xx COUNTS. The endpoint answers 204, but a proxy that rewrites it to
 * 200 has not failed to revoke anything, and treating that as a failure would
 * tell an operator their session survived when it did not.
 */
export async function revokeRefreshToken(
  issuerBaseUrl: string,
  refreshToken: string,
  fetchImpl: RevokeFetch,
): Promise<RevocationOutcome> {
  const base = issuerBaseUrl.trim().replace(/\/+$/, "");
  const token = refreshToken.trim();
  if (base === "" || token === "") return { attempted: false };

  const url = `${base}/auth/logout`;
  let response: RevokeResponseLike;
  try {
    response = await fetchImpl(url, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: JSON.stringify({ refresh_token: token }),
    });
  } catch (err) {
    return {
      attempted: true,
      revoked: false,
      // The message is shown to a person deciding whether to go and revoke the
      // session by hand, so it names the host that could not be reached.
      reason: `${base} could not be reached (${errorText(err)})`,
    };
  }

  if (!response.ok) {
    return { attempted: true, revoked: false, reason: `${url} returned ${response.status}` };
  }
  return { attempted: true, revoked: true };
}

/**
 * What to tell the person, given the outcome.
 *
 * Returned as a sentence rather than shown here, for the module's no-`vscode`
 * rule -- and because the caller composes it with the cluster's name.
 *
 * The unrevoked wording names the portal's Devices page, which is where a
 * session can actually be ended when this could not do it. A message that says
 * only "revocation failed" leaves the person with a live credential and no
 * next step.
 */
export function signOutMessage(clusterName: string, outcome: RevocationOutcome): string {
  const quoted = `"${clusterName}"`;
  if (outcome.attempted && outcome.revoked) {
    return `MemQL: signed out of ${quoted} and ended the session on the cluster.`;
  }
  if (outcome.attempted && !outcome.revoked) {
    return (
      `MemQL: forgot the credentials for ${quoted} on this machine, but could not end the ` +
      `session on the cluster (${outcome.reason}). The refresh token stays valid until it ` +
      `expires -- end it from the portal's Devices page if that matters.`
    );
  }
  return `MemQL: signed out of ${quoted}. Run "MemQL: Sign In" to authenticate again.`;
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
