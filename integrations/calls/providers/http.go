package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxResponseBytes = 1 << 20

// Rejection bodies stay private: they may contain provider identifiers or PII.
// A payment adapter may inspect a bounded error object to retain its intent ID.
type rejectedResponse struct {
	status int
	body   []byte
}

func (e *rejectedResponse) Error() string {
	return fmt.Sprintf("provider request rejected (HTTP %d)", e.status)
}

type providerHTTP struct {
	mode   Mode
	client *http.Client
}

func newHTTP(mode Mode, client *http.Client) (providerHTTP, error) {
	if mode == "" {
		mode = Disabled
	}
	if mode != Disabled && mode != Mock && mode != Sandbox {
		return providerHTTP{}, ErrWrongEnvironment
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	copyClient := *client
	// A provider redirect must never leak its server credential to a new origin.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copyClient.Timeout == 0 {
		copyClient.Timeout = 15 * time.Second
	}
	return providerHTTP{mode: mode, client: &copyClient}, nil
}
func (p providerHTTP) enabled() error {
	switch p.mode {
	case Sandbox:
		return nil
	case Mock:
		return ErrMockOperation
	default:
		return ErrDisabled
	}
}
func (p providerHTTP) request(ctx context.Context, method, endpoint string, body io.Reader, headers http.Header, out any) (int, error) {
	if err := p.enabled(); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, errors.New("invalid provider request")
	}
	req.Header = headers
	resp, err := p.client.Do(req)
	if err != nil {
		if method == http.MethodPost {
			return 0, ErrOutcomeUnknown
		}
		return 0, errors.New("provider read unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		if method == http.MethodPost {
			return resp.StatusCode, ErrOutcomeUnknown
		}
		return resp.StatusCode, errors.New("invalid provider response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A timed-out or server-failed mutation may already have succeeded. Never
		// turn that ambiguity into an invitation to release its reservation.
		if method == http.MethodPost && (resp.StatusCode >= 500 || resp.StatusCode == 408) {
			return resp.StatusCode, ErrOutcomeUnknown
		}
		return resp.StatusCode, &rejectedResponse{status: resp.StatusCode, body: data}
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		if method == http.MethodPost {
			return resp.StatusCode, ErrOutcomeUnknown
		}
		return resp.StatusCode, errors.New("invalid provider response")
	}
	return resp.StatusCode, nil
}
func validReturnURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "[::1]" || u.Hostname() == "::1"))
}
func validIdentifier(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-:", r)) {
			return false
		}
	}
	return true
}
