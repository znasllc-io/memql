// Package openairealtime exposes OpenAI Realtime ephemeral client-secret
// minting to the MemQL DSL, so the browser can open a DIRECT
// browser<->OpenAI Realtime WebRTC session (the v2 voice path; see the
// frontend repo's docs/openai_agents_sdk/realtime-v2-direct-webrtc-handoff.md)
// WITHOUT ever seeing the standing OpenAI API key.
//
// Capability (callable as a builtin from .memql files / executeNamed):
//
//	realtimeCreateClientSecret({ model?: string, voice?: string })
//	    -> { value, expiresAt, config: { model, voice } }
//
// The browser receives ONLY the short-lived ephemeral secret (`value`),
// never the standing key. The standing OpenAI key is resolved from
// v1:platform:globalSecret under MEMQL_AI_OPENAI_API_KEY (falling back to
// the dev-seeded MEMQL_OPENAI_API_KEY), matching the AI-provider key convention
// documented in component/memql/ai_providers.go.
//
// This mirrors the integrations/avatardirect pattern -- the established way
// the product mints a third-party browser session credential server-side
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
	"os"
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

	// Key resolution order mirrors the AI-provider convention in
	// component/memql/ai_providers.go: the vendor-prefixed name first, then
	// the dev-manifest bare form.
	secretAPIKeyPrimary  = "MEMQL_AI_OPENAI_API_KEY"
	secretAPIKeyFallback = "MEMQL_OPENAI_API_KEY"
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
		i.logger.Error("openairealtime.createClientSecret: api key unavailable", "error", err)
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

	secret, err := i.mint(ctx, apiKey, model, voice)
	if err != nil {
		i.logger.Error("openairealtime.createClientSecret: mint failed", "model", model, "voice", voice, "error", err)
		return nil, fmt.Errorf("openairealtime.createClientSecret: %w", err)
	}
	i.logger.Info("openairealtime.createClientSecret: minted ephemeral key", "model", model, "valueLen", len(secret.value))

	// Shape matches the browser hook's MemqlRealtimeSessionResponse
	// (the frontend's src/hooks/useAgentsSdk.ts): { value, expiresAt, config }.
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
//
// The requested voice is baked into the ephemeral session here (GA shape:
// session.audio.output.voice) so the minted session actually speaks in that
// voice -- the realtime voice can only be set before the first audio output,
// so a caller that wants a different voice (e.g. the frontend's intake
// switching to a male/female voice on the identity answer, frontend#274)
// re-mints with the new voice and reconnects, rather than trying to mutate a
// live session.
func (i *Integration) mint(ctx context.Context, apiKey, model, voice string) (*clientSecret, error) {
	session := map[string]any{
		"type":  "realtime",
		"model": model,
	}
	if voice != "" {
		session["audio"] = map[string]any{
			"output": map[string]any{
				"voice": voice,
			},
		}
	}
	body := map[string]any{
		"session": session,
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
	names := []string{secretAPIKeyPrimary, secretAPIKeyFallback}

	// 1) Encrypted global-secret store (production / seeded installs).
	if i.resolveSecret != nil {
		for _, name := range names {
			v, err := i.resolveSecret(ctx, name)
			if err != nil {
				continue
			}
			if v = strings.TrimSpace(v); v != "" {
				return v, nil
			}
		}
	}

	// 2) OS env fallback (dev): the OpenAI key is commonly provided as an env
	// var (MEMQL_OPENAI_API_KEY / MEMQL_AI_OPENAI_API_KEY) via the genesis envelope
	// rather than seeded into globalSecret. This mirrors the AI-provider /
	// bridge-agent resolution chain (see component/memql/ai_providers.go
	// authConceptLookupNames) so the builtin works wherever those do, without
	// requiring a separate `make secret-set`.
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, nil
		}
	}

	return "", fmt.Errorf(
		"openairealtime: OpenAI API key not found in v1:platform:globalSecret or env (looked for %s then %s) -- seed it with `make secret-set NAME=%s VALUE=... SCOPE=global KIND=integration` or set the env var on the node",
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
