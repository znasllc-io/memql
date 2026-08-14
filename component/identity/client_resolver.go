package identity

import (
	"context"
	"log/slog"
)

// Unified OAuth client resolution.
//
// The identity service admits OAuth clients from two sources:
//
//  1. The static MEMQL_IDENTITY_REGISTERED_CLIENTS slice (Config.RegisteredClients) --
//     operator-pinned relying parties (e.g. the product SPA).
//  2. DB-backed v1:identity:oauthClient rows minted at POST /register
//     (RFC 7591 dynamic client registration) -- claude.ai / Claude Desktop's
//     "add custom connector" self-registers a public client_id it can't
//     pre-configure.
//
// Every OAuth client-validation call site (/authorize, /oauth/token, the
// magic-link issue + verify paths) MUST resolve through ResolveClient /
// ClientAllowsRedirectURI so both sources work identically: static first
// (cheap, no DB hit, and operator config always wins), then the store.

// ResolveClient returns the RegisteredClient for clientId, checking the
// static config FIRST, then the DB-backed oauthClient store. Returns nil
// when the client is registered in neither source.
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
// oauthClient store rather than in the operator-configured static list.
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

// ClientAllowsRedirectURI resolves clientId through ResolveClient (static
// then DB) and applies the redirect-URI matcher (exact-match plus the
// RFC 8252 loopback-any-port exception). Returns false when the client is
// unknown or the uri is not registered for it. store may be nil.
func ClientAllowsRedirectURI(ctx context.Context, cfg Config, store *Store, clientId, uri string) bool {
	return clientAllowsRedirectURI(ResolveClient(ctx, cfg, store, clientId), uri)
}
