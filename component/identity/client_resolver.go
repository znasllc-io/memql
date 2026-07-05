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
	if clientId == "" {
		return nil
	}
	if c := cfg.FindClient(clientId); c != nil {
		return c
	}
	if store == nil {
		return nil
	}
	row, err := store.LookupOAuthClientByClientId(ctx, clientId)
	if err != nil {
		if store.Logger != nil {
			store.Logger.Warn("oauth client store lookup failed",
				slog.String("client_id", clientId),
				slog.String("error", err.Error()),
			)
		}
		return nil
	}
	if row == nil || row.ClientId == "" {
		return nil
	}
	return &RegisteredClient{
		ClientId:     row.ClientId,
		RedirectURIs: row.RedirectURIs,
		Name:         row.ClientName,
	}
}

// ClientAllowsRedirectURI resolves clientId through ResolveClient (static
// then DB) and applies the redirect-URI matcher (exact-match plus the
// RFC 8252 loopback-any-port exception). Returns false when the client is
// unknown or the uri is not registered for it. store may be nil.
func ClientAllowsRedirectURI(ctx context.Context, cfg Config, store *Store, clientId, uri string) bool {
	return clientAllowsRedirectURI(ResolveClient(ctx, cfg, store, clientId), uri)
}
