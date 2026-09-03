import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten, stringsOf } from "../../../kit/rows";

// A personal source credential, as the OS reads it (epic memql#4885, design
// section D; widened for the GRANT by memql#4915, design section C).
//
// ===========================================================================
// THE CARD, NEVER THE VALUE
// ===========================================================================
// `v1:platform:sourceCredential` holds a token the engine reads at fetch
// time. What reaches a browser is the CARD -- a label somebody chose, the
// host it is for, the fingerprint of the value, and whether it still stands.
// The projection below has no field for the token, so a row that carried one
// would be dropped here rather than rendered: there is no chip, fact or
// tooltip in this app that could show a value, because there is no type that
// could hold one.
//
// THAT HOLDS UNCHANGED FOR A GRANT, and the three fields added for one were
// chosen against it: `kind`, `login` and `installationIds` say WHOSE
// connection this is and how far it reaches. The grant's user token, its
// refresh token and its expiry are none of this app's business -- they are
// sealed, refreshed server-side on use, and deliberately outside the card
// shape, so `externalId` and `expiresAt` are absent here even though the
// concept carries them.
//
// Everything here is pure -- a row in, a card out -- so the chip on the
// Source stop, the connected-account card and the Sources settings group are
// checked against the same answers.

/** The credentials themselves (`dsl/platform/concepts.memql`). */
export const SOURCE_CREDENTIAL_CONCEPT = "v1:platform:sourceCredential";

/**
 * The kind a credential is (epic memql#4915, design section C).
 *
 * `token` is a value somebody pasted; `github_app` is an authorization GRANT
 * the person made at GitHub, which this cluster can renew and revoke on their
 * behalf. Not a closed union on the row, deliberately -- see
 * `credentialFromRow`.
 */
export const GITHUB_APP_KIND = "github_app";
export const TOKEN_KIND = "token";

export interface CredentialRow {
  id: string;
  ownerUserId: string;
  /** The host the token is for; `github.com` today. */
  host: string;
  /** What the person called it. */
  label: string;
  /** A digest of the value, so two cards can be told apart without either value. */
  fingerprint: string;
  /** `active` | `revoked`. */
  status: string;
  /**
   * `token` (a pasted value) or `github_app` (a grant), and NEVER absent
   * once projected -- see `credentialFromRow` for why the default is
   * `token` and why an unrecognised value is kept verbatim.
   */
  kind: string;
  /** The GitHub login a grant was made by. Empty for a pasted token. */
  login: string;
  /** The installations a grant reaches. Empty for a pasted token. */
  installationIds: readonly string[];
  /** A heartbeat: written by every fetch under this credential. Displayed, never fingerprinted. */
  lastUsedAt: string;
  revokedAt: string;
  createdAt: string;
}

/**
 * The card, from a seed row or a folded subscription envelope.
 *
 * ABSENT `kind` READS AS `token`, AND THAT IS THE WHOLE OF THE DEFAULT.
 * Every credential written before this epic carries no `kind` at all, and a
 * projection that let the empty string through would give this app three
 * states where the model has two -- so the Sources group would list a
 * person's own pasted tokens under neither heading and simply not draw them.
 *
 * An UNRECOGNISED value is kept VERBATIM rather than folded into either
 * state. `isGithubAppGrant` tests for the grant exactly, so a kind from a
 * newer engine reads as "not a grant" everywhere -- the reading that asserts
 * least -- while normalising it to `token` here would have this build claim
 * a fact about a credential it does not understand.
 */
export function credentialFromRow(raw: Row): CredentialRow {
  const row = flatten(raw);
  const kind = rowString(row, "kind");
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    host: rowString(row, "host"),
    label: rowString(row, "label"),
    fingerprint: rowString(row, "fingerprint"),
    status: rowString(row, "status"),
    kind: kind === "" ? TOKEN_KIND : kind,
    login: rowString(row, "login"),
    installationIds: stringsOf(row, "installationIds"),
    lastUsedAt: rowString(row, "lastUsedAt"),
    revokedAt: rowString(row, "revokedAt"),
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * What counts as NEWS on a credential, for the arrival cue.
 *
 * A HEARTBEAT IS NOT NEWS (clients/os/README.md, the arrival-cue rule).
 * `lastUsedAt` is written by every fetch of every source that uses this
 * credential -- the ten-minute poll feed included -- so naming it here would
 * ring the card on a timer for as long as anything tracks a repository under
 * it: the standing badge the cue exists not to be. `revokedAt` is left out
 * for the same reason `status` is in: the flip to `revoked` is the change a
 * person would name, and the timestamp beside it only says when.
 *
 * A rename, a revocation, a different host and a rotated value are what a
 * person would call a change on a credential. Those four -- and, since epic
 * memql#4915, the three a GRANT adds:
 *
 *   * `installationIds` MOVING IS NEWS, and it is the one an installation
 *     webhook produces: somebody installed the app on another organisation,
 *     or removed it from one, and the connected-account card exists to show
 *     exactly that. It is the SET that counts, so the members are sorted
 *     before they are joined -- GitHub answers no ordering guarantee, and a
 *     re-read that returned the same installations in a different order
 *     would ring a card nothing had happened to.
 *   * `kind` and `login` are what a reconnect can change: a grant remade
 *     under a different GitHub account is a different connection wearing the
 *     same row id, and a card that did not announce that would be naming
 *     somebody else's login on a page a person had already read.
 *
 * `expiresAt` is deliberately not here and is not even projected: the user
 * token is refreshed server-side on use, so it moves on its own exactly as
 * `lastUsedAt` does. Nothing timer-driven, in either half.
 */
export function credentialFingerprint(c: CredentialRow): string {
  const reaches = [...c.installationIds].sort().join(",");
  return `${c.label}|${c.status}|${c.host}|${c.fingerprint}|${c.kind}|${c.login}|${reaches}`;
}

/**
 * Whether this credential is a GitHub App GRANT rather than a pasted value.
 *
 * EXACT equality, and the one place this build decides the question. A grant
 * has no label a person chose and no fingerprint worth reading -- it has a
 * login and a set of installations -- so every surface that renders a
 * credential asks this first and renders one of two entirely different
 * things.
 */
export function isGithubAppGrant(c: Pick<CredentialRow, "kind">): boolean {
  return c.kind === GITHUB_APP_KIND;
}

/**
 * The grant this person holds, or null.
 *
 * ONE grant per person, which is a property of the write path rather than of
 * this function: the callback writes or UPDATES a single row keyed on the
 * GitHub user id (design section C), so a second row here would mean a
 * reconnect under a different GitHub account. When that happens the ACTIVE
 * one is the connection -- a revoked grant is a card in the list of things
 * that no longer work, never the account this person is connected as.
 */
export function githubGrantOf(credentials: readonly CredentialRow[]): CredentialRow | null {
  const grants = credentials.filter(isGithubAppGrant);
  return grants.find((c) => !credentialIsRevoked(c)) ?? grants[0] ?? null;
}

/** The credentials a person PASTED: the fallback path's list, which is every
 *  card that is not a grant. Ordered newest first, because the one somebody
 *  just added is the one they came here to see. */
export function pastedCredentials(credentials: readonly CredentialRow[]): CredentialRow[] {
  return credentials
    .filter((c) => !isGithubAppGrant(c))
    .slice()
    .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
}

/** Whether the card still stands. Anything but `revoked` reads as usable,
 *  because a value this build has never seen is not evidence of revocation. */
export function credentialIsRevoked(c: Pick<CredentialRow, "status">): boolean {
  return c.status === "revoked";
}
