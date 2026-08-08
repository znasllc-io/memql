// THE SEAM #3315 PLUGS INTO. Read this before adding any credential handling
// anywhere else in the portal.
//
// C1 (#3314) scaffolds the portal and connects it to a cluster; AUTHENTICATION
// IS #3315's JOB. The temptation at this stage is to invent something
// throwaway -- a token in localStorage, a prompt, a query parameter -- which
// #3315 would then have to find and rip out of however many call sites it had
// spread to. So instead there is one interface, one default implementation
// that supplies no credential, and a single place the connection reads it.
//
// WHAT #3315 DOES: writes an identity-service-backed PortalAuthSource
// (magic-link / OAuth against MEMQL_IDENTITY_BASE_URL, returning the bearer
// and refreshing it) and passes it to <ClusterProvider auth={...}>. Nothing
// else in the portal changes, because nothing else in the portal touches a
// credential.
//
// WHAT IT MUST NOT DO: reach past this interface. If a component needs to know
// whether the operator is signed in, that belongs on the connection state
// (which #3315 can widen), not on a second credential path.

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
