package memql

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/znasllc-io/memql/core/common"
)

// suggest_registry.go -- the AiSuggest domain extension point (memql#1959).
//
// The gRPC AiSuggestMsg handler (handleAiSuggest in
// component/grpc/ai_handlers.go) used to carry a hardcoded `switch domain`
// listing every suggest surface, with the per-domain prompt/schema/post-process
// helpers imported directly from component/server/sihttp + integrations/agent.
// That coupled engine-only core to CoPresent product suggest surfaces (space /
// agent / group / card-summary / guide), violating the zero-CoPresent-refs core
// goal (G3).
//
// This file replaces the switch with a registry, mirroring RegisterPlugin (in
// plugins.go) / node.RegisterRoutingRule: a suggest domain self-registers from
// init() under a build tag, supplying a SuggestDomainHandler. The generic
// dispatch + structured-output plumbing stays in core handleAiSuggest -- it
// looks up the handler for the wire `domain`, builds a narrow SuggestContext,
// calls the handler to obtain the SuggestPlan (messages + schema +
// post-process), runs the structured-output call, applies the post-process,
// and replies. Unknown domain falls through to the existing typed
// InvalidArgument error.
//
// The registry lives in the engine package (component/memql) rather than in
// component/grpc so the pack can register its product domains by importing only
// the stable engine package it already depends on for RegisterPlugin -- it does
// NOT have to pull in the heavy gRPC server package.
//
// The 9 CoPresent product domains (spaces, spaceTitle, agents, groups,
// groupDescription, agentCardSummary, spaceCardSummary, groupCardSummary,
// guide) register from the pack (memql-bff-copresent) under the `copresent`
// build tag; only `knowledge` stays core-registered (knowledge is core per epic
// 3.1, see suggest_knowledge.go). Engine-only core builds (mcp/identity/voice)
// drop the pack and therefore carry only the knowledge domain -- exactly the G3
// surface.
//
// The wire `domain` enum string values are unchanged -- the SPA is unaffected.

// SuggestContext is the narrow surface a SuggestDomainHandler receives. It
// exposes the parsed request payload plus a few payload-extraction helpers
// (mirroring the helpers the old switch used), without leaking the gRPC stream
// session, the engine, or any core internals. The structured-output call + the
// reply are driven by core handleAiSuggest after the handler returns its plan.
type SuggestContext struct {
	// Payload is the deserialized AiSuggestMsg payload (Struct.AsMap()),
	// never nil. Each domain pulls its own required fields out of it.
	Payload map[string]any
}

// String returns the string value at key, or "" when missing / non-string.
func (c SuggestContext) String(key string) string {
	if v, ok := c.Payload[key].(string); ok {
		return v
	}
	return ""
}

// StringSlice returns the []string value at key, dropping non-string
// elements (returns nil when absent / not an array).
func (c SuggestContext) StringSlice(key string) []string {
	arr, ok := c.Payload[key].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Bool returns the bool at key, or fallback when missing / non-bool.
func (c SuggestContext) Bool(key string, fallback bool) bool {
	if v, ok := c.Payload[key].(bool); ok {
		return v
	}
	return fallback
}

// Int returns the int at key (JSON numbers decode as float64), or fallback
// when missing / non-numeric.
func (c SuggestContext) Int(key string, fallback int) int {
	if v, ok := c.Payload[key].(float64); ok {
		return int(v)
	}
	return fallback
}

// SuggestPlan is what a SuggestDomainHandler returns: the rendered prompt
// messages, the structured-output schema (+ its name), and an optional
// post-process pass run on the parsed suggestion map before it is serialized
// back to the client. This is the same four-part shape the old switch built
// inline (messages / schema / schemaName / postProcess).
type SuggestPlan struct {
	// Messages is the rendered system + user prompt for the structured-output
	// call. Required.
	Messages []common.ChatMessage

	// SchemaName names the structured-output schema (e.g.
	// "spaceConfigSuggest"). Required.
	SchemaName string

	// Schema is the JSON Schema the provider enforces on the output.
	Schema json.RawMessage

	// PostProcess, when non-nil, mutates the parsed suggestion map in place
	// after the structured-output call and before serialization (e.g.
	// validating ids, clamping strings, supplying deterministic fallbacks).
	PostProcess func(map[string]any)
}

// SuggestValidationError is returned by a SuggestDomainHandler when the
// request payload is missing a required field. Core handleAiSuggest converts
// it to the same codes.InvalidArgument query error the old per-domain
// validation checks emitted (preserving the wire behavior).
type SuggestValidationError struct {
	Message string
}

func (e *SuggestValidationError) Error() string { return e.Message }

// SuggestValidationErrorf builds a SuggestValidationError with a formatted
// message. Handlers use this for the "X is required in payload" checks.
func SuggestValidationErrorf(format string, args ...any) error {
	return &SuggestValidationError{Message: fmt.Sprintf(format, args...)}
}

// SuggestDomainHandler builds the SuggestPlan for one suggest domain from the
// request context. Returning a *SuggestValidationError signals a bad request
// (InvalidArgument); any other error is treated as an internal failure.
type SuggestDomainHandler func(ctx SuggestContext) (SuggestPlan, error)

var (
	suggestDomainsMu sync.RWMutex
	suggestDomains   = map[string]SuggestDomainHandler{}
)

// RegisterSuggestDomain registers a suggest-domain handler under its wire
// `domain` string from an init() function. Build tags on the calling file
// control which binaries include the registration, so product suggest surfaces
// (the CoPresent space/agent/group/card-summary/guide domains in the pack) can
// self-register without editing core handleAiSuggest. Core registers only
// `knowledge` (knowledge is core). A duplicate registration for the same domain
// panics at init -- it signals two packages claiming the same wire domain,
// which is always a bug.
func RegisterSuggestDomain(domain string, handler SuggestDomainHandler) {
	if domain == "" {
		panic("RegisterSuggestDomain: empty domain")
	}
	if handler == nil {
		panic("RegisterSuggestDomain: nil handler for domain " + domain)
	}
	suggestDomainsMu.Lock()
	defer suggestDomainsMu.Unlock()
	if _, dup := suggestDomains[domain]; dup {
		panic("RegisterSuggestDomain: duplicate registration for domain " + domain)
	}
	suggestDomains[domain] = handler
}

// LookupSuggestDomain returns the handler registered for domain, or nil.
func LookupSuggestDomain(domain string) SuggestDomainHandler {
	suggestDomainsMu.RLock()
	defer suggestDomainsMu.RUnlock()
	return suggestDomains[domain]
}

// RegisteredSuggestDomains returns the sorted set of registered domain names
// (used in the unsupported-domain error so the message lists what IS available
// in this binary).
func RegisteredSuggestDomains() []string {
	suggestDomainsMu.RLock()
	defer suggestDomainsMu.RUnlock()
	out := make([]string, 0, len(suggestDomains))
	for d := range suggestDomains {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
