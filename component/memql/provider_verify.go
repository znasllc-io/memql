package memql

// The `providersReload` and `providerVerify` ACTIONS (epic memql#4440,
// design D4/D5).
//
// Both are builtins rather than mutations because neither writes a row, and
// both are owner-gated in Go because a MemQL construct cannot carry a role
// predicate of its own -- the coarse write check that applies over the query
// surface admits every role from `writer` up.
//
// VERIFY IS A GO CALL, NOT THE INSTALL SHELL SCRIPT. `verify-provider-key.sh`
// runs on an operator's machine against a key on that machine's disk, before
// a cluster exists. This runs INSIDE a node, against whatever that node's
// resolution chain actually produced -- which is the only question worth
// asking after a seed, and the one the script structurally cannot answer.
// They coexist; neither replaces the other.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// ProvidersReloadConcept / ProviderVerifyConcept are the canonical ids of the
// single-row results these actions return. Virtual, like the status
// projection: an action's outcome is a reply, not a record.
const (
	ProvidersReloadConcept = "v1:platform:providersReloadResult"
	ProviderVerifyConcept  = "v1:platform:providerVerifyResult"
)

// evaluateProvidersReloadExpression re-resolves provider auth on THIS node and
// broadcasts the same request to every other node.
//
// LOCAL FIRST, THEN BROADCAST, and the order matters. The caller is waiting on
// a reply and expects it to describe a state that is now true somewhere; doing
// the local reload synchronously means the returned `available` count is this
// node's real post-reload number rather than a promise. The broadcast is what
// makes the other replicas agree, and their outcomes land in their own logs
// and in their own providerAuthStatus -- which is what the portal re-reads.
//
// THE BROADCAST REACHES THIS NODE TOO, and that is harmless rather than
// sloppy: the reload is idempotent (build a registry, swap it in), so the
// second pass produces the same contents. Filtering self out would mean
// trusting OriginNodeId to be set on a locally-published event, which is a
// thing to be checked rather than assumed, and the cost of being wrong is a
// node that never reloads.
func (e *MemQLEngine) evaluateProvidersReloadExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if !rowAuthzIsClusterOwner(ctx) {
		return nil, fmt.Errorf("providersReload is owner-only")
	}

	requestId := strings.TrimSpace(stringArg(args, "requestId"))
	if requestId == "" {
		// Derived rather than required: the caller has no reason to invent
		// one, and an empty trailing topic segment would collapse two
		// concurrent Applies into one topic.
		requestId = fmt.Sprintf("apply-%d", time.Now().UTC().UnixNano())
	}

	requestedBy := ""
	if access, ok := auth.AccessFromContext(ctx); ok && access != nil {
		requestedBy = access.UserId
	}

	available, err := e.ReloadAIProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("providersReload: %w", err)
	}

	// AUDIT BEFORE BROADCAST. The line records that a human asked, which is
	// true the moment the local reload succeeded; emitting it after the
	// fan-out would make an audit trail that is missing exactly the rotations
	// that went wrong on other nodes.
	e.emitProvidersReloadAudit(ctx, requestedBy, requestId)
	e.PublishProvidersReload(requestId)

	payload := map[string]any{
		"requestId": requestId,
		// This node's number. Named so a reader cannot mistake it for a
		// fleet-wide one -- the other replicas reload asynchronously, and
		// their answer comes from providerAuthStatus.
		"availableOnThisNode": available,
		"registered":          e.providers.Count(),
		"broadcast":           true,
	}
	return singleVirtualRow(ProvidersReloadConcept, requestId, payload)
}

// evaluateProviderVerifyExpression makes ONE authenticated call to a
// provider's vendor and reports whether the credential this node resolved was
// accepted.
//
// TOKEN-FREE BY CONSTRUCTION. It lists models -- the cheapest authenticated
// request either vendor serves -- so an operator can press Verify as often as
// they like without spending inference. That property is why this can be a
// button at all, and it is the reason the call is a models-list rather than a
// one-token completion.
//
// A REFUSAL IS NOT AN ERROR. A bad key is an ANSWER, and returning a Go error
// for it would make the portal render an exception where it should render
// "the vendor rejected this key". Only a question that cannot be asked --
// unknown provider name, a non-owner caller -- errors.
func (e *MemQLEngine) evaluateProviderVerifyExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if !rowAuthzIsClusterOwner(ctx) {
		return nil, fmt.Errorf("providerVerify is owner-only")
	}
	if e.providers == nil {
		return nil, fmt.Errorf("providerVerify: no provider registry on this node")
	}

	name := strings.TrimSpace(stringArg(args, "provider"))
	if name == "" {
		return nil, fmt.Errorf("providerVerify: a provider name is required")
	}
	entry, ok := e.providers.Entry(name)
	if !ok || entry == nil {
		return nil, fmt.Errorf("providerVerify: no provider named %q is registered on this node", name)
	}

	result := map[string]any{
		"provider":   entry.Config.Name,
		"vendor":     entry.Config.Type,
		"model":      entry.Config.Model,
		"authSource": string(providerAuthSourceOf(entry)),
		"verified":   false,
		"reason":     "",
	}

	// UNAVAILABLE IS ITS OWN ANSWER, distinct from "the vendor said no". The
	// credential never reached the vendor, so reporting a rejection would
	// blame Anthropic for a missing env var.
	if !entry.Available || entry.Client == nil {
		result["reason"] = providerUnavailableReason(entry)
		if result["reason"] == "" {
			result["reason"] = "this node has no usable client for the provider"
		}
		return singleVirtualRow(ProviderVerifyConcept, entry.Config.Name, result)
	}

	models, err := verifyProviderCredential(ctx, entry)
	if err != nil {
		result["reason"] = err.Error()
		return singleVirtualRow(ProviderVerifyConcept, entry.Config.Name, result)
	}
	result["verified"] = true
	result["modelsListed"] = models
	return singleVirtualRow(ProviderVerifyConcept, entry.Config.Name, result)
}

// verifyProviderCredential performs the vendor call for one entry.
//
// ANTHROPIC ONLY, TODAY, and it says so rather than reporting a pass it did
// not observe. Reusing `CheckProviderAuth`'s machinery is impossible here: it
// rebuilds the registry from scratch WITHOUT the engine, so it consults only
// the process environment and would report a key seeded through the portal as
// absent -- which is the exact state this button exists to confirm. This runs
// against the LIVE entry the running node resolved.
func verifyProviderCredential(ctx context.Context, entry *ProviderConfigEntry) (int, error) {
	if anthropicProviderTypes[strings.ToLower(entry.Config.Type)] {
		client, err := anthropicClientOf(entry.Client)
		if err != nil {
			return 0, err
		}
		page, err := client.Models.List(ctx, anthropic.ModelListParams{})
		if err != nil {
			return 0, err
		}
		if page == nil {
			return 0, nil
		}
		return len(page.Data), nil
	}
	// Saying so beats a silent true. An OpenAI provider that constructed has
	// a well-formed key and a reachable base URL, which is not the same as an
	// accepted one, and the page must not claim otherwise.
	return 0, fmt.Errorf(
		"no live verification is implemented for provider type %q; "+
			"the credential resolved and the client constructed, which is not the same as the vendor accepting it",
		entry.Config.Type)
}

// singleVirtualRow wraps one payload as the single-row result an action
// returns. Never persisted -- the same contract as the status projection.
func singleVirtualRow(concept, id string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:      id,
		Concept: concept,
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
	}}, nil
}

// stringArg reads a string builtin argument, tolerating absence.
func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}
