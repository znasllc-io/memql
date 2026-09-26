// MyAccess data shape, data only. No portal chrome (memql#4706).
//
// ===========================================================================
// THE PARSER THAT USED TO LIVE HERE IS GONE (memql#4775)
// ===========================================================================
// `parseProfileAccess` returned null unless `userId`, `primaryEmail` AND
// `role` were all non-blank. That is one of the two ways the shell came
// to believe nobody had a role: a credential with no address -- a PAT, an
// operator key, a service account, all of which the SDK's own `AccessSummary`
// says exist -- had its perfectly good role thrown away with it.
//
// It also had no caller left. Boot used to fetch the facts over HTTP and run
// them through it; the facts now arrive from `query.getMyAccess()` on the
// cluster stream, already typed, and are narrowed by `accessFromSummary` in
// `useResolvedAccess.ts` -- which is lenient about the email and says why.
// A dead strict parser beside a live lenient one is an invitation to use the
// wrong one.
//
// What stays here is the TYPE, which is the shell's own vocabulary for a
// session and is deliberately narrower than the wire summary: no requestId, no
// sessionId, nothing chrome does not render or gate on.

export interface ProfileAccess {
  userId: string;
  primaryEmail: string;
  /**
   * The cluster role's SLUG, as the person's `v1:identity:user` row carries it.
   *
   * IT WAS `role` AND IT WAS AN ENUM (epic memql#5166). The wire field
   * was `cluster_role`, the proto's `UserRole`, and an enum can only name the
   * roles the engine shipped -- so a role a cluster authored for itself arrived
   * as the value an unauthenticated caller gets, and the shell could not tell
   * "you hold a role I do not know" from "you hold none". The field is renamed
   * as well as retyped, because `role` was the camelCase of a wire field
   * that no longer exists and it named an enum.
   */
  role: string;
  /**
   * The role's DISPLAY NAME off the cluster's catalog ("Owner", "Support
   * Lead"). Empty when the slug ranks nowhere -- a role retired under its
   * holder, or a node whose catalog has not loaded.
   *
   * EMPTY IS A REAL ANSWER, not a gap to paper over: a surface renders the slug
   * it already holds rather than inventing a title for a role the engine will
   * refuse this person everything for.
   */
  roleName: string;
  /**
   * The role's rung, HIGHER == more privileged. Zero when the slug ranks
   * nowhere -- the answer rather than "unknown", because an unrankable role
   * admits nothing, which is what rank 0 means everywhere else in this model.
   *
   * Presentation only. `roleAdmits` resolves the actor's rung against the LIVE
   * ladder rather than trusting this number, so a stale rank cannot unlock a
   * surface; this is what the identity strip prints.
   */
  rank: number;
  /**
   * The groups this person is in, as MyAccess reports them (epic memql#5165,
   * section H).
   *
   * This membership summary accompanies identity so pickers can resolve the
   * caller's organization scope before an app opens. Organization group reads
   * are membership-bounded; global identity directory access remains separate.
   *
   * ABSENT OR EMPTY MEANS "NOT REPORTED" as well as "none", because a cluster
   * whose engine predates the field sends nothing. Every reader treats it as a
   * FALLBACK behind what it can read for itself, never as an authority --
   * which is also why it is OPTIONAL: a harness constructing a session by hand
   * is not making a claim about anybody's groups.
   */
  groups?: AccessGroup[];
  /** Resolved organization scope reported by the server; absence grants nothing. */
  accountIds?: string[];
  everyAccount?: boolean;
}

/** One group, and the client it grants -- named, so a member can read it. */
export interface AccessGroup {
  id: string;
  name: string;
  kind: string;
  accountId: string;
  accountName: string;
}
