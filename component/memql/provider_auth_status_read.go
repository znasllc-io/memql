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
//
// THE ANSWERING NODE'S OWN SIGN-IN (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions A5 and U6). A row of a vendor this process has tried to sign in to
// carries `signIn`: what that sign-in got, and when. It is THIS process's
// record and nobody else's, for the reason the rows are per node: each node
// signs in for itself, so one can be refused while another is accepted, and a
// page that showed one answer for the cluster would be wrong about some node.
// The page names it as the answering node's.
//
// It is a short list of fields copied one by one (signInOf), never the record
// marshalled whole. The record also holds the federation ids, the identity
// token's subject and audience, a configuration fingerprint and the process
// id, which the check command prints for an operator at a shell and which a
// page has no use for. The bearer is not on the record at all.
//
// Regression tests: provider_auth_status_signin_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

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
		if signIn := signInOf(entry.Config.Type); signIn != nil {
			payload["signIn"] = signIn
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

// signInVendorOfType names the vendor whose sign-in a provider type uses, or ""
// for a type that signs in to neither. Every record of a vendor shares the
// vendor's one sign-in, whatever it does: chat, speech, embeddings.
func signInVendorOfType(providerType string) string {
	switch lower := strings.ToLower(strings.TrimSpace(providerType)); {
	case anthropicProviderTypes[lower]:
		return VendorAnthropic
	case isOpenAIProviderType(lower):
		return VendorOpenAI
	}
	return ""
}

// signInOf is what THIS process's sign-in at a provider type's vendor last got,
// as a status row may show it, or nil when the process has made no attempt.
//
// "Nobody set this vendor up" and "this node type signs in to no vendor" are
// records too, and are not sign-ins: the row's own `authSource` and `reason`
// already say that. A set of ids the engine refuses before asking any vendor is
// likewise the row's `reason`. Only an attempt, or a wait for the next token,
// is reported here.
//
// A time that has not happened is absent, never a zero date.
func signInOf(providerType string) map[string]any {
	vendor := signInVendorOfType(providerType)
	if vendor == "" {
		return nil
	}
	held, ok := currentVendorSignIn(vendor)
	if !ok {
		return nil
	}
	rec, recorded := held.Record()
	if !recorded {
		return nil
	}
	switch rec.Outcome {
	case signInOutcomeOK, signInOutcomeDenied, signInOutcomeError, signInOutcomeWaiting:
	default:
		return nil
	}
	out := map[string]any{"outcome": rec.Outcome}
	stamp := func(key string, at time.Time) {
		if !at.IsZero() {
			out[key] = at.UTC().Format(time.RFC3339)
		}
	}
	text := func(key, value string) {
		if value != "" {
			out[key] = value
		}
	}
	stamp("at", rec.At)
	stamp("nextTokenDue", rec.NextTokenDue)
	stamp("proofAt", rec.ProofAt)
	stamp("bearerDroppedAt", rec.BearerDroppedAt)
	text("detail", rec.Detail)
	text("proof", rec.Proof)
	text("proofDetail", rec.ProofDetail)
	if rec.MayBeReplay {
		out["mayBeReplay"] = true
	}
	return out
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
	// `missing()` empty is the complete set -- the same predicate each
	// vendor's credential switch branches on, read through the same helper
	// rather than re-derived, so "what counts as federated" has one
	// definition per vendor. Anthropic's workspace id is deliberately outside
	// its required set.
	if fed := anthropicFederationFrom(entry.Config.Auth); len(fed.missing()) == 0 && len(fed.present()) > 0 {
		return AuthSourceFederation
	}
	if fed := openaiFederationFrom(entry.Config.Auth); len(fed.missing()) == 0 && len(fed.present()) > 0 {
		return AuthSourceFederation
	}
	// THERE IS NO KEY TIER LEFT (epic memql#5088). This function used to fall
	// through to reading auth.apiKey and reporting which of globalSecret /
	// globalVariable / env supplied it. No provider declares an apiKey any
	// more, so the fall-through would have been an unreachable branch that
	// still looked like a supported answer -- and the one thing a status
	// reader must never do is report a credential path the engine cannot take.
	// Federation, or nothing.
	return AuthSourceUnresolved
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
