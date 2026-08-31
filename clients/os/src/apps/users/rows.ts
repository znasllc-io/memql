import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// The wire rows the Users app renders, projected into the shapes its surfaces
// read.
//
// PURE, and separate from every component, for the reason apps/fleet/rows.ts
// is: a projection asserted through render() is asserted through three layers
// that can each fail for unrelated reasons. Everything here is a function of a
// row and is unit-testable with no browser, no cluster and no React.

/**
 * Unwrap a `payload`-nested row to the flat form the field helpers read.
 *
 * A row reaches these functions from two places -- the SEED (a named query's
 * result, already shape-flattened) and the SUBSCRIPTION fold (a CDC envelope,
 * which carries the concept fields inside `payload`) -- and the two paths have
 * to produce the same object. Otherwise a person renders one way on load and
 * another way the moment anything about them changes.
 *
 * The envelope wins on a collision so `id` stays the ROW's id rather than any
 * `id` the payload happens to carry.
 */
export function flatten(row: Row): Row {
  const nested = row["payload"];
  if (nested && typeof nested === "object" && !Array.isArray(nested)) {
    return { ...(nested as Row), ...row };
  }
  return row;
}

/**
 * A boolean field that can be ABSENT, with the default the concept declares.
 *
 * The SDK's `rowBool` returns `false` for a missing key, which collapses
 * "absent" and "explicitly false" into one answer. That is fine for a field
 * whose default is false, and wrong for every field here: `user.active` and
 * `invitation.active` both default to TRUE, and a folded CDC event carries
 * only what the write touched -- so reading absent as false makes everybody
 * vanish from the list the first time anything about them changes.
 */
function boolOr(row: Row, key: string, fallback: boolean): boolean {
  const v = row[key];
  return typeof v === "boolean" ? v : fallback;
}

// ---------------------------------------------------------------------------
// A person
// ---------------------------------------------------------------------------

/** `any` | `passkey_only`, or "" on a row written before the field existed. */
export type SignInPolicy = "" | "any" | "passkey_only";

export interface PersonRow {
  id: string;
  displayName: string;
  firstName: string;
  lastName: string;
  primaryEmail: string;
  /** The cluster-wide role: owner | admin | developer | writer | reader. */
  role: string;
  signInPolicy: SignInPolicy;
  /**
   * A mailbox several people read (memql#4477 territory). It is a quiet hint
   * rather than a column: it changes what a sign-in LINK means for this
   * address, which matters when reading the policy beside it, and it is true
   * of very few rows.
   */
  sharedMailbox: boolean;
  active: boolean;
  suspendedAt: string;
  suspendedReason: string;
  /**
   * Liveness. NEVER put this in a LiveList fingerprint: the engine churns it,
   * so naming it turns the whole list into a strobe on a timer -- the standing
   * badge the arrival cue exists not to be (clients/os/README.md).
   */
  lastSeenAt: string;
  createdAt: string;
}

export function personFromRow(raw: Row): PersonRow {
  const row = flatten(raw);
  const policy = rowString(row, "signInPolicy");
  return {
    id: rowString(row, "id"),
    displayName: rowString(row, "displayName"),
    firstName: rowString(row, "firstName"),
    lastName: rowString(row, "lastName"),
    primaryEmail: rowString(row, "primaryEmail"),
    role: rowString(row, "role"),
    signInPolicy: policy === "any" || policy === "passkey_only" ? policy : "",
    sharedMailbox: boolOr(row, "sharedMailbox", false),
    // ABSENT MEANS ACTIVE -- see boolOr.
    active: boolOr(row, "active", true),
    suspendedAt: rowString(row, "suspendedAt"),
    suspendedReason: rowString(row, "suspendedReason"),
    lastSeenAt: rowString(row, "lastSeenAt"),
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * What to call somebody.
 *
 * `displayName` when the directory has one, then the name parts, then the
 * email, and only then the id. NEVER blank: a nameless row is
 * indistinguishable from a row that failed to render, and every person on this
 * list has at least an address.
 */
export function personName(person: PersonRow): string {
  const display = person.displayName.trim();
  if (display !== "") return display;
  const parts = `${person.firstName} ${person.lastName}`.trim();
  if (parts !== "") return parts;
  if (person.primaryEmail.trim() !== "") return person.primaryEmail;
  return person.id;
}

/** Deactivated or suspended -- the two ways an account stops being current. */
export function personIsDim(person: PersonRow): boolean {
  return !person.active || person.suspendedAt !== "";
}

// ---------------------------------------------------------------------------
// An invitation
// ---------------------------------------------------------------------------

export type DeliveryState = "not_attempted" | "sent" | "failed";

export interface InvitationRow {
  id: string;
  kind: string;
  status: string;
  active: boolean;
  inviteeEmail: string;
  inviteeName: string;
  inviteeRole: string;
  inviterName: string;
  expiresAt: string;
  respondedAt: string;
  /**
   * Read WITH `deliveryError`, never alone (memql#4587). `not_attempted` with
   * no error is a CONFIGURATION statement -- no mail is wired on this cluster,
   * so the link is the only delivery mechanism -- while `failed` with an error
   * is an incident. Rendering both as "not sent" is what let an invitation
   * look delivered when nothing had been sent at all.
   */
  deliveryState: DeliveryState;
  deliveryError: string;
  createdAt: string;
}

export function invitationFromRow(raw: Row): InvitationRow {
  const row = flatten(raw);
  const delivery = rowString(row, "deliveryState");
  return {
    id: rowString(row, "id"),
    // ABSENT MEANS "user" HERE AND ONLY HERE. The concept defaults `kind` to
    // "guest", but every row this app sees came through
    // pendingUserInvitations, whose filter is `kind=="user"`, and a folded
    // event that omits the field must not read as a guest invitation and get
    // dropped by our own scope check.
    kind: rowString(row, "kind") || "user",
    // Same reasoning: the concept defaults `status` to "pending".
    status: rowString(row, "status") || "pending",
    active: boolOr(row, "active", true),
    inviteeEmail: rowString(row, "inviteeEmail"),
    inviteeName: rowString(row, "inviteeName"),
    inviteeRole: rowString(row, "inviteeRole"),
    inviterName: rowString(row, "inviterName"),
    expiresAt: rowString(row, "expiresAt"),
    respondedAt: rowString(row, "respondedAt"),
    deliveryState:
      delivery === "sent" || delivery === "failed" ? delivery : "not_attempted",
    deliveryError: rowString(row, "deliveryError"),
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * Still outstanding.
 *
 * The READ says this server-side (`pendingUserInvitations` filters on
 * `statusIsPending && isActiveRecord`); this says the same thing about an
 * ARRIVING event, which is what stops an acceptance folding straight back in
 * as an update. The two have to agree, or a row the read excludes reappears
 * live and then vanishes on the next reseed.
 */
export function invitationIsPending(invite: InvitationRow): boolean {
  return invite.kind === "user" && invite.active && invite.status === "pending";
}

/** Past its expiry against the supplied clock. Blank expiry never expires. */
export function invitationHasExpired(invite: InvitationRow, now: Date): boolean {
  if (invite.expiresAt === "") return false;
  const at = Date.parse(invite.expiresAt);
  return Number.isFinite(at) && at <= now.getTime();
}
