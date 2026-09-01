// MyAccess data shape, data only. No portal chrome (memql#4706).
//
// ===========================================================================
// THE PARSER THAT USED TO LIVE HERE IS GONE (memql#4775)
// ===========================================================================
// `parseProfileAccess` returned null unless `userId`, `primaryEmail` AND
// `clusterRole` were all non-blank. That is one of the two ways the shell came
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
  clusterRole: string;
}
