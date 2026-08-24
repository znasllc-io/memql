package memql

// The `providerAuthStatus` VIRTUAL READ (epic memql#4440, design D4).
//
// One row per registered provider: what it is, whether this node can call it,
// WHERE its credential came from, and -- when it cannot -- why not. Produced
// at query time from the live registry and never persisted, the
// `dataOrigins` / `v1:router:modelCatalog` pattern, and for the same reason:
// the answer is a property of THIS process's registry at THIS instant, so a
// persisted copy could only ever be a second, staler answer, and the staleness
// would be indistinguishable from the failure it is describing.
//
// PER-NODE, AND THAT IS A FEATURE. Every node resolves provider auth for
// itself, so two replicas genuinely can disagree -- one restarted after a
// secret was seeded and one not. That disagreement is the single most useful
// thing this read surfaces, and it is why D5's reload broadcasts rather than
// running on whichever node the portal happened to reach.
//
// WHAT IT MUST NEVER CARRY is a credential, or anything a credential can be
// reconstructed from. There is no fingerprint field and no truncated value
// here: the page shows availability and source, and "verify" is a live call
// rather than a comparison against something rendered. `authSource` names a
// TIER, never a value.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// ProviderAuthStatusConcept is the canonical id of the virtual projection.
const ProviderAuthStatusConcept = "v1:platform:providerAuthStatus"

// evaluateProviderAuthStatusExpression produces one row per registered
// provider, sorted by name so a client rendering a list gets a stable order.
//
// OWNER-ONLY, GATED IN GO. A MemQL builtin carries no role predicate of its
// own and the coarse write check admits every role from `writer` up, so the
// wall has to be here. What it protects is not a secret -- no credential is in
// the payload -- but a map of which vendors this deployment talks to and which
// of them are misconfigured, which is reconnaissance and is nobody's business
// but the operator's.
func (e *MemQLEngine) evaluateProviderAuthStatusExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, nil
	}
	if !rowAuthzIsClusterOwner(ctx) {
		return nil, fmt.Errorf("providerAuthStatus is owner-only")
	}
	if e.providers == nil {
		return nil, nil
	}

	names := e.providers.Names()
	nodes := make([]memorynodes.MemoryNode, 0, len(names))
	for _, name := range names {
		entry, ok := e.providers.Entry(name)
		if !ok || entry == nil {
			continue
		}
		// A @base provider is metadata -- an @extends target with no model and
		// no client -- and is Available=false on purpose. Listing it would put
		// a permanently-red row in front of the operator describing something
		// that was never meant to be callable.
		if entry.Config.Base {
			continue
		}
		payload := map[string]any{
			"name":       entry.Config.Name,
			"vendor":     entry.Config.Type,
			"model":      entry.Config.Model,
			"available":  entry.Available,
			"authSource": string(providerAuthSourceOf(entry)),
			"reason":     providerUnavailableReason(entry),
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, memorynodes.MemoryNode{
			// The row id IS the provider name: there is exactly one row per
			// provider and the provider is what it is about.
			ID:      entry.Config.Name,
			Concept: ProviderAuthStatusConcept,
			Type:    memorynodes.NodeTypeObject,
			Payload: raw,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes, nil
}

// providerAuthSourceOf reports which tier supplied this provider's credential.
//
// FEDERATION IS DECIDED FIRST and structurally, not by re-resolving: an
// Anthropic provider whose four federation ids are present authenticates by
// token exchange no matter where those ids came from, and reporting the tier
// that supplied one of the ids would answer a question nobody asked. The
// all-or-none rule means "complete" is the only federated state that boots, so
// a partial set correctly falls through to whatever the apiKey resolves to.
//
// Otherwise it RE-RESOLVES rather than remembering what boot found, which is
// deliberate: a value seeded since boot shows up here as globalSecret even
// before a reload, so the page can say "seeded, not yet applied" -- which is
// exactly the state an operator is in between Save and Apply.
func providerAuthSourceOf(entry *ProviderConfigEntry) ProviderAuthSource {
	if entry == nil {
		return AuthSourceUnresolved
	}
	// `missing()` empty is the complete set -- the same predicate
	// anthropicCredential branches on, read through the same helper rather
	// than re-derived, so "what counts as federated" has one definition. Note
	// the workspace id is deliberately outside that required set.
	if fed := anthropicFederationFrom(entry.Config.Auth); len(fed.missing()) == 0 && len(fed.present()) > 0 {
		return AuthSourceFederation
	}
	// The placeholder is what names the env key; a literal value in the DSL
	// (tests, and nothing shipped) has no tier and reads as env-equivalent.
	for _, key := range []string{authKeyAPIKey} {
		raw, ok := entry.Config.Auth[key]
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(raw)
		if !strings.HasPrefix(trimmed, "${") || !strings.HasSuffix(trimmed, "}") {
			// Already substituted (boot resolves in place) or a literal. Boot
			// only substitutes what it resolved, so a non-empty value here is
			// a credential that came from SOMEWHERE; env is the honest floor
			// rather than a guess at which row supplied it.
			if trimmed == "" {
				return AuthSourceUnresolved
			}
			envKey := providerAuthEnvKeyFor(entry)
			if envKey == "" {
				return AuthSourceEnv
			}
			_, source, ok := resolveAuthValueSourced(envKey)
			if !ok {
				return AuthSourceEnv
			}
			return source
		}
		_, source, _ := resolveAuthValueSourced(strings.TrimSpace(trimmed[2 : len(trimmed)-1]))
		return source
	}
	return AuthSourceUnresolved
}

// providerAuthEnvKeyFor names the env key a provider's API key is seeded
// under, for an entry whose auth map boot has already substituted in place.
//
// Derived from the VENDOR rather than remembered, because the substituted map
// no longer holds the placeholder that named it. Only the two supported
// vendors have an answer; anything else reports none and the caller falls back
// to the honest floor.
func providerAuthEnvKeyFor(entry *ProviderConfigEntry) string {
	switch {
	case anthropicProviderTypes[strings.ToLower(entry.Config.Type)]:
		return envAnthropicAPIKey
	case strings.HasPrefix(strings.ToLower(entry.Config.Type), "openai"):
		return "MEMQL_AI_OPENAI_API_KEY"
	default:
		return ""
	}
}

// providerUnavailableReason is the operator-facing sentence for a provider
// that cannot be called, or "" when it can.
//
// The construction error is passed through rather than summarised: it is
// written by the constructor that refused, it already names the missing env
// var or the half-configured federation set, and a friendlier rewrite here
// would be a second, vaguer account of a decision made somewhere else.
func providerUnavailableReason(entry *ProviderConfigEntry) string {
	if entry == nil {
		return "provider entry is missing"
	}
	if entry.Available {
		return ""
	}
	if entry.err != nil {
		return entry.err.Error()
	}
	return "registered but not callable, and no reason was recorded"
}
