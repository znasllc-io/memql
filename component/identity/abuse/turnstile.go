package abuse

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// turnstileSiteVerifyURL is Cloudflare's server-side verification
// endpoint. Public, documented at:
//
//	https://developers.cloudflare.com/turnstile/get-started/server-side-validation/
const turnstileSiteVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// TurnstileVerifier holds the secret + an HTTP client used to call
// Cloudflare's siteverify endpoint. Safe for concurrent use.
type TurnstileVerifier struct {
	// Secret is the Cloudflare-issued site secret. When empty, the
	// verifier returns OK=true / Borderline=false unconditionally so
	// dev environments without a configured secret don't block all
	// signups.
	Secret string

	// Logger receives debug + warn log lines. Optional.
	Logger *slog.Logger

	// HTTPClient overrides the default client. Optional.
	HTTPClient *http.Client
}

// TurnstileResult is the verifier's normalized output. OK reflects
// whether the token verified; Borderline reflects Cloudflare's "this
// looked like a bot but we didn't fail it" signal (codes that
// indicate elevated risk without an outright failure). ErrorCodes
// is the raw `error-codes` slice for downstream introspection.
type TurnstileResult struct {
	OK         bool
	Borderline bool
	ErrorCodes []string
}

// turnstileResponse mirrors the JSON shape Cloudflare returns.
type turnstileResponse struct {
	Success     bool      `json:"success"`
	ChallengeTS time.Time `json:"challenge_ts"`
	Hostname    string    `json:"hostname"`
	ErrorCodes  []string  `json:"error-codes"`
	Action      string    `json:"action"`
	CData       string    `json:"cdata"`
}

// Verify exchanges a client-supplied Turnstile token for the
// server-side verification result. When Secret is empty, returns
// {OK:true, Borderline:false} and skips the network call (dev mode).
//
// Returns an error only on transport / JSON-decode failures. A
// `success: false` response from Cloudflare returns a non-error
// result with OK=false and ErrorCodes populated.
//
// remoteIP is forwarded to Cloudflare to improve risk scoring on
// their side; safe to pass an empty string when not known.
func (v *TurnstileVerifier) Verify(ctx context.Context, token, remoteIP string) (TurnstileResult, error) {
	if v == nil || v.Secret == "" {
		return TurnstileResult{OK: true}, nil
	}
	if strings.TrimSpace(token) == "" {
		return TurnstileResult{
			OK:         false,
			ErrorCodes: []string{"missing-input-response"},
		}, nil
	}

	form := url.Values{}
	form.Set("secret", v.Secret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileSiteVerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TurnstileResult{}, fmt.Errorf("turnstile: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := v.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return TurnstileResult{}, fmt.Errorf("turnstile: POST %s: %w", turnstileSiteVerifyURL, err)
	}
	defer resp.Body.Close()

	var decoded turnstileResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return TurnstileResult{}, fmt.Errorf("turnstile: decode response: %w", err)
	}

	result := TurnstileResult{
		OK:         decoded.Success,
		ErrorCodes: decoded.ErrorCodes,
		Borderline: isBorderlineErrorCodes(decoded.ErrorCodes),
	}

	if v.Logger != nil {
		v.Logger.Debug("turnstile_verify",
			slog.Bool("success", decoded.Success),
			slog.String("hostname", decoded.Hostname),
			slog.Any("error_codes", decoded.ErrorCodes),
			slog.String("remote_ip", remoteIP),
		)
	}

	return result, nil
}

// isBorderlineErrorCodes returns true when Cloudflare returned a
// "looks risky but not an outright fail" signal. Today this catches
// the timeout / duplicate-response codes that usually indicate
// elevated bot probability without strictly failing verification.
//
// We deliberately treat truly-failed codes as `OK=false` (the call
// returns a non-OK result), not borderline — borderline is reserved
// for the gray zone the risk scorer should weigh.
func isBorderlineErrorCodes(codes []string) bool {
	for _, c := range codes {
		switch c {
		case "timeout-or-duplicate", "challenge-expired":
			return true
		}
	}
	return false
}
