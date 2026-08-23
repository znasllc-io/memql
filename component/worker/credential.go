package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// credential.go mints the per-run MCP back-channel bearer
// (memql#4360).
//
// WHAT THIS CREDENTIAL IS. A class="service_account" JWT whose `sub`
// is the OWNING USER's id. That subject choice is the whole security
// story of D1: the delegated app reads over MCP as that user, so row
// authz applies to it exactly as it does to their browser -- it sees
// what they could see and nothing more. The service-account
// interceptor pins the surface to read/query and stamps role=system
// rather than owner, so the credential cannot reach a cluster-owner
// gate either.
//
// WHAT IT IS NOT: revocable. The verify path is JWKS-only and DB-free
// by design (that is what lets it work on every node without a
// lookup), so there is no row to strike. Revoking one issued token
// means rotating the signing key, which invalidates every other token
// in the cluster -- not a per-session operation. Three things stand in
// for revocation, and the runbook says so plainly rather than
// implying a revoke exists:
//
//   - the lifetime is short and hard-capped at the identity service;
//   - the cockpit deletes the MCP configuration file at session end,
//     which is where the bearer actually lives;
//   - a run that outlives its credential is handed a REPLACEMENT
//     rather than a longer-lived bearer up front, so the window any
//     one bearer is worth something stays short even for a long run.
//
// The residual exposure is the unexpired remainder of one short-lived,
// user-scoped, read-surface-pinned bearer. That is the trade the
// DB-free verify path buys, and it is stated here so nobody later
// reads "revoked at end" in a design doc and assumes a mechanism.

// CredentialMinter mints and renews app-session back-channel bearers.
type CredentialMinter interface {
	// Mint returns a bearer for the session and its expiry.
	Mint(ctx context.Context, req CredentialRequest) (Credential, error)
}

// CredentialRequest names the session a bearer is for.
type CredentialRequest struct {
	SessionId   string
	OwnerUserId string
	TTL         time.Duration
}

// Credential is a minted back-channel bearer.
type Credential struct {
	Token      string
	ExpiresAt  time.Time
	IdentityId string
}

// Expired reports whether the credential is past its expiry at now.
func (c Credential) Expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt)
}

// RenewBefore is how long before expiry a live session is handed a
// replacement. Generous on purpose: a renewal that lands after the
// bearer died shows up as the app's MCP calls failing mid-run, which
// is far harder to read than an early renewal is to pay for.
const RenewBefore = 10 * time.Minute

// BootstrapCredentialMinter mints through the identity service's
// POST /node/bootstrap endpoint with tokenClass="app_session",
// presenting the same bootstrap secret the node used to mint its own
// class="node" token. It is the only mint surface a non-identity node
// has: the signing key lives on the identity service and nowhere else.
type BootstrapCredentialMinter struct {
	// BaseURL is the identity service's base URL
	// (MEMQL_IDENTITY_VERIFIER_BASE_URL).
	BaseURL string
	// BootstrapToken is MEMQL_NODE_BOOTSTRAP_TOKEN.
	BootstrapToken string
	// HTTPClient is optional; a sane default is used when nil.
	HTTPClient *http.Client
}

// NewBootstrapCredentialMinter builds a minter, or returns nil when
// the node is not configured to mint. A nil minter is what makes an
// app session refuse to start with a NAMED reason rather than start
// and hand the app a blank bearer -- an app with no credential can
// reach nothing, and would report that as "MemQL's tools are broken".
func NewBootstrapCredentialMinter(baseURL, bootstrapToken string) *BootstrapCredentialMinter {
	baseURL = strings.TrimSpace(baseURL)
	bootstrapToken = strings.TrimSpace(bootstrapToken)
	if baseURL == "" || bootstrapToken == "" {
		return nil
	}
	return &BootstrapCredentialMinter{BaseURL: baseURL, BootstrapToken: bootstrapToken}
}

// Mint implements CredentialMinter.
func (m *BootstrapCredentialMinter) Mint(ctx context.Context, req CredentialRequest) (Credential, error) {
	if m == nil {
		return Credential{}, fmt.Errorf("worker: no credential minter configured")
	}
	if strings.TrimSpace(req.SessionId) == "" || strings.TrimSpace(req.OwnerUserId) == "" {
		return Credential{}, fmt.Errorf("worker: credential request needs a session id and an owner")
	}
	payload := map[string]any{
		"tokenClass":  "app_session",
		"sessionId":   req.SessionId,
		"ownerUserId": req.OwnerUserId,
	}
	if req.TTL > 0 {
		payload["ttlSeconds"] = int64(req.TTL / time.Second)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Credential{}, fmt.Errorf("worker: marshal credential request: %w", err)
	}

	url := strings.TrimRight(m.BaseURL, "/") + "/node/bootstrap"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Credential{}, fmt.Errorf("worker: build credential request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bootstrap "+m.BootstrapToken)

	client := m.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Credential{}, fmt.Errorf("worker: mint app-session credential: %w", err)
	}
	defer resp.Body.Close()

	var decoded struct {
		Success    bool   `json:"success"`
		PlainToken string `json:"plainToken"`
		IdentityId string `json:"identityId"`
		ExpiresAt  string `json:"expiresAt"`
		Error      string `json:"error"`
		ErrorCode  string `json:"errorCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return Credential{}, fmt.Errorf("worker: decode credential response (status %d): %w", resp.StatusCode, err)
	}
	if !decoded.Success || decoded.PlainToken == "" {
		reason := decoded.Error
		if reason == "" {
			reason = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return Credential{}, fmt.Errorf("worker: app-session credential refused (%s): %s", decoded.ErrorCode, reason)
	}
	expiresAt, _ := time.Parse(time.RFC3339, decoded.ExpiresAt)
	return Credential{
		Token:      decoded.PlainToken,
		ExpiresAt:  expiresAt,
		IdentityId: decoded.IdentityId,
	}, nil
}
