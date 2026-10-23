// Package auth implements native OAuth authorization with PKCE and a loopback callback.
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// LoginResult holds the tokens returned from a successful login.
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

// Login runs the cockpit's browser-driven authorization flow. Returns
// a LoginResult on success. Times out after 5 minutes if the user
// doesn't complete the magic-link round trip.
func Login(ctx context.Context, issuer, clientId string) (*LoginResult, error) {
	return loginWithBrowser(ctx, issuer, clientId, openBrowser)
}

func loginWithBrowser(ctx context.Context, issuer, clientId string, open func(string) error) (*LoginResult, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return nil, errors.New("auth: issuer is required")
	}
	clientId = strings.TrimSpace(clientId)
	if clientId == "" {
		return nil, errors.New("auth: client_id is required")
	}

	// 127.0.0.1 (not "localhost") so the redirect URL the browser
	// follows is unambiguous regardless of the user's /etc/hosts +
	// IPv6 resolution. Identity registers this exact loopback host and path.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for callback: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/cockpit/callback", port)

	state, err := randomString(32)
	if err != nil {
		return nil, err
	}
	verifier, err := randomString(32)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	authURL := buildLoginURL(issuer, clientId, redirectURL, state, challenge)

	codeCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/cockpit/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid sign-in response", http.StatusBadRequest)
			return
		}
		deliver := func(result callbackResult) {
			select {
			case codeCh <- result:
			default:
			}
		}
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			desc := r.URL.Query().Get("error_description")
			msg := errParam
			if desc != "" {
				msg = errParam + ": " + desc
			}
			deliver(callbackResult{err: errors.New(msg)})
			writeCallbackPage(w, false, "Sign-in failed", msg)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			deliver(callbackResult{err: errors.New("no code in callback")})
			writeCallbackPage(w, false, "Sign-in failed", "The identity service didn't return an authorization code.")
			return
		}
		deliver(callbackResult{code: code})
		writeCallbackPage(w, true, "You're signed in", "memQL Cockpit has been authorized to connect on your behalf.")
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	fmt.Fprintln(os.Stderr, "Complete sign-in in your browser:", authURL)
	if err := open(authURL); err != nil {
		return nil, fmt.Errorf("%w: %v (URL: %s)", ErrNoBrowser, err, authURL)
	}

	select {
	case result := <-codeCh:
		if result.err != nil {
			return nil, fmt.Errorf("auth callback: %w", result.err)
		}
		return exchangeCodeForToken(ctx, issuer, clientId, redirectURL, result.code, verifier)
	case err := <-serveErrCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil, errors.New("callback server closed before receiving the auth code")
		}
		return nil, fmt.Errorf("callback server: %w", err)
	case <-time.After(5 * time.Minute):
		return nil, errors.New("authentication timed out (5 minutes)")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// buildLoginURL composes the URL the browser should hit to start the
// login flow.
func buildLoginURL(issuer, clientId, redirectURL, state, challenge string) string {
	params := url.Values{}
	params.Set("client_id", clientId)
	params.Set("redirect_uri", redirectURL)
	params.Set("response_type", "code")
	params.Set("scope", "openid profile email offline_access")
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	return issuer + "/authorize?" + params.Encode()
}

// tokenResponse mirrors identity's /oauth/token success body
// (component/identity/http/token.go).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// errorResponse mirrors identity's RFC-6749-shaped JSON error body
// (component/identity/http/server.go writeJSONError).
type errorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// exchangeCodeForToken POSTs the auth code to /oauth/token.
func exchangeCodeForToken(ctx context.Context, issuer, clientId, redirectURL, code, verifier string) (*LoginResult, error) {
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"code_verifier": verifier,
		"client_id":     clientId,
		"redirect_uri":  redirectURL,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errBody errorResponse
		if json.Unmarshal(body, &errBody) == nil && errBody.Error != "" {
			return nil, fmt.Errorf("token exchange: HTTP %d: %s: %s", resp.StatusCode, errBody.Error, errBody.ErrorDescription)
		}
		return nil, fmt.Errorf("token exchange: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token exchange: response missing access_token")
	}
	expiry := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		// Defensive: identity always populates ExpiresIn, but if a
		// future change drops it, treat the token as short-lived
		// rather than infinite.
		expiry = time.Now().Add(15 * time.Minute)
	}
	return &LoginResult{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Expiry:       expiry,
	}, nil
}

type callbackResult struct {
	code string
	err  error
}

// ErrInvalidGrant is the sentinel returned by Refresh when identity
// has rejected the cached refresh token (revoked, expired absolute
// lifetime, or rotated). Callers should react by deleting the
// cached credential and falling through to a fresh interactive
// Login -- there's no way to recover the session without user
// re-consent. Anything else (network error, 5xx) is a transient
// failure and should NOT trigger the browser flow on its own.
var ErrInvalidGrant = errors.New("auth: refresh token rejected (invalid_grant)")

// Refresh exchanges a cached refresh token for a new access +
// refresh token pair against identity's /auth/refresh endpoint.
// Returns a LoginResult with the same shape Login emits, so the
// SaveToken / dispatcher-bearer call sites don't have to branch.
//
// The identity service's refresh endpoint reads the token from one
// of three places: the httpOnly memql_refresh cookie (browser-only),
// the JSON body, or the Authorization: Bearer header. We use the
// body so the call works regardless of cookie jar state -- the
// cockpit doesn't ride a browser session.
//
// On a server-side rejection (HTTP 4xx with error="invalid_grant"),
// returns ErrInvalidGrant so the caller can distinguish "user must
// sign in again" from "network blip, try later." Other failures
// (5xx, transport error, unparseable response) come back as wrapped
// errors and should be treated as transient by the caller.
func Refresh(ctx context.Context, issuer, refreshToken string) (*LoginResult, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return nil, errors.New("auth: issuer is required")
	}
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("auth: refresh token is required")
	}

	payload, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return nil, fmt.Errorf("marshal refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/auth/refresh", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// invalid_grant terminal -- session is gone server-side; the
		// caller must sign in again. We surface the sentinel even when
		// the body doesn't parse, because a 401 from /auth/refresh has
		// only one cause in identity's contract: the presented refresh
		// token is no longer valid (see refresh.go switch on
		// ErrSession{NotFound,Revoked,Expired} / ErrTokenMismatch).
		var errBody errorResponse
		_ = json.Unmarshal(body, &errBody) // best-effort
		if errBody.Error == "" {
			errBody.Error = "invalid_grant"
		}
		return nil, fmt.Errorf("%w: %s: %s", ErrInvalidGrant, errBody.Error, errBody.ErrorDescription)
	}
	if resp.StatusCode != http.StatusOK {
		var errBody errorResponse
		if json.Unmarshal(body, &errBody) == nil && errBody.Error != "" {
			return nil, fmt.Errorf("refresh exchange: HTTP %d: %s: %s", resp.StatusCode, errBody.Error, errBody.ErrorDescription)
		}
		return nil, fmt.Errorf("refresh exchange: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("refresh exchange: response missing access_token")
	}
	expiry := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		// Same defensive default Login uses. A future identity build
		// that stops populating ExpiresIn shouldn't translate into an
		// effectively-immortal token in our cache.
		expiry = time.Now().Add(15 * time.Minute)
	}
	// Identity rotates the refresh token on every successful refresh
	// (see http/refresh.go:setRefreshCookie + refresh.Rotator). The
	// response body MAY echo the new value back -- if it does, persist
	// it; if it doesn't (operator chose cookie-only rotation), reuse
	// the caller's existing refresh token so we still have something
	// to present on the next call.
	rotated := tok.RefreshToken
	if rotated == "" {
		rotated = refreshToken
	}
	return &LoginResult{
		AccessToken:  tok.AccessToken,
		RefreshToken: rotated,
		Expiry:       expiry,
	}, nil
}

// randomString generates a random URL-safe string of the given byte
// length. Currently unused by the OAuth flow (identity supplies state)
// but kept as a public helper for future token-id generation.
func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// openBrowser opens the default browser with the given URL.
func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "linux":
		return exec.Command("xdg-open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// writeCallbackPage renders the browser landing page shown after the
// loopback OAuth callback fires -- a small, self-contained (no external
// assets) memQL Cockpit-branded card. ok picks the success vs error
// treatment; title + message are the headline + body. message is
// HTML-escaped since it can carry an upstream error string.
func writeCallbackPage(w http.ResponseWriter, ok bool, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}
	badgeClass, icon := "ok", "&#10003;" // check mark
	if !ok {
		badgeClass, icon = "err", "&#10005;" // multiplication x
	}
	fmt.Fprintf(w, callbackPageHTML,
		html.EscapeString(title), badgeClass, icon,
		html.EscapeString(title), html.EscapeString(message))
}

// callbackPageHTML is the landing-page template. Order of %s:
// <title>, badge class, icon, headline, body. Inline CSS only; dark
// theme tuned to read well in any browser without external fonts.
const callbackPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s &middot; memQL Cockpit</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  html, body { height: 100%%; margin: 0; }
  body {
    display: grid; place-items: center;
    font-family: ui-sans-serif, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: radial-gradient(1100px 560px at 50%% -8%%, #1b2330, #0b0e14);
    color: #e6edf3; padding: 24px;
  }
  .card {
    width: min(92vw, 430px); padding: 40px 36px; text-align: center;
    background: #11161f; border: 1px solid #232b38; border-radius: 16px;
    box-shadow: 0 24px 64px rgba(0,0,0,.5);
  }
  .badge {
    width: 64px; height: 64px; margin: 0 auto 22px; border-radius: 50%%;
    display: grid; place-items: center; font-size: 30px; line-height: 1;
  }
  .badge.ok  { background: rgba(63,185,80,.14); color: #3fb950; }
  .badge.err { background: rgba(248,81,73,.14); color: #f85149; }
  .brand { font-size: 12px; letter-spacing: .16em; text-transform: uppercase; color: #8b949e; margin-bottom: 8px; }
  h1 { font-size: 20px; margin: 0 0 10px; font-weight: 600; }
  p  { font-size: 14px; line-height: 1.55; color: #aab2bd; margin: 0; }
  .hint { margin-top: 24px; font-size: 12px; color: #6e7681; }
</style>
</head>
<body>
  <main class="card">
    <div class="badge %s">%s</div>
    <div class="brand">memQL Cockpit</div>
    <h1>%s</h1>
    <p>%s</p>
    <div class="hint">You can close this tab and return to your terminal.</div>
  </main>
</body>
</html>`
