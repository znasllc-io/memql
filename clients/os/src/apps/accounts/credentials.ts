import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten } from "../../kit/rows";

// The credentials an operator has issued on behalf of one BILLING account,
// projected out of the wire rows `accountTokensForAccount` returns.
//
// PURE, and separate from the panel, for the reason rows.ts is: a projection
// asserted through render() is asserted through three layers that can each
// fail for unrelated reasons.
//
// ===========================================================================
// WHAT THIS CREDENTIAL IS -- AND THE THING THE VOCABULARY MUST NOT IMPLY
// ===========================================================================
// It is issued TO A USER ON BEHALF OF A BILLING ACCOUNT -- a
// `v1:identity:account`, the paying subject of the account-isolation model, and
// NOT the `v1:accounts:account` client registry this app also lists. The two
// share the word and nothing else. Its authenticated subject is the operator's
// own `v1:identity:user` row -- which is why the field below is called
// `subjectUserId` rather than `ownerUserId` or, worst of all, `accountUserId`.
// `accountId` is a BINDING, carried so a credential can be attributed to the
// work it was issued for and revoked as a group. Nothing authenticates as a
// billing account, and no field here should ever be read as though something
// did (component/grpc/account_token_handlers.go states the same rule at the
// other end of the wire, and the server echoes the subject back on the mint
// reply precisely so a browser cannot render otherwise).
//
// THE PROJECTION CARRIES NO SECRET, and that is a property of the shape
// rather than of this file: `accountTokenSummary` names the credential's
// non-secret leaves by path, so `keyHash` is simply never sent. The plaintext
// exists once, in the mint reply, and is never on a row at all.

/** The shape `accountTokenSummary` projects, in this app's words. */
export interface AccountTokenRow {
  /** The `v1:identity:identity` row id. What `revokeAccountToken` takes. */
  id: string;
  /**
   * The credential's authenticated SUBJECT -- the operator's user row, never
   * the account. `userId` on the wire; renamed here because `userId` beside an
   * `accountId` invites exactly the misreading this whole surface avoids.
   */
  subjectUserId: string;
  /** What the operator called it. The only handle a revoke has. */
  label: string;
  /** False once revoked. See `accountTokenIsRevoked` for the absent case. */
  active: boolean;
  /** The billing account this credential is bound to, for attribution. */
  accountId: string;
  /** Who minted it (`credentials.mintedBy`). */
  mintedBy: string;
  /** "" means no auto-expiry, which is a real answer rather than a gap. */
  expiresAt: string;
  /** "" means never used. Nothing admits one of these yet, so "" is normal. */
  lastUsedAt: string;
  createdAt: string;
}

export function accountTokenFromRow(row: Row): AccountTokenRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    subjectUserId: rowString(flat, "userId"),
    label: rowString(flat, "label"),
    // ABSENT READS AS ACTIVE, and the direction is chosen rather than
    // inherited. `active` decides whether a Revoke control is offered at all
    // (DESIGN.md rule 12: an act that is not legal is ABSENT), so the two
    // wrong answers are not symmetric. Reading an unreadable value as
    // "revoked" hides the only control that can stop a LIVE credential;
    // reading it as "active" offers a revoke on something already revoked,
    // which the server answers idempotently. The recoverable mistake wins.
    active: boolOr(flat, "active", true),
    accountId: rowString(flat, "accountId"),
    mintedBy: rowString(flat, "mintedBy"),
    expiresAt: rowString(flat, "expiresAt"),
    lastUsedAt: rowString(flat, "lastUsedAt"),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function accountTokenIsRevoked(token: AccountTokenRow): boolean {
  return !token.active;
}

/**
 * The name to show, never blank.
 *
 * The server refuses an unlabelled mint precisely so a list of credentials is
 * not a list of blanks -- "an unlabelled credential cannot be revoked with
 * confidence" is its own sentence for it. This is the belt to that braces: a
 * row written before that rule, or one whose label failed to project, still
 * has to be distinguishable from the one below it, and the id's tail is the
 * one thing every row has.
 */
export function accountTokenLabel(token: AccountTokenRow): string {
  const label = token.label.trim();
  if (label !== "") return label;
  const tail = token.id.split(":").pop() ?? "";
  return tail === "" ? "Unlabelled credential" : `Unlabelled credential (${tail})`;
}

/**
 * Newest first, without leaning on the server's sort.
 *
 * `accountTokensForAccount` already sorts on `row.createdAt` descending, so
 * this agrees with it rather than reordering it. It exists because the list
 * is re-read after every mint and revoke: a fold that put a new credential
 * anywhere but the top would have somebody hunting for the row they just
 * created, and a client-side order is the only one this surface can promise
 * across those re-reads.
 */
export function sortAccountTokens(tokens: AccountTokenRow[]): AccountTokenRow[] {
  return [...tokens].sort((a, b) => b.createdAt.localeCompare(a.createdAt));
}
