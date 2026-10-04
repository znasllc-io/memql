// The well-known first-party OAuth client this extension authorizes as.
//
// -----------------------------------------------------------------------------
// WHY THERE IS NO REGISTRATION STEP
// -----------------------------------------------------------------------------
//
// This extension used to obtain its client_id by RFC 7591 dynamic client
// registration -- POST <issuer>/register on the first sign-in, persisting the
// minted id into clusters.yaml. That failed on every cluster in its DEFAULT
// posture, because MEMQL_IDENTITY_OAUTH_DCR_ENABLED defaults to FALSE
// (memql#3719): /register is an unauthenticated write whose client_name is
// chosen by the caller, so only clusters that expose MCP to third-party
// connectors turn it on. Both grants dead-ended -- the browser flow at
// `https://identity.<domain>/register returned 403: registration_disabled`,
// and the device fallback at /device/code, which refuses an unregistered
// client.
//
// So the editor stopped riding the third-party door. It ships WITH the
// product, and identity now knows it the way it knows any operator-configured
// relying party: a compiled-in first-party entry
// (component/identity/builtin_clients.go) present on every cluster with zero
// operator configuration. Resolving a client_id is therefore a local constant
// lookup and never a network call.
//
// -----------------------------------------------------------------------------
// WHY THE REGISTERED REDIRECT URI HAS NO PORT
// -----------------------------------------------------------------------------
//
// (This reasoning moved here from register.ts when that file was deleted. It
// outlives registration because it is about the MATCHER, not about how the
// client came to exist.)
//
// The loopback listener takes whatever ephemeral port the kernel hands it
// (loopback.ts), so the redirect_uri the browser actually comes back to is
// `http://127.0.0.1:54321/callback` -- a different number every sign-in. A
// registration that pinned one port would therefore be wrong on the second
// attempt, and registering a range is not a thing OAuth offers.
//
// RFC 8252 §7.3 exists for precisely this, and identity implements it: a
// registered loopback redirect URI WITH NO EXPLICIT PORT matches an incoming
// URI on any port as long as the scheme, host and path agree
// (component/identity/config.go, matchesLoopbackAnyPort -- `r.Port() !== ""`
// returns false, i.e. a registered URI that carries a port opts OUT of the
// exception and goes back to exact-match). So the value below is deliberately
// portless, and adding a port to it would silently break every callback.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { CALLBACK_PATH, LOOPBACK_HOST } from "./loopbackAddress.js";

/**
 * The client_id this extension authorizes with, on every cluster.
 *
 * MUST equal identity.BuiltinClientVSCode
 * (component/identity/builtin_clients.go). It is part of the wire contract
 * between a released extension and a released cluster: changing it strands
 * every editor already carrying the old string.
 */
export const WELL_KNOWN_CLIENT_ID = "memql-vscode";

/**
 * The redirect URI the built-in client is registered with, PORTLESS on purpose
 * -- see the header. Must equal the `redirectURIs` entry in identity's built-in
 * registry byte for byte, because the RFC 8252 matcher compares scheme, host
 * and path exactly.
 */
export const WELL_KNOWN_REDIRECT_URI = `http://${LOOPBACK_HOST}${CALLBACK_PATH}`;

// -----------------------------------------------------------------------------
// WHY THE REGISTRY'S client_id IS NOT THE EDITOR'S
// -----------------------------------------------------------------------------
//
// `~/.memql/clusters.yaml` is SHARED: the MemQL Cockpit writes it too, and it
// records its OWN client there (`client_id: cockpit`). This extension used to
// read that value as an "operator override" and authorize as it. The Cockpit's
// client is registered only for `/cockpit/callback`, so the editor's
// `http://127.0.0.1:<port>/callback` was refused at /authorize, the browser
// showed a bare "Bad Request", and the editor waited out its callback deadline.
// It failed exactly when the Cockpit had touched the slot, which is why it
// looked intermittent: a cluster the extension installed carried no client_id
// and signed in fine.
//
// So the rule is: a client_id in the shared registry belongs to the tool that
// wrote it. The editor always signs in AS ITSELF, and refreshes as the client a
// refresh token was actually issued to -- which the store keeps beside the
// token (auth/store.ts) rather than reading back out of a file another tool
// owns.

/** The client_id every sign-in from this editor authorizes as, loopback and device code alike. */
export const EDITOR_CLIENT_ID = WELL_KNOWN_CLIENT_ID;

/**
 * The client_id a refresh presents: the one the refresh token was ISSUED to,
 * as recorded beside it, or the editor's own id when nothing was recorded (a
 * token from before the record existed, or one pasted into the file by hand,
 * which the editor then treats as its own).
 *
 * Never the registry's `client_id`: that is another tool's.
 */
export function refreshClientId(issuedTo: string | undefined): string {
  const recorded = (issuedTo ?? "").trim();
  return recorded === "" ? EDITOR_CLIENT_ID : recorded;
}

/** normalizeIssuer trims whitespace and any trailing slashes, matching identityBaseUrlFor. */
export function normalizeIssuer(issuer: string | undefined): string {
  return (issuer ?? "").trim().replace(/\/+$/, "");
}
