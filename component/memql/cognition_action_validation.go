package memql

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const defaultActionUtteranceMaxPayloadBytes = 64 * 1024

// validateCognitionActionUtterancePayload enforces structured action utterance requirements.
// It is intentionally scoped to v1:cognition:utterance payloads with utteranceType == "action".
//
// Envs:
// - MEMQL_ACTION_UTTERANCE_MAX_BYTES (int, default 65536)
// - MEMQL_ACTION_UTTERANCE_ALLOWLIST_PREFIXES (comma-separated prefixes; if empty, no allowlist)
// - MEMQL_ACTION_UTTERANCE_ALLOWLIST_ENFORCE (true/false; default false)
func validateCognitionActionUtterancePayload(payload map[string]any) error {
	if payload == nil {
		return fmt.Errorf("utterance payload is nil")
	}
	utteranceType := strings.TrimSpace(stringFromAny(payload["utteranceType"]))
	if utteranceType != "action" {
		return nil
	}

	actionAny, ok := payload["action"]
	if !ok || actionAny == nil {
		return fmt.Errorf(`action utterance requires "action" object`)
	}
	action, ok := actionAny.(map[string]any)
	if !ok || action == nil {
		return fmt.Errorf(`"action" must be an object`)
	}

	actionType := strings.TrimSpace(stringFromAny(action["type"]))
	if actionType == "" {
		return fmt.Errorf(`action.type is required`)
	}

	payloadAny, ok := action["payload"]
	if !ok || payloadAny == nil {
		return fmt.Errorf(`action.payload is required`)
	}
	if _, ok := payloadAny.(map[string]any); !ok {
		return fmt.Errorf(`action.payload must be an object`)
	}

	// Validate JSON serializability and enforce size limit
	b, err := json.Marshal(payloadAny)
	if err != nil {
		return fmt.Errorf("action.payload must be JSON-serializable: %w", err)
	}
	maxBytes := actionUtteranceMaxPayloadBytes()
	if len(b) > maxBytes {
		return fmt.Errorf("action.payload too large: %d bytes (max %d bytes)", len(b), maxBytes)
	}

	// Optional allowlist
	prefixes := actionUtteranceAllowlistPrefixes()
	if len(prefixes) > 0 && !hasAnyPrefix(actionType, prefixes) {
		if actionUtteranceAllowlistEnforced() {
			return fmt.Errorf("action.type %q is not allowed", actionType)
		}
	}

	return nil
}

func actionUtteranceMaxPayloadBytes() int {
	raw := strings.TrimSpace(os.Getenv("MEMQL_ACTION_UTTERANCE_MAX_BYTES"))
	if raw == "" {
		return defaultActionUtteranceMaxPayloadBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultActionUtteranceMaxPayloadBytes
	}
	// Safety floor so misconfig doesn't DoS normal usage.
	if n < 1024 {
		return 1024
	}
	return n
}

func actionUtteranceAllowlistPrefixes() []string {
	raw := strings.TrimSpace(os.Getenv("MEMQL_ACTION_UTTERANCE_ALLOWLIST_PREFIXES"))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func actionUtteranceAllowlistEnforced() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_ACTION_UTTERANCE_ALLOWLIST_ENFORCE")))
	return raw == "true" || raw == "1" || raw == "yes" || raw == "on"
}

func hasAnyPrefix(s string, prefixes []string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(prefixes) == 0 {
		return false
	}
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
