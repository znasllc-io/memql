import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";

// A personal source credential, as the OS reads it (epic memql#4885, design
// section D).
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
// Everything here is pure -- a row in, a card out -- so the chip on the
// Source stop and the Sources settings group are checked against the same
// answers.

/** The credentials themselves (`dsl/platform/concepts.memql`). */
export const SOURCE_CREDENTIAL_CONCEPT = "v1:platform:sourceCredential";

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
  /** A heartbeat: written by every fetch under this credential. Displayed, never fingerprinted. */
  lastUsedAt: string;
  revokedAt: string;
  createdAt: string;
}

export function credentialFromRow(raw: Row): CredentialRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    host: rowString(row, "host"),
    label: rowString(row, "label"),
    fingerprint: rowString(row, "fingerprint"),
    status: rowString(row, "status"),
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
 * person would call a change on a credential. Those four, and nothing that
 * moves on its own.
 */
export function credentialFingerprint(c: CredentialRow): string {
  return `${c.label}|${c.status}|${c.host}|${c.fingerprint}`;
}

/** Whether the card still stands. Anything but `revoked` reads as usable,
 *  because a value this build has never seen is not evidence of revocation. */
export function credentialIsRevoked(c: Pick<CredentialRow, "status">): boolean {
  return c.status === "revoked";
}
