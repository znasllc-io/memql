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
// itself (not a user) with the Entra tenant that HOSTS THE SENDER MAILBOX,
// requests an access token for the Microsoft Graph API, and calls the
// `/users/{sender}/sendMail` endpoint on behalf of `Sender` (the mailbox to
// send from). Graph resolves {sender} in the tenant the token was issued
// for, so TenantId is the Microsoft 365 tenant the mailbox lives on -- not
// necessarily the tenant the cluster's subscription lives in; a token from
// the wrong tenant is a 404 on a mailbox that exists (memql#4226,
// docs/public/operate/auth/identity-service.md).
//
// Admin consent for the Graph Mail.Send (Application) permission must
// be granted on that tenant ahead of time. Optionally, an Exchange
// ApplicationAccessPolicy should be applied there to scope the app to a
// single mailbox (hardening) -- see GUEST_INVITE_SPECS.md.
type GraphConfig struct {
	TenantId     string // The mailbox tenant's id (a GUID), not the subscription's
	ClientId     string // Entra app registration's application (client) ID
	ClientSecret string // Raw client secret value (never log)
	SenderAddr   string // Full mailbox address the app sends as, e.g. "no-reply@<domain>"
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
//
// # Sending as a non-default identity (design D5)
//
// A non-zero `as` moves THREE things together -- the `/users/{address}`
// path segment, the From address and the From display name -- and they are
// resolved ONCE, in sendOnce, precisely so they cannot move apart. Graph
// resolves the path segment against the token's tenant and stamps the
// envelope sender from it, so a path segment that disagreed with the From
// header would produce a message whose envelope and header name different
// mailboxes: a DMARC alignment failure that arrives as a deliverability
// mystery rather than an error. Whether the credential may in fact send as
// that mailbox is Exchange ApplicationAccessPolicy state we cannot see from
// here; the honest report is Graph's own 403 on the campaign's lastError.
func (g *GraphSender) Send(ctx context.Context, msg Message, as SendAs) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	if err := as.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(g.cfg.TenantId) == "" ||
		strings.TrimSpace(g.cfg.ClientId) == "" ||
		strings.TrimSpace(g.cfg.ClientSecret) == "" ||
		strings.TrimSpace(g.cfg.SenderAddr) == "" {
		return errors.New("graph: missing required config (TenantId, ClientId, ClientSecret, SenderAddr)")
	}

	// Resolved here as well as inside sendOnce, for the log lines only. The
	// wire value is the one sendOnce computes; this is the same fold over the
	// same inputs, so the two cannot disagree, and reporting the DEFAULT
	// mailbox on a send that left from an identity mailbox would make the
	// node log actively misleading about which reputation took the hit.
	sendAsAddr, _ := resolveIdentity(as, g.cfg.SenderAddr, g.cfg.FromName)

	// First attempt with whatever's cached.
	status, body, retryAfter, err := g.sendOnce(ctx, msg, as, false)
	if err != nil {
		return err
	}
	if status == http.StatusAccepted {
		if g.logger != nil {
			g.logger.Info("email sent via graph",
				"to", msg.To,
				"subject", msg.Subject,
				"sender", sendAsAddr)
		}
		return nil
	}
	// 401 → cached token is dead from Graph's POV. Force-refresh and
	// retry exactly once.
	if status == http.StatusUnauthorized {
		if g.logger != nil {
			g.logger.Warn("graph: sendMail 401 with cached token; forcing token refresh + retry",
				"to", msg.To,
				"sender", sendAsAddr,
			)
		}
		status, body, retryAfter, err = g.sendOnce(ctx, msg, as, true)
		if err != nil {
			return err
		}
		if status == http.StatusAccepted {
			if g.logger != nil {
				g.logger.Info("email sent via graph (after token refresh)",
					"to", msg.To,
					"subject", msg.Subject,
					"sender", sendAsAddr)
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
func (g *GraphSender) sendOnce(ctx context.Context, msg Message, as SendAs, forceRefresh bool) (int, []byte, string, error) {
	if forceRefresh {
		g.invalidateToken()
	}
	token, err := g.getToken(ctx)
	if err != nil {
		return 0, nil, "", err
	}

	// THE resolution, once, at the top -- the three places the identity is
	// spent below all read these two variables and never g.cfg again. See
	// Send's comment for why a drift between them is a silent deliverability
	// failure rather than an error.
	sendAsAddr, fromName := resolveIdentity(as, g.cfg.SenderAddr, g.cfg.FromName)

	endpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/sendMail",
		url.PathEscape(sendAsAddr))

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
		return g.sendMIME(ctx, endpoint, token, msg, sendAsAddr, fromName)
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
					"address": sendAsAddr,
					"name":    fromName,
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
//
// The identity arrives ALREADY RESOLVED rather than being re-derived here.
// This is the third of the three places one send spends it, and the one
// where a re-derivation would be least visible: the endpoint was built from
// the resolved address several lines up, so a locally-recomputed From here
// could disagree with the path segment and nothing would say so.
func (g *GraphSender) sendMIME(ctx context.Context, endpoint, token string, msg Message, sendAsAddr, fromName string) (int, []byte, string, error) {
	raw, err := RenderRFC5322(FromHeader(sendAsAddr, fromName), msg)
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
	status, respBody, retryAfter, err := g.do(ctx, http.MethodPost, endpoint, token, contentType, body)
	if err != nil {
		// A transport error never reached a status, so it cannot be
		// classified as permanent. Returned as an error rather than a
		// SendError because the caller's classifier only sees responses;
		// the campaign worker treats any unclassified error as retryable.
		return 0, nil, "", fmt.Errorf("graph: sendMail network: %w", err)
	}
	return status, respBody, retryAfter, nil
}

// get issues one authenticated Graph READ (memql#4824). The NDR poller's
// half of the credential: the same app registration, the same token cache,
// the same client, pointed at the mailbox instead of at sendMail.
//
// It returns the status rather than judging it, for the same reason post
// does: the status and the Retry-After header are the only place a throttle
// is visible, and Graph throttles a mailbox READ exactly as it throttles a
// send. classifyHTTPSend is what the caller runs on the pair, so a 429 while
// listing the inbox backs off instead of hammering.
func (g *GraphSender) get(ctx context.Context, endpoint, token string) (int, []byte, error) {
	status, body, retryAfter, err := g.do(ctx, http.MethodGet, endpoint, token, "", nil)
	if err != nil {
		return 0, nil, fmt.Errorf("graph: read network: %w", err)
	}
	if status/100 != 2 {
		return status, body, classifyHTTPSend(status, retryAfter,
			fmt.Sprintf("graph: GET %d: %s", status, string(body)))
	}
	return status, body, nil
}

// patch issues one authenticated Graph WRITE against a mailbox resource --
// used only to stamp a processed message read and categorized. Same
// classification story as get.
func (g *GraphSender) patch(ctx context.Context, endpoint, token string, body []byte) (int, []byte, error) {
	status, respBody, retryAfter, err := g.do(ctx, http.MethodPatch, endpoint, token, "application/json", body)
	if err != nil {
		return 0, nil, fmt.Errorf("graph: patch network: %w", err)
	}
	if status/100 != 2 {
		return status, respBody, classifyHTTPSend(status, retryAfter,
			fmt.Sprintf("graph: PATCH %d: %s", status, string(respBody)))
	}
	return status, respBody, nil
}

// do is the one authenticated Graph round trip every verb above goes
// through. Extracted rather than copied when the reader arrived
// (memql#4824): the bearer header, the client and the Retry-After read are
// the three things every Graph call needs identically, and three copies of
// them is three places to forget one.
func (g *GraphSender) do(ctx context.Context, method, endpoint, token, contentType string, body []byte) (int, []byte, string, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, "", fmt.Errorf("graph: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := g.http.Do(req)
	if err != nil {
		return 0, nil, "", err
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
// working until everyone has re-run `go run ./scripts/secrets seed`. Once the
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
