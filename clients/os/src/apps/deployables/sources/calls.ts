import { renderMemQLValue, rowBool, rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

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
 * authorizes and revokes locally FIRST, then attempts the GitHub half
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

// ---------------------------------------------------------------------------
// The cluster's GitHub App (engine design record 2026-09-20-github-app-setup)
// ---------------------------------------------------------------------------
//
// Connect needs an app, and these three are how a surface finds out whether
// the cluster has one -- BEFORE anybody presses Connect -- and how a cluster
// owner gives it one. Hand-rendered for githubConnectBegin's reason.
//
// NONE OF THESE HANDLE A CREDENTIAL EITHER. The status answers a slug, which is
// public; beginning a setup answers a URL to navigate to; the app's six values
// are exchanged, sealed and stored server-side, and no reply carries one.

/** What `githubAppStatus` answers. */
export interface GithubAppStatus {
  configured: boolean;
  /** Where the app came from: the deployment's `environment`, a registration
   *  made from this product (`cluster`), or "" when there is none. */
  source: "" | "environment" | "cluster";
  /** The app's URL slug: the last segment of its own page on github.com. */
  slug: string;
  /** Where the app is installed on another account. Empty with no app. */
  installUrl: string;
  /** The caller is a cluster owner and the environment is not deciding. */
  canSetup: boolean;
}

// EITHER READING. A scalar boolean on a builtin's reply row has reached this
// client as the STRING "true" (`revokeSourceCredential` above, and
// `awaitingConfirm` in `packages/calls.ts`), and the SDK's `rowBool` answers
// false for a string. Accepting both costs a line, and the mistake it prevents
// is not cosmetic: `configured` misread as false would tell every cluster WITH
// an app that it has none, and offer its owner a second one.
const flag = (row: Row, key: string): boolean => rowString(row, key) === "true" || rowBool(row, key);

/**
 * Whether this cluster has a GitHub App. A read: it mints nothing.
 *
 * NULL WHEN THE CLUSTER ANSWERED NO ROW. The capability always answers one, so
 * no row is not "no app" -- it is no answer, and a caller reads that as "not
 * known" (`useGithubApp`). Reading it as "not configured" would take Connect
 * away from a cluster that has an app on the strength of an empty reply.
 */
export async function readGithubAppStatus(query: QueryClient): Promise<GithubAppStatus | null> {
  const result = await query.executeNamed("githubAppStatus", "builtin githubAppStatus()");
  const row = result.rows()[0];
  if (!row) return null;
  const source = rowString(row, "source");
  return {
    configured: flag(row, "configured"),
    source: source === "environment" || source === "cluster" ? source : "",
    slug: rowString(row, "slug"),
    installUrl: rowString(row, "installUrl"),
    canSetup: flag(row, "canSetup"),
  };
}

/** What `githubAppSetupBegin` answers. */
export interface GithubAppSetupBegin {
  /** The page that hands this cluster's manifest to GitHub. EMPTY when refused. */
  startUrl: string;
  /** `ok`, or the refusal code. */
  reason: string;
}

/**
 * Begin registering the cluster's GitHub App. Cluster owners only.
 *
 * `organization` is a GitHub organization's login, or "" for the account of
 * whoever is signed in to GitHub in this browser. Sent ALWAYS, empty included,
 * for `readSourceRepositories`' reason: one call shape on the wire.
 *
 * THE MANIFEST IS NOT AN ARGUMENT, and there is nowhere here to put one. The
 * cluster composes it from its own domain, so nothing this browser sends can
 * ask GitHub for a wider app than Connect uses.
 */
export async function githubAppSetupBegin(
  query: QueryClient,
  returnPath: string,
  organization: string,
): Promise<GithubAppSetupBegin> {
  const result = await query.executeNamed(
    "githubAppSetupBegin",
    `builtin githubAppSetupBegin(returnPath: ${renderMemQLValue(returnPath)}, organization: ${renderMemQLValue(organization)})`,
  );
  const row = result.rows()[0];
  return {
    startUrl: row ? rowString(row, "startUrl") : "",
    reason: row ? rowString(row, "reason") : "",
  };
}

/** Remove the app registered from this product. Answers the reason. */
export async function githubAppRemove(query: QueryClient): Promise<{ removed: boolean; reason: string }> {
  const result = await query.executeNamed("githubAppRemove", "builtin githubAppRemove()");
  const row = result.rows()[0];
  return {
    removed: row ? flag(row, "removed") : false,
    reason: row ? rowString(row, "reason") : "",
  };
}
