import { renderMemQLValue, rowString, type QueryClient } from "@znasllc-io/memql-sdk-core/client";

import { repositoryPageFrom, type RepositoryPage } from "./repositories";

// The GitHub Connect calls, in one place (epic memql#4915).
//
// The same two rules `packages/calls.ts` states hold here. The generated
// typed builder is used wherever one exists -- it is the point of sdk-gen --
// and every value that is not a literal goes through `renderMemQLValue`,
// because a return path and a credential id are both text that reached this
// browser from somewhere else.
//
// NONE OF THESE HANDLE A TOKEN. `githubConnectBegin` answers a URL to
// navigate to; `sourceRepositories` answers what a grant can see; revoking
// names a row. The grant's own tokens are minted, sealed, refreshed and
// revoked server-side, and there is no argument or reply field here that
// could carry one.

/** What `githubConnectBegin` answers. */
export interface ConnectBegin {
  /** Where to send the browser. EMPTY when the call was refused. */
  authorizeUrl: string;
  /** `ok`, or the refusal code. */
  reason: string;
  /** The app's installation page. Empty when this cluster has no app. */
  installUrl: string;
}

/**
 * Start a connect flow: the cluster mints a state row bound to the caller and
 * answers the URL to navigate to.
 *
 * HAND-RENDERED, because `githubConnectBegin` has no generated builder in
 * this tree yet -- the engine half of this epic lands it. The text below is
 * exactly what that builder will render, so switching to
 * `query.githubConnectBegin({ returnPath })` is a one-line change with no
 * behaviour in it.
 *
 * `returnPath` is where the callback should send the browser back to, as a
 * PATH rather than a URL: the cluster composes the origin from its own
 * domain, so nothing this browser says can redirect somebody off-cluster.
 */
export async function githubConnectBegin(query: QueryClient, returnPath: string): Promise<ConnectBegin> {
  const result = await query.executeNamed(
    "githubConnectBegin",
    `builtin githubConnectBegin(returnPath: ${renderMemQLValue(returnPath)})`,
  );
  const row = result.rows()[0];
  return {
    authorizeUrl: row ? rowString(row, "authorizeUrl") : "",
    reason: row ? rowString(row, "reason") : "",
    installUrl: row ? rowString(row, "installUrl") : "",
  };
}

/**
 * One page of the repositories a grant can reach.
 *
 * Both arguments are sent ALWAYS, empty included. `credentialId: ""` is a
 * documented value rather than an omission -- it means "the grant I hold" --
 * and sending `page` on every call keeps one call shape on the wire instead
 * of two that differ by which optional happened to be set.
 */
export async function readSourceRepositories(
  query: QueryClient,
  credentialId: string,
  page: number,
): Promise<RepositoryPage> {
  const result = await query.sourceRepositories({ credentialId, page });
  return repositoryPageFrom(result.rows()[0]);
}

/**
 * Revoke a credential: a pasted token, or a grant.
 *
 * ONE call for both, because it is one act -- the engine flips the row to
 * revoked and, for a grant, revokes the authorization at GitHub as well. The
 * row is never deleted; it is the record of what fetched under it.
 *
 * IT ANSWERS WHETHER GITHUB WAS TOLD, and that is worth reading. The engine
 * revokes at GitHub FIRST and flips the row even when that half failed
 * (`handleSourceCredentialRevoke`) -- the local row is what actually stops
 * every fetch on this cluster, so refusing the disconnect because GitHub was
 * unreachable would leave the cluster fetching under an authorization the
 * person believes they ended. What the person then still has to do is undo it
 * at GitHub themselves, and only this field says so.
 *
 * FALSE IS THE ORDINARY ANSWER FOR A PASTED TOKEN and means nothing went
 * wrong: there is nothing at GitHub to revoke for a value somebody typed in.
 * Only the connected-account card reads it, and only for a grant.
 */
export async function revokeSourceCredential(query: QueryClient, credentialId: string): Promise<boolean> {
  const result = await query.sourceCredentialRevoke({ credentialId });
  const row = result.rows()[0];
  // READ AS TEXT, which is `packages/calls.ts`'s own reading of
  // `awaitingConfirm` and for the same reason: a scalar boolean crosses the
  // wire as the STRING "true" on a builtin's reply row, so the SDK's
  // `rowBool` -- which answers false for anything that is not a real boolean
  // -- would report every successful GitHub-side revoke as a failed one.
  return row ? rowString(row, "remoteRevoked") === "true" : false;
}
