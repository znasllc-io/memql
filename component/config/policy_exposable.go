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
	"fmt"
	"sort"
	"strings"

	busv1 "github.com/znasllc-io/memql/component/bus/gen"
)

// PolicyConfigField describes one exposed configuration entry.
type PolicyConfigField struct {
	// Key — the name under ctx.config (e.g. "openaiApiKey",
	// "sttProvider"). Camel-case; matches the policy authoring
	// convention.
	Key string
	// FieldName — the matching field on busv1.ConfigSnapshot
	// (e.g. "SiDefaultProvider").
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
		Key:         "defaultProvider",
		FieldName:   "SiDefaultProvider",
		Sensitive:   false,
		Description: "Default AI provider name (e.g. chat54Mini).",
	},
	{
		Key:         "sttProvider",
		FieldName:   "SttProvider",
		Sensitive:   false,
		Description: "STT provider name (openai-realtime / openai-whisper).",
	},
	{
		Key:         "authEnabled",
		FieldName:   "AuthEnabled",
		Sensitive:   false,
		Description: "True when MEMQL_IDENTITY_VERIFIER_BASE_URL is set; gates whether bff/agent/etc. enforce JWT verification.",
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

// UnknownKey reports whether `config.<key>` names no entry of the
// allow-list, with the sentence a load-time refusal of the read prints:
// what the keys are and where one is added. A key is matched exactly as
// the allow-list spells it -- the config envelope is keyed by it, so
// another casing reads absent too (FieldByKey's case-insensitive match
// answers a different question).
func UnknownKey(key string) (string, bool) {
	keys := make([]string, 0, len(PolicyExposableConfig))
	for _, f := range PolicyExposableConfig {
		if f.Key == key {
			return "", false
		}
		keys = append(keys, f.Key)
	}
	sort.Strings(keys)
	return fmt.Sprintf("`config.%s` names no key of the config allow-list (%s), so it reads absent at run time: "+
		"read one of those, or add the key to component/config's PolicyExposableConfig", key, strings.Join(keys, ", ")), true
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
		if f.Sensitive {
			// Presence-only, and STRUCTURALLY so (memql#3188): a
			// sensitive field is read through a reader that returns
			// bool, so its raw value has no path into this map at
			// all. It used to be read through readConfigField's
			// `any` return and collapsed here by a runtime type
			// switch -- correct, but only by coincidence of dynamic
			// type. See readSensitivePresence for why that
			// distinction was worth 492 CodeQL alerts.
			out[f.Key] = readSensitivePresence(snapshot, f.FieldName)
			continue
		}
		out[f.Key] = readConfigField(snapshot, f.FieldName)
	}
	return out
}

// readSensitivePresence is the reader for Sensitive allow-list entries.
// It returns whether the field is configured -- never the value.
//
// WHY IT IS A SEPARATE FUNCTION RETURNING bool (memql#3188). The presence
// collapse used to live in BuildPolicyConfigCtx as a type switch over
// readConfigField's `any` return:
//
//	switch v := value.(type) {
//	case string: out[f.Key] = strings.TrimSpace(v) != ""   // the live path
//	case bool:   out[f.Key] = v                            // unreachable here
//	...
//
// The live path was already correct: SiOpenaiApiKey is declared string, is
// written from envStr, and is never anything else, so `case string` always
// won and the raw key never entered the map. But readConfigField merges six
// differently-typed fields behind one `any`, and static analysis cannot
// refine the dynamic type across that boundary. CodeQL therefore admitted the
// provably-unreachable `case bool` branch and reported the key as flowing
// into every downstream logging call -- 492 open `go/clear-text-logging`
// alerts on main, ALL 492 tracing to that single step, median 84-hop flow.
//
// Splitting the reader makes "a sensitive field's raw value never reaches the
// ctx map" a property of the type signature rather than of a runtime type
// coincidence. Behaviour is bit-for-bit identical to the `case string` branch
// it replaces.
//
// Extend this switch alongside readConfigField each time
// PolicyExposableConfig gains a Sensitive entry. An unknown name returns
// false, matching the old `default: value != nil` for a nil read.
// NOTE: there is NO Sensitive entry today. SiOpenaiApiKey was the only one and
// went with the vendor keys (epic memql#5088). This reader stays anyway, and
// deleting it would be the mistake: the property it enforces -- a sensitive
// field's raw value never reaches readConfigField's `any` return -- is a
// property of the SIGNATURE, and it has to already exist when the next
// Sensitive entry is added. Removing it would leave the next author with an
// allow-list flag whose enforcement mechanism they would have to rediscover
// from a CodeQL report of 492 alerts.
func readSensitivePresence(snapshot *busv1.ConfigSnapshot, name string) bool {
	if snapshot == nil {
		return false
	}
	switch name {
	// Add a case here whenever PolicyExposableConfig gains a Sensitive entry.
	}
	return false
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
	// A Sensitive allow-list entry is deliberately ABSENT from this switch
	// (memql#3188): such a field is read through readSensitivePresence, which
	// returns bool. Adding one here would put a raw secret into this
	// function's `any` return and re-open the taint path that produced 492
	// CodeQL alerts. There is no Sensitive entry at present -- SiOpenaiApiKey
	// was the only one and went with the vendor keys in memql#5088.
	case "SiDefaultProvider":
		return snapshot.SiDefaultProvider
	case "SttProvider":
		return snapshot.SttProvider
	case "AuthEnabled":
		return snapshot.AuthEnabled
	case "DemoMode":
		return snapshot.DemoMode
	}
	return nil
}
