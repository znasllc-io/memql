package identity

import (
	"context"
	"log/slog"
)

// Unified OAuth client resolution.
//
// The identity service admits OAuth clients from three sources, in this order:
//
//  1. The static MEMQL_IDENTITY_REGISTERED_CLIENTS slice (Config.RegisteredClients) --
//     operator-pinned relying parties (e.g. the product SPA).
//  2. The compiled-in first-party registry (builtin_clients.go) -- software
//     that ships WITH the product, so identity knows it without anyone
//     registering it. One entry today: the VS Code extension.
//  3. DB-backed v1:identity:oauthClient rows minted at POST /register
//     (RFC 7591 dynamic client registration) -- claude.ai / Claude Desktop's
//     "add custom connector" self-registers a public client_id it can't
//     pre-configure.
//
// WHY THAT ORDER. Operator config wins outright: an operator who writes
// `memql-vscode` into MEMQL_IDENTITY_REGISTERED_CLIENTS is deliberately
// shadowing the built-in -- to widen its redirect set, or to opt out of the
// policy the built-in declares -- and a resolution order that let compiled-in
// defaults override that would make the env var advisory. Built-ins come next
// because they are code rather than rows: nothing an unauthenticated caller
// does can add, rename or displace one, so consulting them before the DCR store
// means a self-registered client can never impersonate a first-party id.
//
// Every OAuth client-validation call site (/authorize, /oauth/token, /device/code,
// the magic-link issue + verify paths) MUST resolve through ResolveClient /
// ClientAllowsRedirectURI so all three sources work identically: static first
// (cheap, no DB hit, and operator config always wins), then built-ins (also
// free), then the store.

// ResolveClient returns the RegisteredClient for clientId, checking the
// static config FIRST, then the compiled-in first-party registry, then the
// DB-backed oauthClient store. Returns nil when the client is registered in
// none of the three.
//
// A DB row is converted to a *RegisteredClient carrying its ClientId +
// RedirectURIs, so the redirect matcher applies uniformly. store may be
// nil (static-only resolution -- used by tests and by code paths that
// don't carry a Store handle); a nil store simply skips the DB fallback.
func ResolveClient(ctx context.Context, cfg Config, store *Store, clientId string) *RegisteredClient {
	c, _ := ResolveClientWithOrigin(ctx, cfg, store, clientId)
	return c
}

// ResolveClientWithOrigin is ResolveClient plus the answer to WHERE the client
// came from: selfRegistered is true when it was found in the DB-backed
// oauthClient store rather than in the operator-configured static list or the
// compiled-in first-party registry.
//
// WHY THAT DISTINCTION IS WORTH RETURNING (memql#3794). The two sources carry
// completely different trust. A static client was written into
// MEMQL_IDENTITY_REGISTERED_CLIENTS by an operator. A store row was created by
// whoever called POST /register -- unauthenticated by design, because the point
// of RFC 7591 is completing the flow with no human present -- and it CHOSE ITS
// OWN client_name, which is the string a person is then shown when asked to
// approve access.
//
// This function has always known which branch it took and always discarded it,
// so the consent page could not tell an operator-configured application from a
// stranger's self-description, and everything downstream treated them alike.
//
// Callers that only need the redirect matcher keep using ResolveClient. This is
// for the ones rendering something to a human.
func ResolveClientWithOrigin(ctx context.Context, cfg Config, store *Store, clientId string) (client *RegisteredClient, selfRegistered bool) {
	if clientId == "" {
		return nil, false
	}
	if c := cfg.FindClient(clientId); c != nil {
		return c, false
	}
	// Built-ins are selfRegistered=false -- they are first-party by
	// construction, so the consent page renders them the way it renders an
	// operator-configured client and never as a stranger's self-description
	// (memql#3794).
	if c := FindBuiltinClient(clientId); c != nil {
		return c, false
	}
	if store == nil {
		return nil, false
	}
	row, err := store.LookupOAuthClientByClientId(ctx, clientId)
	if err != nil {
		if store.Logger != nil {
			store.Logger.Warn("oauth client store lookup failed",
				slog.String("client_id", clientId),
				slog.String("error", err.Error()),
			)
		}
		return nil, false
	}
	if row == nil || row.ClientId == "" {
		return nil, false
	}
	return &RegisteredClient{
		ClientId:     row.ClientId,
		RedirectURIs: row.RedirectURIs,
		Name:         row.ClientName,
	}, true
}

// ClientAllowsRedirectURI resolves clientId through ResolveClient (static,
// then built-in, then DB) and applies the redirect-URI matcher (exact-match
// plus the RFC 8252 loopback-any-port exception). Returns false when the
// client is unknown or the uri is not registered for it. store may be nil.
func ClientAllowsRedirectURI(ctx context.Context, cfg Config, store *Store, clientId, uri string) bool {
	return clientAllowsRedirectURI(ResolveClient(ctx, cfg, store, clientId), uri)
}
