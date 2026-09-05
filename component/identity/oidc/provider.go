// Package oidc adds an UPSTREAM identity provider to MemQL (memql#4611).
//
// -----------------------------------------------------------------------------
// WHY THIS IS A DIFFERENT SHAPE FROM EVERYTHING ELSE IN component/identity
// -----------------------------------------------------------------------------
//
// MemQL is an OAuth 2.1 AUTHORIZATION SERVER: claude.ai, the VS Code extension
// and the portal authorize against it. Until now a PERSON always proved
// themselves to MemQL directly -- magic link, passkey, device code, recovery
// key -- so the cluster was an authorization server with no upstream.
//
// For an organization already on Microsoft 365 that is the wrong shape, and the
// reason is not convenience:
//
//   - Directory membership IS the invitation, which removes the entire class of
//     defect memql#4601 exists to fix. No invitation emails for internal staff.
//   - No new credential for the user to hold, lose, or be phished out of.
//   - The organization's existing MFA and conditional access apply, rather than
//     being approximated per-cluster.
//   - DEPROVISIONING FOLLOWS THE DIRECTORY. Today, removing somebody from the
//     company does not remove them from a MemQL cluster. That one is not a
//     convenience at all.
//
// -----------------------------------------------------------------------------
// GENERIC OIDC, NOT MICROSOFT-ONLY
// -----------------------------------------------------------------------------
//
// Everything here is driven by the provider's own DISCOVERY DOCUMENT
// (OpenID Connect Discovery 1.0, `/.well-known/openid-configuration`), so Entra
// ID is a configuration rather than a code path. That is the same lesson
// memql#4624 records on the client side: an endpoint composed by convention is
// an endpoint that is wrong for somebody, and the document is published
// precisely so nobody has to guess.
//
// The one Entra-specific concession is `tenantIssuerTemplate` handling in
// config.go, because Entra's issuer carries the tenant id and an operator has
// the tenant id rather than the issuer URL.
//
// -----------------------------------------------------------------------------
// WHAT THIS PACKAGE IS NOT
// -----------------------------------------------------------------------------
//
// It does NOT decide who may register, what role they get, or whether the row
// it lands on is new. Those are policy, they involve the cluster's own state,
// and they live in linking.go and in registration. This package answers exactly
// one question: WHO does the upstream say this person is, and can that be
// believed. Keeping it that narrow is what makes it testable without a cluster.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DiscoveryPath is OpenID Connect Discovery 1.0 §4. Note this is NOT the same
// path as RFC 8414's `/.well-known/oauth-authorization-server`, which MemQL
// publishes for its own clients (component/identity/oauth_metadata.go). A
// provider may serve both; OIDC clients read this one.
const DiscoveryPath = "/.well-known/openid-configuration"

// Metadata is the subset of the discovery document this package uses.
//
// A subset rather than the whole thing, deliberately: every field here is one
// the flow actually reads, so an absent one is a real problem rather than a
// gap in a struct nobody consults.
type Metadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	UserInfoEndpoint      string   `json:"userinfo_endpoint"`
	EndSessionEndpoint    string   `json:"end_session_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	ResponseTypes         []string `json:"response_types_supported"`
	IDTokenSigningAlgs    []string `json:"id_token_signing_alg_values_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

// Valid reports whether the document names everything a code flow needs.
func (m Metadata) Valid() error {
	switch {
	case strings.TrimSpace(m.Issuer) == "":
		return errors.New("discovery document names no issuer")
	case strings.TrimSpace(m.AuthorizationEndpoint) == "":
		return errors.New("discovery document names no authorization_endpoint")
	case strings.TrimSpace(m.TokenEndpoint) == "":
		return errors.New("discovery document names no token_endpoint")
	case strings.TrimSpace(m.JWKSURI) == "":
		// Without JWKS there is no way to verify an id token, and an
		// unverified id token is a JSON document a stranger handed us.
		return errors.New("discovery document names no jwks_uri, so an id token could not be verified")
	}
	return nil
}

// SupportsS256 reports whether the provider advertises S256 PKCE.
//
// An EMPTY list means "not advertised", which the spec leaves open, and
// refusing on a missing optional field would reject a conformant provider. Only
// a list that is PRESENT and lacks S256 is a no. (memql#4624 records the same
// rule on the client side, for the same reason.)
func (m Metadata) SupportsS256() bool {
	if len(m.CodeChallengeMethods) == 0 {
		return true
	}
	for _, v := range m.CodeChallengeMethods {
		if v == "S256" {
			return true
		}
	}
	return false
}

// Discoverer fetches and caches a provider's discovery document.
//
// CACHED WITH A TTL rather than forever: an endpoint move is rare but real, and
// a process that has to be restarted to notice one is a process that will be
// restarted at the worst moment. Cached at all because this is on the sign-in
// path and the document changes on the order of years.
type Discoverer struct {
	HTTP *http.Client
	TTL  time.Duration

	mu     sync.Mutex
	cached map[string]cachedMetadata
}

type cachedMetadata struct {
	doc     Metadata
	fetched time.Time
}

// DefaultDiscoveryTTL is one hour: long enough that sign-in never waits on a
// fetch in practice, short enough that a rotation is picked up the same day.
const DefaultDiscoveryTTL = time.Hour

// Discover returns the provider's metadata, from cache when fresh.
//
// THE ISSUER IN THE DOCUMENT MUST MATCH THE ONE ASKED FOR. OpenID Connect
// Discovery §4.3 requires it, and it is not a formality: without the check, a
// provider (or anything that can answer for its hostname) could name a
// DIFFERENT issuer, and every id token this cluster then accepted would be
// validated against the wrong authority.
func (d *Discoverer) Discover(ctx context.Context, issuer string) (Metadata, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return Metadata{}, errors.New("no issuer configured")
	}
	if !strings.HasPrefix(issuer, "https://") {
		// An id token is a bearer of identity. Fetching the keys that verify it
		// over plaintext would let anyone on the path choose them.
		return Metadata{}, fmt.Errorf("issuer %q is not https", issuer)
	}

	ttl := d.TTL
	if ttl <= 0 {
		ttl = DefaultDiscoveryTTL
	}
	now := time.Now()

	d.mu.Lock()
	if hit, ok := d.cached[issuer]; ok && now.Sub(hit.fetched) < ttl {
		d.mu.Unlock()
		return hit.doc, nil
	}
	d.mu.Unlock()

	client := d.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+DiscoveryPath, nil)
	if err != nil {
		return Metadata{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Metadata{}, fmt.Errorf("fetch %s%s: %w", issuer, DiscoveryPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Metadata{}, fmt.Errorf("%s%s answered %d", issuer, DiscoveryPath, resp.StatusCode)
	}

	var doc Metadata
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return Metadata{}, fmt.Errorf("parse %s%s: %w", issuer, DiscoveryPath, err)
	}
	if err := doc.Valid(); err != nil {
		return Metadata{}, err
	}
	if strings.TrimRight(doc.Issuer, "/") != issuer {
		return Metadata{}, fmt.Errorf(
			"discovery at %s names issuer %q; an id token validated against a mismatched authority "+
				"is not validated at all (OIDC Discovery §4.3)", issuer, doc.Issuer)
	}

	d.mu.Lock()
	if d.cached == nil {
		d.cached = map[string]cachedMetadata{}
	}
	d.cached[issuer] = cachedMetadata{doc: doc, fetched: now}
	d.mu.Unlock()
	return doc, nil
}

// AuthorizeParams are the per-sign-in values the authorize URL carries.
type AuthorizeParams struct {
	ClientID    string
	RedirectURI string
	Scopes      []string
	State       string
	// Nonce binds the id token to THIS authorization request. It is not
	// interchangeable with state: state protects the redirect (was this
	// callback the one we started), nonce protects the TOKEN (was this id token
	// minted for that request, or replayed from another one). OIDC Core §3.1.2.1
	// requires it for the code flow when the client asks for one, and this
	// always asks.
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	// Prompt / LoginHint are optional passthroughs; empty means unset.
	Prompt    string
	LoginHint string
	// Domain hint, which Entra honours to skip the account picker for a known
	// tenant. Harmless to other providers, which ignore unknown parameters.
	DomainHint string
}

// AuthorizeURL composes the upstream authorization URL.
func AuthorizeURL(m Metadata, p AuthorizeParams) (string, error) {
	if strings.TrimSpace(p.ClientID) == "" {
		return "", errors.New("no client_id configured for the upstream provider")
	}
	if strings.TrimSpace(p.State) == "" || strings.TrimSpace(p.Nonce) == "" {
		// Both are generated by the caller and both are load-bearing; a blank
		// one here means a bug upstream of this call, not a permissive default.
		return "", errors.New("state and nonce are both required")
	}
	u, err := url.Parse(m.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("authorization_endpoint %q: %w", m.AuthorizationEndpoint, err)
	}
	scopes := p.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes()
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", p.RedirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", p.State)
	q.Set("nonce", p.Nonce)
	if p.CodeChallenge != "" {
		q.Set("code_challenge", p.CodeChallenge)
		q.Set("code_challenge_method", p.CodeChallengeMethod)
	}
	for k, v := range map[string]string{
		"prompt":      p.Prompt,
		"login_hint":  p.LoginHint,
		"domain_hint": p.DomainHint,
	} {
		if strings.TrimSpace(v) != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// DefaultScopes is the minimum that answers "who is this".
//
// `openid` is required; `email` and `profile` are what let the cluster show a
// person their own name and match them to an existing row. GROUPS ARE NOT HERE:
// a provider that carries them in the id token needs no extra scope, and one
// that does not needs a provider-specific scope the operator configures. Asking
// for something the directory may refuse would fail sign-in for a reason that
// has nothing to do with the person signing in.
func DefaultScopes() []string { return []string{"openid", "email", "profile"} }
