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
// AND NEVER PART OF THE OWNERSHIP WALK (memql#4078). The walk that finishes an
// install -- passkey, then a verification sign-in, then the portal -- ends in a
// sign-in, and this offer fires on the heels of every sign-in. So the first
// fully-green install landed THREE stacked notifications in three vocabularies,
// and the offer's, being a toast with buttons, outlived the others in the
// notification bell: the operator clicked it AFTER enrolling through the walk
// and the browser answered that a passkey already exists. Two rules close that:
// the walk marks its cluster suppressed (OfferMemory.suppress) before it mints,
// so the sign-in that ends it cannot raise a fresh offer; and a clicked offer
// re-reads the passkey count before acting, so a stale one answers "all set"
// in the editor instead of dead-ending in the browser (enrolmentStillNeeded).
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// the decision is the part worth testing, and `src/extension.ts` supplies the
// notification.
//
// Refs: #4078 #3902 #3885 #3884 #3591 #3408 #3409

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
 * because the five reasons are genuinely different and a caller that wanted to
 * log or test one of them should not have to infer it from prose.
 *
 * `suppressedByWalk` is not `declinedThisSession` under another name: declined
 * means the OPERATOR said no, suppressed means the ownership walk claimed the
 * cluster's enrolment story and the operator said nothing (memql#4078).
 */
export type NoOfferReason =
  | "alreadyEnrolled"
  | "declinedThisSession"
  | "suppressedByWalk"
  | "cannotMint"
  | "indeterminate";

export type PasskeyOfferDecision =
  | { offer: true; userId: string }
  | { offer: false; reason: NoOfferReason };

/**
 * Which clusters this window will not offer enrolment on, and why. Two markers,
 * kept apart because they answer different questions:
 *
 *   declined    the OPERATOR said no (or dismissed the toast, which is an
 *               answer too).
 *   suppressed  the OWNERSHIP WALK owns this cluster's enrolment story now
 *               (memql#4078). The walk mints its own link, verifies with its
 *               own sign-in, and that sign-in must not raise a fresh offer on
 *               top of the walk's notifications -- nor leave a stale one in
 *               the bell to be clicked after the passkey already exists.
 *
 * SESSION-SCOPED ON PURPOSE, and in memory rather than on disk. A decision to
 * skip enrolment today is not a decision to never be told again -- an operator
 * who declines because they are mid-task should meet the offer again next time
 * they open the editor, and one who declines because they enrolled on another
 * machine will not see it again anyway, because the passkey check answers
 * `alreadyEnrolled` from then on. Suppression is the same shape: once the walk
 * has run, the next window's check answers `alreadyEnrolled` on its own.
 *
 * Keyed by cluster NAME because the answer is per-cluster: declining on a
 * throwaway local install says nothing about a shared one, and a walk through
 * one cluster says nothing about the others.
 */
export class OfferMemory {
  private readonly declined = new Set<string>();
  private readonly suppressed = new Set<string>();

  decline(clusterName: string): void {
    this.declined.add(clusterName);
  }

  hasDeclined(clusterName: string): boolean {
    return this.declined.has(clusterName);
  }

  suppress(clusterName: string): void {
    this.suppressed.add(clusterName);
  }

  hasSuppressed(clusterName: string): boolean {
    return this.suppressed.has(clusterName);
  }
}

/**
 * Whether to offer enrolment, and for whom.
 *
 * THE ORDER OF THE CHECKS IS THE CHEAP-FIRST ORDER, and it matters for more
 * than speed: the session memory -- both markers -- is consulted BEFORE any
 * network call, so a cluster the operator declined on, or one the ownership
 * walk claimed, does not silently cost a round trip on every reconnect for the
 * rest of the session. Suppression is asked first because it is the stronger
 * claim: it holds whatever the operator later says about the offer.
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
  if (memory.hasSuppressed(clusterName)) {
    return { offer: false, reason: "suppressedByWalk" };
  }
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

/**
 * Whether a clicked offer should still mint, given a just-re-read passkey count.
 *
 * THE CLICK CAN BE STALE (memql#4078). The offer is a toast with buttons, so it
 * persists in the notification bell; the click can arrive minutes after the
 * decision, late enough for the operator to have enrolled through the ownership
 * walk in between. Acting on the stale offer minted a fresh link and left the
 * BROWSER to deliver the bad news ("a passkey already exists"). So the caller
 * re-reads the count at click time and asks this before minting.
 *
 * THE DEFAULT IS THE INVERSE OF decidePasskeyOffer's, on purpose. There, an
 * unreadable count reads as "stay quiet", because nobody asked for the offer.
 * Here somebody DID ask -- they clicked -- so cancelling the mint over a
 * transient query failure would be a new dead end. Only a CONFIRMED enrolment
 * cancels the click; NaN, a negative, or an infinity reads as "cannot tell,
 * proceed".
 */
export function enrolmentStillNeeded(passkeyCount: number): boolean {
  return !(Number.isFinite(passkeyCount) && passkeyCount > 0);
}

/**
 * The one-line answer a stale offer gets instead of a browser dead-end.
 *
 * An INFO line, not an error: an operator who clicked after enrolling through
 * the walk has done everything right, and the fact to confirm is that they are
 * finished -- not that something went wrong.
 */
export function passkeyAlreadyEnrolledMessage(): string {
  return "memQL: this account already has a passkey -- you are all set.";
}
