package auth

// The RFC 8628 device-authorization flow -- the sign-in path for
// machines where the loopback-redirect browser flow cannot work: SSH
// sessions, headless fleet boxes, containers. The identity service
// serves POST /device/code and the device_code grant on /oauth/token
// (engine memql#3410); the human approves from a browser on any other
// device.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// ErrNoBrowser marks a Login failure that means "this terminal cannot
// open a browser at all" -- the signal to fall back to DeviceLogin
// rather than surface an error.
var ErrNoBrowser = errors.New("auth: no browser available")

// deviceGrantType is the RFC 8628 grant_type URN the identity
// service's /oauth/token accepts.
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// browserAvailable reports whether this session can plausibly open a
// local browser: macOS and Windows always can; Linux only under a
// display server. SSH sessions on Linux fall out naturally (no
// DISPLAY / WAYLAND_DISPLAY forwarded).
func browserAvailable() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	case "linux":
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	default:
		return false
	}
}

// InteractiveLogin picks the sign-in flow for this terminal: the
// loopback-redirect browser flow where a browser can open, the RFC
// 8628 device flow where one cannot -- and falls back to the device
// flow when the browser flow reports ErrNoBrowser.
func InteractiveLogin(ctx context.Context, issuer, clientId string) (*LoginResult, error) {
	if !browserAvailable() {
		fmt.Fprintln(os.Stderr, "No local browser available; using the device sign-in flow.")
		return DeviceLogin(ctx, issuer, clientId)
	}
	result, err := Login(ctx, issuer, clientId)
	if err != nil && errors.Is(err, ErrNoBrowser) {
		fmt.Fprintln(os.Stderr, "Browser launch failed; using the device sign-in flow.")
		return DeviceLogin(ctx, issuer, clientId)
	}
	return result, err
}

// deviceCodeResponse is POST /device/code's answer (RFC 8628 §3.2).
type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceGrantError is the token endpoint's RFC 8628 §3.5 error body.
// Interval rides along on slow_down, where the server names the new
// polling floor instead of leaving the client to guess.
type deviceGrantError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

// DeviceLogin runs the full device flow: request a code, show the
// human where to go, poll the token endpoint until they answer (or
// the window closes). Returns the same LoginResult shape Login does.
func DeviceLogin(ctx context.Context, issuer, clientId string) (*LoginResult, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return nil, errors.New("auth: issuer is required")
	}
	clientId = strings.TrimSpace(clientId)
	if clientId == "" {
		return nil, errors.New("auth: client_id is required")
	}

	dc, err := requestDeviceCode(ctx, issuer, clientId)
	if err != nil {
		return nil, err
	}

	fmt.Fprintln(os.Stderr, "")
	if dc.VerificationURIComplete != "" {
		fmt.Fprintf(os.Stderr, "  On any device, open:  %s\n", dc.VerificationURIComplete)
		fmt.Fprintf(os.Stderr, "  (or go to %s and enter the code %s)\n", dc.VerificationURI, dc.UserCode)
	} else {
		fmt.Fprintf(os.Stderr, "  On any device, open:  %s\n", dc.VerificationURI)
		fmt.Fprintf(os.Stderr, "  and enter the code:   %s\n", dc.UserCode)
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Waiting for approval...")

	interval := time.Duration(dc.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	window := time.Duration(dc.ExpiresIn) * time.Second
	if window <= 0 {
		window = 10 * time.Minute
	}
	deadline := time.Now().Add(window + 15*time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return nil, errors.New("device sign-in timed out (the code expired)")
		}

		result, retry, newInterval, err := pollDeviceToken(ctx, issuer, clientId, dc.DeviceCode)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
		if newInterval > interval {
			interval = newInterval
		}
		_ = retry // retry==true is the only way to reach here
	}
}

func requestDeviceCode(ctx context.Context, issuer, clientId string) (*deviceCodeResponse, error) {
	payload, err := json.Marshal(map[string]string{"client_id": clientId})
	if err != nil {
		return nil, fmt.Errorf("marshal device-code request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/device/code", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build device-code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device-code request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read device-code response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errBody errorResponse
		if json.Unmarshal(body, &errBody) == nil && errBody.Error != "" {
			return nil, fmt.Errorf("device-code request: HTTP %d: %s: %s", resp.StatusCode, errBody.Error, errBody.ErrorDescription)
		}
		return nil, fmt.Errorf("device-code request: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var dc deviceCodeResponse
	if err := json.Unmarshal(body, &dc); err != nil {
		return nil, fmt.Errorf("decode device-code response: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, errors.New("device-code response missing device_code / user_code")
	}
	return &dc, nil
}

// pollDeviceToken makes one token-endpoint poll. Exactly one of the
// returns is meaningful: a LoginResult on success; retry=true (with an
// optional raised interval) while the human has not answered; an error
// for every terminal outcome.
func pollDeviceToken(ctx context.Context, issuer, clientId, deviceCode string) (*LoginResult, bool, time.Duration, error) {
	payload, err := json.Marshal(map[string]string{
		"grant_type":  deviceGrantType,
		"device_code": deviceCode,
		"client_id":   clientId,
	})
	if err != nil {
		return nil, false, 0, fmt.Errorf("marshal device-token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", bytes.NewReader(payload))
	if err != nil {
		return nil, false, 0, fmt.Errorf("build device-token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Transient transport failure: keep polling inside the window
		// rather than aborting a sign-in the human may be mid-way
		// through approving.
		return nil, true, 0, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, true, 0, nil
	}

	if resp.StatusCode == http.StatusOK {
		var tok tokenResponse
		if err := json.Unmarshal(body, &tok); err != nil {
			return nil, false, 0, fmt.Errorf("decode device-token response: %w", err)
		}
		if tok.AccessToken == "" {
			return nil, false, 0, errors.New("device-token response missing access_token")
		}
		expiry := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		if tok.ExpiresIn <= 0 {
			expiry = time.Now().Add(15 * time.Minute)
		}
		return &LoginResult{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			Expiry:       expiry,
		}, false, 0, nil
	}

	var ge deviceGrantError
	_ = json.Unmarshal(body, &ge)
	switch ge.Error {
	case "authorization_pending":
		return nil, true, 0, nil
	case "slow_down":
		raised := time.Duration(ge.Interval) * time.Second
		if raised <= 0 {
			raised = 10 * time.Second
		}
		return nil, true, raised, nil
	case "access_denied":
		return nil, false, 0, errors.New("sign-in was denied")
	case "expired_token":
		return nil, false, 0, errors.New("the device code expired before approval; run the command again")
	case "":
		return nil, false, 0, fmt.Errorf("device-token poll: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	default:
		return nil, false, 0, fmt.Errorf("device-token poll: %s: %s", ge.Error, ge.ErrorDescription)
	}
}
