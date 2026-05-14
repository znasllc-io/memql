// Policy-exposable configuration allow-list.
//
// Cross-cutting decision policies under dsl/v1/policies/{core,bff}/...
// read configured-at-startup values via ctx.config.<key>. The allow-
// list below declares which ConfigSnapshot fields are visible from a
// policy body, and how (raw value for non-sensitive fields; presence
// bool for sensitive fields).
//
// Adding a new key here is a deliberate act: every entry expands
// what product logic the operational config surface can shape, and
// the wider the surface the harder it is to reason about decisions
// after the fact. Prefer the smallest set that unlocks the consumer.
package config

import (
	"strings"

	busv1 "github.com/visionarys-io/memql/component/bus/gen"
)

// PolicyConfigField describes one exposed configuration entry.
type PolicyConfigField struct {
	// Key — the name under ctx.config (e.g. "openaiApiKey",
	// "voiceProvider"). Camel-case; matches the policy authoring
	// convention.
	Key string
	// FieldName — the matching field on busv1.ConfigSnapshot
	// (e.g. "AiOpenaiApiKey").
	FieldName string
	// Sensitive — when true, the ctx surface exposes only a boolean
	// indicating presence (`ctx.config.openaiApiKey == true`); the
	// raw value is never reachable from a policy body. Useful for
	// secrets / API keys / signing material.
	Sensitive bool
	// Description — operator-facing one-liner; surfaced in any
	// future allow-list dump tool.
	Description string
}

// PolicyExposableConfig is the canonical allow-list. Treat additions
// as design changes (one entry per PR). Removals should bump the
// engine's "config schema" debug marker so policies broken by the
// change fail with a clear "unknown config key" at registration.
var PolicyExposableConfig = []PolicyConfigField{
	{
		Key:         "openaiApiKey",
		FieldName:   "AiOpenaiApiKey",
		Sensitive:   true,
		Description: "Presence-only: is the OpenAI API key configured for this node?",
	},
	{
		Key:         "defaultProvider",
		FieldName:   "AiDefaultProvider",
		Sensitive:   false,
		Description: "Default SI provider name (e.g. chat54Mini).",
	},
	{
		Key:         "sttProvider",
		FieldName:   "SttProvider",
		Sensitive:   false,
		Description: "STT provider name (deepgram / openai-realtime / openai-whisper).",
	},
	{
		Key:         "voiceProvider",
		FieldName:   "PolyphonVoiceProvider",
		Sensitive:   false,
		Description: "Voice provider for the /memql/audio path (deepgram / openai / auto).",
	},
	{
		Key:         "authEnabled",
		FieldName:   "AuthEnabled",
		Sensitive:   false,
		Description: "True when IDENTITY_VERIFIER_BASE_URL is set; gates whether bff/voice/etc. enforce JWT verification.",
	},
	{
		Key:         "demoMode",
		FieldName:   "DemoMode",
		Sensitive:   false,
		Description: "Runtime-mutable demo flag; affects webhook behaviour and some agent paths.",
	},
}

// ConfigKeySet returns the set of allowed ctx.config.<key> names.
// Used by the engine's parse-time validator (Phase 8 commit 3) to
// reject policies that reference an unknown config key.
func ConfigKeySet() map[string]struct{} {
	out := make(map[string]struct{}, len(PolicyExposableConfig))
	for _, f := range PolicyExposableConfig {
		out[strings.ToLower(strings.TrimSpace(f.Key))] = struct{}{}
	}
	return out
}

// FieldByKey returns the allow-list entry for the named key, or
// (zero, false) if unknown. Lookup is case-insensitive against the
// canonical key.
func FieldByKey(key string) (PolicyConfigField, bool) {
	target := strings.ToLower(strings.TrimSpace(key))
	for _, f := range PolicyExposableConfig {
		if strings.ToLower(f.Key) == target {
			return f, true
		}
	}
	return PolicyConfigField{}, false
}

// BuildPolicyConfigCtx returns the map that backs ctx.config inside
// a policy body. Sensitive fields expose a bool (presence); non-
// sensitive fields expose their raw string / bool value. Fields that
// are empty on the snapshot still appear as keys — sensitive entries
// resolve to false, non-sensitive entries to the empty value — so
// `ctx.config.openaiApiKey` always exists in the policy ctx.
func BuildPolicyConfigCtx(snapshot *busv1.ConfigSnapshot) map[string]any {
	out := make(map[string]any, len(PolicyExposableConfig))
	if snapshot == nil {
		for _, f := range PolicyExposableConfig {
			if f.Sensitive {
				out[f.Key] = false
			} else {
				out[f.Key] = ""
			}
		}
		return out
	}
	for _, f := range PolicyExposableConfig {
		value := readConfigField(snapshot, f.FieldName)
		if f.Sensitive {
			// Presence-only: collapse to a bool.
			switch v := value.(type) {
			case string:
				out[f.Key] = strings.TrimSpace(v) != ""
			case bool:
				out[f.Key] = v
			default:
				out[f.Key] = value != nil
			}
			continue
		}
		out[f.Key] = value
	}
	return out
}

// readConfigField is the per-field reader. Keeping it as a switch
// (rather than reflection) keeps the allow-list explicit at the
// call site — a typo in FieldName is caught here, not at runtime.
// Extend the switch each time PolicyExposableConfig gains an entry.
func readConfigField(snapshot *busv1.ConfigSnapshot, name string) any {
	if snapshot == nil {
		return nil
	}
	switch name {
	case "AiOpenaiApiKey":
		return snapshot.AiOpenaiApiKey
	case "AiDefaultProvider":
		return snapshot.AiDefaultProvider
	case "SttProvider":
		return snapshot.SttProvider
	case "PolyphonVoiceProvider":
		return snapshot.PolyphonVoiceProvider
	case "AuthEnabled":
		return snapshot.AuthEnabled
	case "DemoMode":
		return snapshot.DemoMode
	}
	return nil
}
