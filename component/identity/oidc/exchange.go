package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenResponse is the upstream's answer to a code exchange.
type TokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// ExchangeParams are the values a code exchange needs.
type ExchangeParams struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Code         string
	CodeVerifier string
}

// Exchange redeems an authorization code at the upstream token endpoint.
//
// FORM-ENCODED, because RFC 6749 §4.1.3 says so and providers enforce it -- the
// JSON body every other request in this tree uses is refused here.
//
// THE CLIENT SECRET IS OPTIONAL, and that is not laxness. A confidential client
// (Entra's default for a web app) has one; a public client using PKCE does not,
// and sending an empty `client_secret` to a provider that expects none is a
// rejection. Which one this cluster is, is the operator's registration choice,
// so it is read rather than assumed.
func Exchange(ctx context.Context, client *http.Client, m Metadata, p ExchangeParams) (TokenResponse, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", p.Code)
	form.Set("redirect_uri", p.RedirectURI)
	form.Set("client_id", p.ClientID)
	if strings.TrimSpace(p.CodeVerifier) != "" {
		form.Set("code_verifier", p.CodeVerifier)
	}
	if strings.TrimSpace(p.ClientSecret) != "" {
		form.Set("client_secret", p.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("token exchange at %s: %w", m.TokenEndpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return TokenResponse{}, fmt.Errorf("parse token response from %s: %w", m.TokenEndpoint, err)
	}
	if out.Error != "" {
		// The provider's own words. A fixed token here would collapse
		// "invalid_client" (registration is wrong), "invalid_grant" (the code
		// expired) and "unauthorized_client" (the grant is not enabled) into
		// one message, and those send an operator to three different places.
		desc := out.ErrorDesc
		if desc == "" {
			desc = "no description"
		}
		return out, fmt.Errorf("the identity provider refused the code exchange: %s (%s)", out.Error, desc)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("token exchange at %s answered %d", m.TokenEndpoint, resp.StatusCode)
	}
	if strings.TrimSpace(out.IDToken) == "" {
		// Without an id token there is no verified statement of WHO this is --
		// only an access token, which says what the bearer may do at the
		// provider and nothing about identity. Signing somebody in on that is
		// the mistake OIDC exists to prevent.
		return out, fmt.Errorf("the identity provider returned no id_token, so it said nothing about who signed in")
	}
	return out, nil
}
