// THE CREDENTIAL SEAM. Read this before adding any credential handling
// anywhere else in the portal.
//
// C1 (#3314) scaffolded the portal with one interface, one no-credential
// default, and a single place the connection reads it -- precisely so #3315
// would not have to hunt a throwaway token out of a dozen call sites.
//
// #3315 FILLED IT IN. src/auth/identityAuthSource.ts is the real
// implementation: OAuth 2.1 authorization-code + PKCE against the identity
// service, the access token held in a closure variable, refresh through the
// HttpOnly cookie. It is built and owned by src/auth/AuthProvider.tsx and
// handed to <ClusterProvider auth={...}> by src/app/App.tsx.
//
// The seam held: those two implementations -- the anonymous one below and the
// identity-backed one -- are the only credential paths that exist.
//
// WHAT A LATER CHANGE MUST NOT DO: reach past this interface. A component that
// needs to know whether the operator is signed in asks `useAuth().status` (a
// state) or `IdentityAuthSource.hasToken()` (a boolean) -- never for the token
// itself. Every additional place that can read the string is another place it
// can be logged, folded into an error report, or written to storage by
// accident.

import type { ConnectionAuth } from "@znasllc-io/memql-sdk-core/client";

export interface PortalAuthSource {
  // bearer resolves the credential to dial with, or null to dial with none.
  // Called once per dial attempt, so a reconnect always re-reads it.
  bearer(): Promise<string | null>;

  // refresh resolves a FRESH credential. The SDK owns the rotation timer and
  // the in-place rotateAuth round-trip (sdk/ts Connection, memql#1110); this
  // is only the "get me a new one" hook it calls, both proactively before
  // expiry and reactively when the server rejects the current bearer.
  //
  // Returning null means "give up" -- the SDK stops rotating and the stream
  // runs until the server closes it.
  refresh(): Promise<string | null>;
}

// anonymousAuthSource dials with no credential at all.
//
// This is not a pretend-authenticated stub: it supplies nothing, so on a
// cluster with MEMQL_IDENTITY_ENABLED=true (the default in every environment)
// the stream is refused and the portal shows a connection error naming that.
// That is the honest pre-#3315 behaviour, and it is visible rather than
// silent. Against a cluster running with auth disabled for troubleshooting,
// the same dial is admitted as the synthetic local-dev cluster owner and the
// concept browser works end to end -- which is what makes C1 demonstrable
// without pre-empting #3315's design.
export const anonymousAuthSource: PortalAuthSource = {
  bearer: async () => null,
  refresh: async () => null,
};

// toConnectionAuth adapts a PortalAuthSource to the SDK's ConnectionAuth
// shape. Returns undefined when the source supplies no bearer, because the
// SDK treats a present-but-empty `bearer` differently from an absent one --
// only the absent form dials without an auth subprotocol.
export async function toConnectionAuth(
  source: PortalAuthSource,
): Promise<ConnectionAuth | undefined> {
  const bearer = await source.bearer();
  if (bearer === null || bearer === "") return undefined;
  return {
    bearer,
    onTokenExpired: () => source.refresh(),
  };
}
