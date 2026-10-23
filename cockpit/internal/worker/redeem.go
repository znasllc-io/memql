package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity/workerpairing"
)

// RedeemResult captures the credentials returned by a successful
// pairing-code redemption.
type RedeemResult struct {
	PlainToken  string
	IdentityId  string
	OwnerUserId string
	ClusterURL  string
}

// RedeemPairingCode posts the supplied code to the identity service's
// `POST /pair/redeem` endpoint with `Authorization: Pair <code>` and
// returns the resulting worker token + canonical gRPC dial address.
//
// identityURL is the public origin of the memQL identity service
// (e.g. http://localhost:8081 or https://identity.acme.com). The
// cockpit captures this at `memql cluster add <domain>` time and
// stores it as ClusterConfig.Issuer in ~/.memql/clusters.yaml.
func RedeemPairingCode(ctx context.Context, identityURL, plainCode string, workerName string, logger *slog.Logger) (*RedeemResult, error) {
	canonical := workerpairing.Canonicalize(plainCode)
	if canonical == "" {
		return nil, errors.New("redeem: code must be 8 chars from the pairing alphabet (e.g. ABCD-EFGH)")
	}

	endpoint := strings.TrimRight(strings.TrimSpace(identityURL), "/")
	if endpoint == "" {
		return nil, errors.New("redeem: identity URL is empty")
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("redeem: parse identity URL: %w", err)
	}

	hostname, _ := os.Hostname()
	if workerName == "" {
		workerName = hostname
	}

	body := map[string]string{"workerName": workerName}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("redeem: marshal body: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint+"/pair/redeem", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("redeem: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Pair "+canonical)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("redeem: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("redeem: read response: %w", err)
	}

	var parsed struct {
		Success     bool   `json:"success"`
		PlainToken  string `json:"plainToken"`
		IdentityId  string `json:"identityId"`
		OwnerUserId string `json:"ownerUserId"`
		ClusterURL  string `json:"clusterUrl"`
		ErrorCode   string `json:"errorCode"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("redeem: parse response (status=%d, body=%q): %w", resp.StatusCode, truncate(respBytes, 200), err)
	}

	if resp.StatusCode != http.StatusOK || !parsed.Success {
		code := parsed.ErrorCode
		if code == "" {
			code = http.StatusText(resp.StatusCode)
		}
		msg := parsed.Error
		if msg == "" {
			msg = string(truncate(respBytes, 200))
		}
		if logger != nil {
			logger.Warn("redeem rejected", "status", resp.StatusCode, "code", code, "message", msg)
		}
		return nil, fmt.Errorf("redeem: %s -- %s", code, msg)
	}

	clusterURL := strings.TrimSpace(parsed.ClusterURL)
	if clusterURL == "" {
		return nil, errors.New("redeem: server returned empty cluster URL")
	}

	return &RedeemResult{
		PlainToken:  parsed.PlainToken,
		IdentityId:  parsed.IdentityId,
		OwnerUserId: parsed.OwnerUserId,
		ClusterURL:  clusterURL,
	}, nil
}

// truncate clips a byte slice for inclusion in error messages without
// dragging an enormous response body into a single log line.
func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append(b[:n:n], '.', '.', '.')
}
