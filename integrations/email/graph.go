package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GraphConfig configures the Microsoft Graph email sender.
//
// The client-credentials OAuth flow is used: the app authenticates as
// itself (not a user) with the Entra tenant, requests an access token
// for the Microsoft Graph API, and calls the `/users/{sender}/sendMail`
// endpoint on behalf of `Sender` (the mailbox to send from).
//
// Admin consent for the Graph Mail.Send (Application) permission must
// be granted in Entra ahead of time. Optionally, an Exchange
// ApplicationAccessPolicy should be applied to scope the app to a
// single mailbox (hardening) -- see GUEST_INVITE_SPECS.md.
type GraphConfig struct {
	TenantId     string // e.g. "9e861255-f48b-4000-b00c-81de9d9c203e"
	ClientId     string // Entra app registration's application (client) ID
	ClientSecret string // Raw client secret value (never log)
	SenderAddr   string // Full mailbox address the app sends as, e.g. "no-reply@znas.io"
	FromName     string // Friendly display name, e.g. "Example App"
}

// GraphSender implements Sender against Microsoft Graph sendMail.
// Safe for concurrent use. Tokens are cached under a mutex and
// refreshed ~2 min before expiry.
type GraphSender struct {
	cfg    GraphConfig
	http   *http.Client
	logger *slog.Logger

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// NewGraphSender wires up a GraphSender. Pass nil for httpClient to
// get a default client with a 30s timeout.
func NewGraphSender(cfg GraphConfig, httpClient *http.Client, logger *slog.Logger) *GraphSender {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &GraphSender{cfg: cfg, http: httpClient, logger: logger}
}

// Send delivers msg via Microsoft Graph sendMail. Returns nil on HTTP
// 202 Accepted; otherwise returns an error that includes the response
// status + body for diagnosis.
//
// Retry semantics: on a 401 Unauthorized response (typically
// `InvalidAuthenticationToken: Lifetime validation failed`), Send
// invalidates its cached access token and retries the call once with
// a fresh token. This handles the gap between the cache's local
// expiresAt check and Graph's server-side validation -- Azure can
// revoke a token before its nominal lifetime (clock skew, key
// rotation, app secret rotation) which left the previous
// implementation stuck in a 401 loop until the cached expiresAt
// finally passed naturally. One retry is enough; if the refreshed
// token also 401s the underlying credential is the problem.
func (g *GraphSender) Send(ctx context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(g.cfg.TenantId) == "" ||
		strings.TrimSpace(g.cfg.ClientId) == "" ||
		strings.TrimSpace(g.cfg.ClientSecret) == "" ||
		strings.TrimSpace(g.cfg.SenderAddr) == "" {
		return errors.New("graph: missing required config (TenantId, ClientId, ClientSecret, SenderAddr)")
	}

	// First attempt with whatever's cached.
	status, body, retryAfter, err := g.sendOnce(ctx, msg, false)
	if err != nil {
		return err
	}
	if status == http.StatusAccepted {
		if g.logger != nil {
			g.logger.Info("email sent via graph",
				"to", msg.To,
				"subject", msg.Subject,
				"sender", g.cfg.SenderAddr)
		}
		return nil
	}
	// 401 → cached token is dead from Graph's POV. Force-refresh and
	// retry exactly once.
	if status == http.StatusUnauthorized {
		if g.logger != nil {
			g.logger.Warn("graph: sendMail 401 with cached token; forcing token refresh + retry",
				"to", msg.To,
				"sender", g.cfg.SenderAddr,
			)
		}
		status, body, retryAfter, err = g.sendOnce(ctx, msg, true)
		if err != nil {
			return err
		}
		if status == http.StatusAccepted {
			if g.logger != nil {
				g.logger.Info("email sent via graph (after token refresh)",
					"to", msg.To,
					"subject", msg.Subject,
					"sender", g.cfg.SenderAddr)
			}
			return nil
		}
	}
	// CLASSIFIED rather than flattened to a string (memql#3348). Graph
	// throttles with 429 + Retry-After, and a bulk send that cannot see
	// that header degrades into retries the recipient reads as duplicates.
	// The classification has to happen here because this is the only place
	// the status and the header still exist.
	return classifyHTTPSend(status, retryAfter, fmt.Sprintf("graph: sendMail %d: %s", status, string(body)))
}

// sendOnce performs a single Graph sendMail attempt. When forceRefresh
// is true it invalidates the cached access token before fetching a
// fresh one. Returns the response status, body, and any transport
// error so the caller can decide whether to retry.
func (g *GraphSender) sendOnce(ctx context.Context, msg Message, forceRefresh bool) (int, []byte, string, error) {
	if forceRefresh {
		g.invalidateToken()
	}
	token, err := g.getToken(ctx)
	if err != nil {
		return 0, nil, "", err
	}

	endpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/sendMail",
		url.PathEscape(g.cfg.SenderAddr))

	// TWO REQUEST FORMS, chosen by whether the message carries extra
	// headers (memql#3348).
	//
	// The structured JSON payload below is what transactional mail has
	// always used. It cannot carry `List-Unsubscribe`: Graph exposes custom
	// headers only through `internetMessageHeaders`, which refuses any name
	// not beginning with `x-`. So a message with extras is rendered to RFC
	// 5322 and POSTed base64-encoded with `Content-Type: text/plain`, which
	// is Graph's other documented sendMail form and has no such limit.
	//
	// The structured form is kept for the no-extras case rather than
	// routing everything through MIME, because it is the path every
	// transactional message in this deployment already goes out on and
	// swapping it wholesale would put the guest-invite lane behind an
	// untested encoder for no gain.
	if len(msg.Headers) > 0 {
		return g.sendMIME(ctx, endpoint, token, msg)
	}

	// Graph takes exactly one body contentType. Prefer HTML when the
	// caller supplied both; falls back to Text.
	contentType := "Text"
	content := msg.TextBody
	if strings.TrimSpace(msg.HTMLBody) != "" {
		contentType = "HTML"
		content = msg.HTMLBody
	}

	payload := map[string]any{
		"message": map[string]any{
			"subject": msg.Subject,
			"body": map[string]string{
				"contentType": contentType,
				"content":     content,
			},
			"toRecipients": []map[string]any{
				{"emailAddress": map[string]string{"address": msg.To}},
			},
			"from": map[string]any{
				"emailAddress": map[string]string{
					"address": g.cfg.SenderAddr,
					"name":    g.cfg.FromName,
				},
			},
		},
		// Transactional traffic shouldn't fill the shared mailbox's
		// Sent Items. Operators can flip this by swapping to a
		// per-send option later if audit copies become useful.
		"saveToSentItems": "false",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, "", fmt.Errorf("graph: marshal payload: %w", err)
	}
	return g.post(ctx, endpoint, token, "application/json", body)
}

// sendMIME POSTs the message as a base64-encoded RFC 5322 document --
// Graph's MIME sendMail form. The only path that can carry
// `List-Unsubscribe`; see sendOnce for why.
func (g *GraphSender) sendMIME(ctx context.Context, endpoint, token string, msg Message) (int, []byte, string, error) {
	raw, err := RenderRFC5322(FromHeader(g.cfg.SenderAddr, g.cfg.FromName), msg)
	if err != nil {
		return 0, nil, "", err
	}
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(encoded, raw)
	return g.post(ctx, endpoint, token, "text/plain", encoded)
}

// post issues one authenticated sendMail request and returns the raw
// outcome -- status, body, and the Retry-After header, which the caller
// needs to classify a throttle.
func (g *GraphSender) post(ctx context.Context, endpoint, token, contentType string, body []byte) (int, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", fmt.Errorf("graph: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)

	resp, err := g.http.Do(req)
	if err != nil {
		// A transport error never reached a status, so it cannot be
		// classified as permanent. Returned as an error rather than a
		// SendError because the caller's classifier only sees responses;
		// the campaign worker treats any unclassified error as retryable.
		return 0, nil, "", fmt.Errorf("graph: sendMail network: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, resp.Header.Get("Retry-After"), nil
}

// invalidateToken clears the cached access token. Called by Send on a
// 401 response so the next getToken fetches a fresh one rather than
// re-presenting the dead one. Locked because getToken takes the same
// mutex.
func (g *GraphSender) invalidateToken() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.accessToken = ""
	g.expiresAt = time.Time{}
}

// getToken returns a cached access token when fresh, otherwise fetches
// a new one via the client-credentials grant.
func (g *GraphSender) getToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.accessToken != "" && time.Until(g.expiresAt) > 2*time.Minute {
		return g.accessToken, nil
	}

	form := url.Values{}
	form.Set("client_id", g.cfg.ClientId)
	form.Set("client_secret", g.cfg.ClientSecret)
	form.Set("grant_type", "client_credentials")
	form.Set("scope", "https://graph.microsoft.com/.default")

	endpoint := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token",
		url.PathEscape(g.cfg.TenantId))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("graph: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("graph: token network: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("graph: token %s: %s", resp.Status, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("graph: token parse: %w", err)
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return "", errors.New("graph: token response had empty access_token")
	}

	g.accessToken = tr.AccessToken
	g.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return g.accessToken, nil
}

// GraphEnvKeys names the env vars consulted by NewGraphSenderFromEnv.
type GraphEnvKeys struct {
	TenantId     string
	ClientId     string
	ClientSecret string
	SenderAddr   string
	FromName     string
}

// DefaultGraphEnvKeys returns the canonical names for the email
// integration's Microsoft Graph credentials, prefixed with `EMAIL_`
// so it's obvious at a glance which subsystem owns them. (Bare
// `AZURE_*` would be ambiguous -- Azure could mean storage,
// identity, OpenAI-on-Azure, etc.) The resolver also accepts the
// legacy un-prefixed names (`AZURE_TENANT_ID`, `MAIL_SENDER`, ...)
// during the rename window; see resolveLegacyAlias.
func DefaultGraphEnvKeys() GraphEnvKeys {
	return GraphEnvKeys{
		TenantId:     "MEMQL_EMAIL_AZURE_TENANT_ID",
		ClientId:     "MEMQL_EMAIL_AZURE_CLIENT_ID",
		ClientSecret: "MEMQL_EMAIL_AZURE_CLIENT_SECRET",
		SenderAddr:   "MEMQL_EMAIL_SENDER",
		FromName:     "MEMQL_EMAIL_FROM_NAME",
	}
}

// LegacyGraphEnvKeys returns the pre-rename names. Kept so installs
// that still have the old `AZURE_*` / `MAIL_*` values seeded keep
// working until everyone has re-run `make secrets-seed`. Once the
// last install is migrated this can come out.
func LegacyGraphEnvKeys() GraphEnvKeys {
	return GraphEnvKeys{
		TenantId:     "AZURE_TENANT_ID",
		ClientId:     "AZURE_CLIENT_ID",
		ClientSecret: "AZURE_CLIENT_SECRET",
		SenderAddr:   "MAIL_SENDER",
		FromName:     "MAIL_FROM_NAME",
	}
}
