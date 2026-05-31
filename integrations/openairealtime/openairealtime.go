// Package openairealtime exposes OpenAI Realtime ephemeral client-secret
// minting to the MemQL DSL, so the browser can open a DIRECT
// browser<->OpenAI Realtime WebRTC session (the v3 voice path; see
// copresent docs/openai_agents_sdk/realtime-v3-direct-webrtc-handoff.md)
// WITHOUT ever seeing the standing OpenAI API key.
//
// Capability (callable as a builtin from .memql files / executeNamed):
//
//	realtimeCreateClientSecret({ model?: string, voice?: string })
//	    -> { value, expiresAt, config: { model, voice } }
//
// The browser receives ONLY the short-lived ephemeral secret (`value`),
// never the standing key. The standing OpenAI key is resolved from
// v1:platform:globalSecret under MEMQL_SI_OPENAI_API_KEY (falling back to
// the dev-seeded OPENAI_API_KEY), matching the SI-provider key convention
// documented in component/memql/si_providers.go.
//
// This mirrors the integrations/liveavatar pattern -- the established way
// CoPresent mints a third-party browser session credential server-side
// instead of through a frontend-tier Express route (gRPC-first; see the
// #147 architecture spike).
package openairealtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	// defaultAPIBase is OpenAI's REST base. Overridable on the Integration
	// (apiBase) so unit tests can point at an httptest server.
	defaultAPIBase = "https://api.openai.com/v1"
	// clientSecretPath is OpenAI's GA ephemeral-key mint endpoint. The
	// pre-GA endpoint was /realtime/sessions (returned client_secret.value);
	// mint() tolerates both response shapes.
	clientSecretPath = "/realtime/client_secrets"

	defaultRequestTime = 15 * time.Second
	defaultModel       = "gpt-realtime-2"
	defaultVoice       = "marin"

	// Key resolution order mirrors the SI-provider convention in
	// component/memql/si_providers.go: the vendor-prefixed name first, then
	// the dev-manifest bare form.
	secretAPIKeyPrimary  = "MEMQL_SI_OPENAI_API_KEY"
	secretAPIKeyFallback = "OPENAI_API_KEY"
)

// Integration implements memql.IntegrationProvider for OpenAI Realtime
// ephemeral-key minting. Registered as a plug-in (init() in plugin.go) so
// every node type that loads the core plug-ins gets it.
type Integration struct {
	resolveSecret func(ctx context.Context, name string) (string, error)
	logger        *slog.Logger
	httpClient    *http.Client
	apiBase       string
}

// New constructs the integration. resolveSecret is usually
// engine.ResolveSystemSecret. The OpenAI key is resolved lazily at request
// time, so the factory succeeds even on a fresh install -- the "key not
// configured" surface error fires only when a caller invokes the capability.
func New(
	resolveSecret func(ctx context.Context, name string) (string, error),
	logger *slog.Logger,
) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		resolveSecret: resolveSecret,
		logger:        logger,
		httpClient:    &http.Client{Timeout: defaultRequestTime},
		apiBase:       defaultAPIBase,
	}
}

func (i *Integration) IntegrationName() string { return "openairealtime" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "createClientSecret",
			Description: "Mint a short-lived OpenAI Realtime ephemeral client_secret for a direct browser<->OpenAI WebRTC session. Returns { value, expiresAt, config }. Never exposes the standing OpenAI key.",
			Handler:     i.handleCreateClientSecret,
			ArgsSchema: map[string]string{
				"model": "string (optional) - realtime model id (default gpt-realtime-2)",
				"voice": "string (optional) - realtime voice (default marin)",
			},
		},
	}
}

// -----------------------------------------------------------------
// Capability handler
// -----------------------------------------------------------------

func (i *Integration) handleCreateClientSecret(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	apiKey, err := i.apiKey(ctx)
	if err != nil {
		return nil, err
	}

	model := strings.TrimSpace(asString(args["model"]))
	if model == "" {
		model = defaultModel
	}
	voice := strings.TrimSpace(asString(args["voice"]))
	if voice == "" {
		voice = defaultVoice
	}

	secret, err := i.mint(ctx, apiKey, model)
	if err != nil {
		return nil, fmt.Errorf("openairealtime.createClientSecret: %w", err)
	}

	// Shape matches the browser hook's MemqlRealtimeSessionResponse
	// (copresent src/hooks/useAgentsSdk.ts): { value, expiresAt, config }.
	out := map[string]any{
		"value":     secret.value,
		"expiresAt": secret.expiresAt,
		"config": map[string]any{
			"model": model,
			"voice": voice,
		},
	}
	return wrapResult("openairealtime:client-secret", "integration:openairealtime:clientSecret", out)
}

// -----------------------------------------------------------------
// OpenAI mint
// -----------------------------------------------------------------

type clientSecret struct {
	value     string
	expiresAt any
}

// mint POSTs OpenAI's GA ephemeral-key endpoint with the standing key and
// returns the short-lived secret. GA returns the secret at top-level
// `value`; the pre-GA /realtime/sessions endpoint nested it under
// `client_secret.value`, so we tolerate both.
func (i *Integration) mint(ctx context.Context, apiKey, model string) (*clientSecret, error) {
	body := map[string]any{
		"session": map[string]any{
			"type":  "realtime",
			"model": model,
		},
	}
	resp, err := i.postBearer(ctx, clientSecretPath, apiKey, body)
	if err != nil {
		return nil, err
	}

	value := strOr(resp["value"], "")
	var expiresAt any = resp["expires_at"]
	if value == "" {
		if cs, ok := resp["client_secret"].(map[string]any); ok {
			value = strOr(cs["value"], "")
			if expiresAt == nil {
				expiresAt = cs["expires_at"]
			}
		}
	}
	if value == "" {
		return nil, fmt.Errorf("mint response missing client secret value")
	}
	return &clientSecret{value: value, expiresAt: expiresAt}, nil
}

// -----------------------------------------------------------------
// Config resolution
// -----------------------------------------------------------------

func (i *Integration) apiKey(ctx context.Context) (string, error) {
	if i.resolveSecret == nil {
		return "", fmt.Errorf("openairealtime: no secret resolver configured")
	}
	for _, name := range []string{secretAPIKeyPrimary, secretAPIKeyFallback} {
		v, err := i.resolveSecret(ctx, name)
		if err != nil {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf(
		"openairealtime: OpenAI API key not found in v1:platform:globalSecret (looked for %s then %s) -- run `make secret-set NAME=%s VALUE=... SCOPE=global KIND=integration`",
		secretAPIKeyPrimary, secretAPIKeyFallback, secretAPIKeyPrimary)
}

// -----------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------

func (i *Integration) postBearer(ctx context.Context, path, bearer string, body any) (map[string]any, error) {
	url := i.apiBase + path
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := i.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respRaw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s -> %d: %s", path, resp.StatusCode, strings.TrimSpace(string(respRaw)))
	}
	if len(respRaw) == 0 {
		return nil, fmt.Errorf("POST %s returned empty body", path)
	}
	var out map[string]any
	if err := json.Unmarshal(respRaw, &out); err != nil {
		return nil, fmt.Errorf("POST %s: parse json: %w", path, err)
	}
	return out, nil
}

// -----------------------------------------------------------------
// helpers
// -----------------------------------------------------------------

func wrapResult(idPrefix, concept string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, _ := json.Marshal(payload)
	return []memorynodes.MemoryNode{{
		ID:      fmt.Sprintf("%s:%d", idPrefix, time.Now().UnixNano()),
		Concept: concept,
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
	}}, nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func strOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
