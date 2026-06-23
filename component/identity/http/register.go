package http

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// maxRegisterBodyBytes caps the POST /register request body. The
// endpoint is intentionally unauthenticated (public clients self-
// register), so the read is bounded to keep the attack surface small.
//
// TODO(#1573 follow-up): add a per-IP rate limit on POST /register
// (reuse the abuse.Middleware machinery). Deferred to avoid pulling new
// infra into the DCR landing; the bounded body read + JSON-only + POST-
// only gates are the minimal hardening for now.
const maxRegisterBodyBytes = 16 << 10 // 16 KiB

// maxRegisterRedirectURIs caps how many redirect_uris a single dynamic
// registration may carry.
const maxRegisterRedirectURIs = 5

// registerRequest is the subset of the RFC 7591 client-metadata body we
// accept. Unknown fields are ignored.
type registerRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// registerResponse mirrors the RFC 7591 §3.2.1 client information
// response for a public client (no client_secret is issued).
type registerResponse struct {
	ClientId                string   `json:"client_id"`
	ClientIdIssuedAt        int64    `json:"client_id_issued_at"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	ClientName              string   `json:"client_name,omitempty"`
}

// handleRegister implements RFC 7591 OAuth 2.0 Dynamic Client
// Registration. claude.ai / Claude Desktop's "add custom connector"
// flow POSTs client metadata here and gets back a freshly minted public
// client_id (no secret) it then uses at /authorize + /oauth/token.
//
// The endpoint is gated on Cfg.OAuthDCREnabled (env
// MEMQL_IDENTITY_OAUTH_DCR_ENABLED, default true). When disabled it returns
// 403 registration_disabled and persists nothing.
//
// Public clients only: token_endpoint_auth_method must be "none" (or
// empty -> defaulted to none). Each redirect_uri must be https or a
// loopback http URI; non-empty; count capped.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.OAuthDCREnabled {
		s.writeJSONError(w, http.StatusForbidden, "registration_disabled",
			"dynamic client registration is disabled on this server")
		return
	}

	// JSON only. RFC 7591 mandates application/json request bodies.
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		s.writeJSONError(w, http.StatusUnsupportedMediaType, "invalid_request",
			"request body must be application/json")
		return
	}

	body, err := readRegisterRequest(r)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}

	// Public-client auth method only.
	authMethod := strings.TrimSpace(body.TokenEndpointAuthMethod)
	if authMethod == "" {
		authMethod = "none"
	}
	if authMethod != "none" {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_client_metadata",
			"only public clients are supported (token_endpoint_auth_method must be \"none\")")
		return
	}

	// redirect_uris: required, non-empty, capped, each https or loopback.
	uris, err := validateRegisterRedirectURIs(body.RedirectURIs)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
		return
	}

	grantTypes := body.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}
	responseTypes := body.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}

	clientId, err := identity.NewRandomId("mcp_")
	if err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("register_client_id_mint_failed",
				slog.String("error_id", eid), slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error",
			"could not mint client_id; reference "+eid)
		return
	}

	now := time.Now().UTC()
	if s.Store == nil {
		// No persistence backend wired -- can't honor the registration.
		s.writeJSONError(w, http.StatusServiceUnavailable, "internal_error",
			"registration store is unavailable")
		return
	}
	if err := s.Store.CreateOAuthClient(
		r.Context(),
		clientId,
		strings.TrimSpace(body.ClientName),
		uris,
		strings.Join(grantTypes, " "),
		strings.Join(responseTypes, " "),
		authMethod,
		now.Format(time.RFC3339),
	); err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("register_client_persist_failed",
				slog.String("error_id", eid), slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error",
			"could not persist client; reference "+eid)
		return
	}

	s.audit(r, identity.AuditEvent{
		Category:   identity.AuditCategoryIdentity,
		Action:     "oauth_client_registered",
		TargetType: "identity",
		TargetId:   clientId,
		Outcome:    identity.AuditOutcomeSuccess,
		Detail: map[string]any{
			"client_name":   strings.TrimSpace(body.ClientName),
			"redirect_uris": uris,
			"grant_types":   grantTypes,
		},
	})

	writeJSON(w, http.StatusCreated, registerResponse{
		ClientId:                clientId,
		ClientIdIssuedAt:        now.Unix(),
		RedirectURIs:            uris,
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		TokenEndpointAuthMethod: authMethod,
		ClientName:              strings.TrimSpace(body.ClientName),
	})
}

// readRegisterRequest decodes the JSON body under a bounded read.
func readRegisterRequest(r *http.Request) (*registerRequest, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxRegisterBodyBytes)
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, errors.New("request body too large or unreadable")
	}
	var body registerRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("request body must be valid JSON")
	}
	return &body, nil
}

// validateRegisterRedirectURIs enforces: non-empty, count cap, and that
// every URI is https OR a loopback http URI (127.0.0.1 / localhost /
// ::1). Returns the trimmed list on success.
func validateRegisterRedirectURIs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("redirect_uris is required and must be a non-empty array")
	}
	if len(in) > maxRegisterRedirectURIs {
		return nil, errors.New("too many redirect_uris (max 5)")
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		uri := strings.TrimSpace(raw)
		if uri == "" {
			return nil, errors.New("redirect_uris entries must be non-empty")
		}
		u, err := url.Parse(uri)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, errors.New("redirect_uri is not a valid absolute URI: " + uri)
		}
		switch strings.ToLower(u.Scheme) {
		case "https":
			// always allowed
		case "http":
			if !isLoopbackRegisterHost(u.Hostname()) {
				return nil, errors.New("http redirect_uri is only allowed for loopback hosts (127.0.0.1, localhost, ::1): " + uri)
			}
		default:
			return nil, errors.New("redirect_uri must use https (or http on a loopback host): " + uri)
		}
		out = append(out, uri)
	}
	return out, nil
}

// isLoopbackRegisterHost reports whether host is a loopback host for the
// purpose of the http-redirect exception.
func isLoopbackRegisterHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}
