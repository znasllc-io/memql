// Offering passkey enrolment to an operator who has just signed in.
//
// The second half of memql#3885's three-state table (memql#3902):
//
//   installed, owner never signed in  -> CLAIM it        (memql#3885, done)
//   claimed, no passkey registered    -> ENROL a passkey (here)
//   claimed, passkey registered       -> sign in         (correct already)
//
// WHY THIS HALF COULD NOT LAND WITH THE FIRST. The claim state is derivable
// with NO credential: `GET <issuer>/setup` answers 200 (unclaimed) or 404
// (sealed), because the wizard mints the first owner and so must be reachable
// before anyone holds anything.
//
// Passkey state is not, and MUST NOT BE. There is deliberately no
// unauthenticated way to ask whether an account has a passkey -- that is an
// enumeration oracle, and it would answer for accounts the asker does not own.
// So this half runs AFTER a successful sign-in, over the authenticated stream,
// and both of its calls are self-scoped or owner-gated server-side:
//
//   passkeysForSelf    no userId argument BY DESIGN -- the row set comes from
//                      userId==actor.userId, so it cannot be pointed at a
//                      stranger's authenticators (memql#3178, memql#3409).
//   issueEnrolmentLink owner/admin, gated in component/identity/adminops, and
//                      every call is audited including the refusals.
//
// WHY AN ENROLMENT LINK RATHER THAN JUST OPENING /me/devices, which already
// carries an "enrol another" control. Because the EXTENSION is authenticated
// and the BROWSER is not. The extension holds an access token; the operator's
// browser holds no session for that identity, so `/me/devices` would land them
// on a sign-in page -- the dead end this whole family of issues is about. The
// `mql_enr_` token is what carries authorization ACROSS that boundary; it is
// the shape `/enroll` exists for, "a page a person opens from a link ... for
// someone holding no credential yet".
//
// AN OFFER, NEVER A GATE. Magic-link sign-in keeps working, and an operator who
// declines is not asked again this session -- see OfferMemory. Nagging somebody
// on every reconnect about an optional credential is how a good prompt becomes
// one people learn to dismiss without reading.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// the decision is the part worth testing, and `src/extension.ts` supplies the
// notification.
//
// Refs: #3902 #3885 #3884 #3591 #3408 #3409

/** The cluster roles that may mint an enrolment link. Mirrors adminops' gate. */
const MINTING_ROLES = new Set(["owner", "admin"]);

/** Who the caller is, as the authenticated stream reports them. */
export interface CallerIdentity {
  userId: string;
  clusterRole: string;
}

export interface PasskeyOfferDeps {
  /**
   * The caller's own identity, or null when the stream did not answer.
   *
   * Comes from `getMyAccess()`, which is the server's view of the actor rather
   * than anything this side decoded from a token -- so a role read here is the
   * role the mint will actually be gated on.
   */
  whoAmI(): Promise<CallerIdentity | null>;
  /** How many passkeys the CALLER has enrolled (`passkeysForSelf`). */
  countOwnPasskeys(): Promise<number>;
}

/**
 * Why no offer is being made. Carried rather than collapsed to a boolean
 * because the four reasons are genuinely different and a caller that wanted to
 * log or test one of them should not have to infer it from prose.
 */
export type NoOfferReason =
  | "alreadyEnrolled"
  | "declinedThisSession"
  | "cannotMint"
  | "indeterminate";

export type PasskeyOfferDecision =
  | { offer: true; userId: string }
  | { offer: false; reason: NoOfferReason };

/**
 * Which clusters the operator has already said no to, for this window's life.
 *
 * SESSION-SCOPED ON PURPOSE, and in memory rather than on disk. A decision to
 * skip enrolment today is not a decision to never be told again -- an operator
 * who declines because they are mid-task should meet the offer again next time
 * they open the editor, and one who declines because they enrolled on another
 * machine will not see it again anyway, because the passkey check answers
 * `alreadyEnrolled` from then on.
 *
 * Keyed by cluster NAME because the answer is per-cluster: declining on a
 * throwaway local install says nothing about a shared one.
 */
export class OfferMemory {
  private readonly declined = new Set<string>();

  decline(clusterName: string): void {
    this.declined.add(clusterName);
  }

  hasDeclined(clusterName: string): boolean {
    return this.declined.has(clusterName);
  }
}

/**
 * Whether to offer enrolment, and for whom.
 *
 * THE ORDER OF THE CHECKS IS THE CHEAP-FIRST ORDER, and it matters for more
 * than speed: the session memory is consulted BEFORE any network call, so an
 * operator who declined does not silently cost a round trip on every reconnect
 * for the rest of the session.
 *
 * EVERY FAILURE IS `indeterminate` AND EVERY `indeterminate` IS SILENT. This
 * runs immediately after a sign-in the operator asked for and got; surfacing
 * "could not determine your passkey state" on top of a successful sign-in
 * reports a problem they do not have, about a credential they may not want. The
 * cost of staying quiet is that the offer waits for the next sign-in.
 */
export async function decidePasskeyOffer(
  clusterName: string,
  deps: PasskeyOfferDeps,
  memory: OfferMemory,
): Promise<PasskeyOfferDecision> {
  if (memory.hasDeclined(clusterName)) {
    return { offer: false, reason: "declinedThisSession" };
  }

  let caller: CallerIdentity | null;
  try {
    caller = await deps.whoAmI();
  } catch {
    return { offer: false, reason: "indeterminate" };
  }
  if (caller === null || caller.userId.trim() === "") {
    return { offer: false, reason: "indeterminate" };
  }
  // The role gate is not a policy this side invents -- it is adminops', read
  // ahead of time. Offering a mint that will come back PERMISSION_DENIED puts
  // a refusal in front of somebody who did nothing wrong, and writes an audit
  // event for a call that should never have been made.
  if (!MINTING_ROLES.has(caller.clusterRole.trim().toLowerCase())) {
    return { offer: false, reason: "cannotMint" };
  }

  let passkeys: number;
  try {
    passkeys = await deps.countOwnPasskeys();
  } catch {
    return { offer: false, reason: "indeterminate" };
  }
  // NOT `> 0` on a number that might be NaN: a malformed row count must read
  // as "cannot tell" rather than as "no passkeys", because the second one
  // prompts somebody who is already enrolled.
  if (!Number.isFinite(passkeys) || passkeys < 0) {
    return { offer: false, reason: "indeterminate" };
  }
  if (passkeys > 0) {
    return { offer: false, reason: "alreadyEnrolled" };
  }

  return { offer: true, userId: caller.userId };
}

/**
 * The line the operator reads.
 *
 * Says what a passkey BUYS rather than what it is. "Enrol a passkey" is a
 * feature name; "sign in without waiting for an email" is the reason somebody
 * would say yes, and it is also the true difference for this cluster -- the
 * alternative path is a magic link.
 */
export function passkeyOfferMessage(clusterLabel: string): string {
  return (
    `memQL: "${clusterLabel}" has no passkey registered for your account. ` +
    "Enrolling one lets you sign in without waiting for an email."
  );
}
