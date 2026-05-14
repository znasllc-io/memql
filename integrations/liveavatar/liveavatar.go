// Package liveavatar exposes LiveAvatar session management to the
// MemQL DSL. Replaces the standalone Express controller previously
// hosted in CoPresent's server tier; the API key now lives in
// v1:platform:globalSecret instead of an env var that has to be present in
// the frontend's process.
//
// Capabilities (callable as builtins from .memql files):
//
//	liveAvatarToken({ is_sandbox?: bool, avatar_id?: string })
//	    -> { session_id, session_token }
//	liveAvatarStart({ session_token: string })
//	    -> { livekit_url, livekit_client_token, ... }
//	liveAvatarStop({ session_token: string })
//	    -> { ok }
//
// Auth + config resolution order:
//
//	LIVEAVATAR_API_KEY    v1:platform:globalSecret  (required)
//	LIVEAVATAR_AVATAR_ID  v1:platform:globalVariable (optional, defaults to
//	                                            sandbox Wayne)
//	LIVEAVATAR_VOICE_ID   v1:platform:globalVariable (optional; if unset the
//	                                            integration fetches the
//	                                            first voice from the
//	                                            LiveAvatar voices API
//	                                            and caches it for the
//	                                            life of the process)
package liveavatar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql"
)

const (
	apiBase            = "https://api.liveavatar.com/v1"
	defaultRequestTime = 15 * time.Second

	secretAPIKey         = "LIVEAVATAR_API_KEY"
	variableAvatarId     = "LIVEAVATAR_AVATAR_ID"
	variableVoiceId      = "LIVEAVATAR_VOICE_ID"
	sandboxAvatarWayneId = "dd73ea75-1218-4ef3-92ce-606d5f7fbc0a"
)

// Integration implements memql.IntegrationProvider for LiveAvatar.
// It is registered as a plug-in (init() in plugin.go) so every node
// type that loads the core plug-ins gets it.
type Integration struct {
	resolveSecret   func(ctx context.Context, name string) (string, error)
	resolveVariable func(ctx context.Context, name string) (string, error)
	logger          *slog.Logger
	httpClient      *http.Client

	voiceMu          sync.Mutex
	cachedVoiceId    string
	cachedVoiceSeen  bool
}

// New constructs a LiveAvatar integration. The secret/variable
// resolvers are usually engine.ResolveSystemSecret /
// engine.ResolveSystemVariable.
func New(
	resolveSecret func(ctx context.Context, name string) (string, error),
	resolveVariable func(ctx context.Context, name string) (string, error),
	logger *slog.Logger,
) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		resolveSecret:   resolveSecret,
		resolveVariable: resolveVariable,
		logger:          logger,
		httpClient:      &http.Client{Timeout: defaultRequestTime},
	}
}

func (i *Integration) IntegrationName() string { return "liveavatar" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "createSessionToken",
			Description: "Create a LiveAvatar session token in FULL mode. Returns { session_id, session_token }. The avatar_id defaults to LIVEAVATAR_AVATAR_ID (or the sandbox Wayne avatar in sandbox mode).",
			Handler:     i.handleCreateToken,
			ArgsSchema: map[string]string{
				"is_sandbox": "bool (optional) - use sandbox mode (sandbox Wayne avatar)",
				"avatar_id":  "string (optional) - override the configured avatar id",
			},
		},
		{
			Name:        "startSession",
			Description: "Start a LiveAvatar session. Returns LiveKit room credentials (livekit_url, livekit_client_token, ...).",
			Handler:     i.handleStartSession,
			ArgsSchema: map[string]string{
				"session_token": "string (required) - token returned by createSessionToken",
			},
		},
		{
			Name:        "stopSession",
			Description: "Stop a LiveAvatar session.",
			Handler:     i.handleStopSession,
			ArgsSchema: map[string]string{
				"session_token": "string (required)",
			},
		},
	}
}

// -----------------------------------------------------------------
// Capability handlers
// -----------------------------------------------------------------

func (i *Integration) handleCreateToken(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	apiKey, err := i.apiKey(ctx)
	if err != nil {
		return nil, err
	}

	isSandbox, _ := args["is_sandbox"].(bool)
	avatarId := strings.TrimSpace(asString(args["avatar_id"]))
	if avatarId == "" {
		avatarId = i.configuredAvatarId(ctx, isSandbox)
	}
	if isSandbox {
		avatarId = sandboxAvatarWayneId
	}

	voiceId, _ := i.resolveVoiceId(ctx, apiKey)

	avatarPersona := map[string]any{"language": "en"}
	if voiceId != "" {
		avatarPersona["voice_id"] = voiceId
	}

	payload := map[string]any{
		"mode":           "FULL",
		"avatar_id":      avatarId,
		"avatar_persona": avatarPersona,
	}
	if isSandbox {
		payload["is_sandbox"] = true
	}

	resp, err := i.callAPI(ctx, http.MethodPost, "/sessions/token", apiKey, payload, false)
	if err != nil {
		return nil, fmt.Errorf("liveavatar.createSessionToken: %w", err)
	}

	inner := unwrapData(resp)
	sessionId := strOr(inner["session_id"], strOr(resp["session_id"], ""))
	sessionToken := strOr(inner["session_token"], strOr(inner["token"], strOr(resp["session_token"], strOr(resp["token"], ""))))
	if sessionToken == "" {
		return nil, fmt.Errorf("liveavatar.createSessionToken: API response missing session_token")
	}

	out := map[string]any{
		"session_id":    sessionId,
		"session_token": sessionToken,
	}
	return wrapResult("liveavatar:session-token", "integration:liveavatar:sessionToken", out)
}

func (i *Integration) handleStartSession(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	token := strings.TrimSpace(asString(args["session_token"]))
	if token == "" {
		return nil, fmt.Errorf("liveavatar.startSession: session_token is required")
	}

	resp, err := i.callAPIBearer(ctx, http.MethodPost, "/sessions/start", token, nil)
	if err != nil {
		return nil, fmt.Errorf("liveavatar.startSession: %w", err)
	}
	inner := unwrapData(resp)
	return wrapResult("liveavatar:session-start", "integration:liveavatar:sessionStart", inner)
}

func (i *Integration) handleStopSession(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	token := strings.TrimSpace(asString(args["session_token"]))
	if token == "" {
		return nil, fmt.Errorf("liveavatar.stopSession: session_token is required")
	}

	if _, err := i.callAPIBearer(ctx, http.MethodPost, "/sessions/stop", token, nil); err != nil {
		// Stop is best-effort; log but don't surface a hard error so
		// the frontend's clean-up path can complete idempotently.
		i.logger.Warn("liveavatar.stopSession failed (logged, returning ok)", "error", err)
	}
	return wrapResult("liveavatar:session-stop", "integration:liveavatar:sessionStop", map[string]any{"ok": true})
}

// -----------------------------------------------------------------
// Config resolution
// -----------------------------------------------------------------

func (i *Integration) apiKey(ctx context.Context) (string, error) {
	if i.resolveSecret == nil {
		return "", fmt.Errorf("liveavatar: no secret resolver configured")
	}
	v, err := i.resolveSecret(ctx, secretAPIKey)
	if err != nil {
		return "", fmt.Errorf("liveavatar: %s not found in v1:platform:globalSecret -- run `make secret-set NAME=%s VALUE=... SCOPE=global KIND=integration` (root cause: %v)", secretAPIKey, secretAPIKey, err)
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("liveavatar: %s is empty", secretAPIKey)
	}
	return v, nil
}

func (i *Integration) configuredAvatarId(ctx context.Context, isSandbox bool) string {
	if i.resolveVariable != nil {
		if v, err := i.resolveVariable(ctx, variableAvatarId); err == nil {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed
			}
		}
	}
	return sandboxAvatarWayneId
}

func (i *Integration) resolveVoiceId(ctx context.Context, apiKey string) (string, bool) {
	// Configured override wins.
	if i.resolveVariable != nil {
		if v, err := i.resolveVariable(ctx, variableVoiceId); err == nil {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed, true
			}
		}
	}
	// Fall back to the cached voice id from the voices API.
	i.voiceMu.Lock()
	if i.cachedVoiceSeen {
		v := i.cachedVoiceId
		i.voiceMu.Unlock()
		return v, v != ""
	}
	i.voiceMu.Unlock()

	voiceId := i.fetchDefaultVoiceId(ctx, apiKey)

	i.voiceMu.Lock()
	i.cachedVoiceId = voiceId
	i.cachedVoiceSeen = true
	i.voiceMu.Unlock()
	return voiceId, voiceId != ""
}

func (i *Integration) fetchDefaultVoiceId(ctx context.Context, apiKey string) string {
	resp, err := i.callAPI(ctx, http.MethodGet, "/voices", apiKey, nil, true)
	if err != nil {
		i.logger.Warn("liveavatar: voices fetch failed", "error", err)
		return ""
	}
	// Response shape varies: { data: [{voice_id|id, name}] } or { voices: [...] } or [...].
	candidates := []any{resp["data"], resp["voices"]}
	for _, raw := range candidates {
		if list, ok := raw.([]any); ok && len(list) > 0 {
			if first, ok := list[0].(map[string]any); ok {
				if id := strOr(first["voice_id"], strOr(first["id"], "")); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

// -----------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------

func (i *Integration) callAPI(ctx context.Context, method, path, apiKey string, body any, allowEmptyBody bool) (map[string]any, error) {
	return i.callAPIWithAuth(ctx, method, path, body, allowEmptyBody, func(req *http.Request) {
		req.Header.Set("X-API-KEY", apiKey)
	})
}

func (i *Integration) callAPIBearer(ctx context.Context, method, path, bearer string, body any) (map[string]any, error) {
	return i.callAPIWithAuth(ctx, method, path, body, true, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+bearer)
	})
}

func (i *Integration) callAPIWithAuth(ctx context.Context, method, path string, body any, allowEmptyBody bool, applyAuth func(*http.Request)) (map[string]any, error) {
	url := apiBase + path
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	applyAuth(req)

	resp, err := i.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if len(raw) == 0 {
		if allowEmptyBody {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("%s %s returned empty body", method, path)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s %s: parse json: %w", method, path, err)
	}
	return out, nil
}

// -----------------------------------------------------------------
// helpers
// -----------------------------------------------------------------

// unwrapData strips the LiveAvatar API's standard `{ code, data,
// message }` envelope so callers operate on the inner payload.
// Returns the original map when no `data` key is present.
func unwrapData(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	if inner, ok := m["data"].(map[string]any); ok {
		return inner
	}
	return m
}

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
