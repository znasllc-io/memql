package memql

// authoring_capability_gate.go -- authority + capability gating for authored
// automations (epic memql#954, issue #961, increment 2).
//
// An authored automation runs LIVE under its author's authz envelope (#959's
// AuthorContext). When such an automation reaches for a privileged action -- a
// computer-use call on the author's machine, an outbound webhook, or a graph
// mutation -- it must clear the SAME gates the planner already enforces for an
// agent doing the same thing (the worker dispatcher's preDispatchCheck):
//
//   1. the per-user KILL SWITCH (v1:identity:user.preferences.computerUseEnabled)
//      -- a global off-switch the user controls;
//   2. the standing SCOPE GRANT (v1:agents:agentAuthorization.computerUseScope:
//      observe / full) -- the author can never act beyond the scope they were
//      granted.
//
// The crucial property: the gate is keyed on the AUTHOR (the bundle owner), so
// an authored automation can NEVER escalate beyond its author's envelope. It
// reuses the exact scope ordering + kill-switch semantics the worker path uses
// rather than inventing a parallel policy.
//
// Like the activation orchestrator, the graph reads go through a narrow
// CapabilityStore the tests fake (no DB), and the decision logic is pure +
// table-tested.

import (
	"context"
	"fmt"
	"strings"
)

// AuthoredCapability is the class of privileged action an authored automation
// is attempting. Each maps to the scope tier it requires.
type AuthoredCapability string

const (
	// CapComputerUseObserve -- a read-only computer-use action (screenshot /
	// read state). Requires at least observe scope.
	CapComputerUseObserve AuthoredCapability = "computerUse.observe"
	// CapComputerUseFull -- a mutating computer-use action (click / type / shell
	// / fs write). Requires full scope.
	CapComputerUseFull AuthoredCapability = "computerUse.full"
	// CapWebhook -- an outbound webhook / external POST. Requires full scope (it
	// acts outside the cluster on the author's behalf).
	CapWebhook AuthoredCapability = "webhook"
	// CapMutation -- a graph mutation. Per-row authz already confines it to what
	// the author owns; the gate adds the kill-switch + an explicit allow so a
	// killed envelope halts writes too.
	CapMutation AuthoredCapability = "mutation"
)

// requiredScope maps a capability to the scope tier it needs. Mutations need no
// scope grant (per-row authz is their gate), so they map to empty.
func (c AuthoredCapability) requiredScope() string {
	switch c {
	case CapComputerUseObserve:
		return "observe"
	case CapComputerUseFull, CapWebhook:
		return "full"
	default: // CapMutation and anything else
		return ""
	}
}

// killSwitchGoverned reports whether the per-user kill switch applies to this
// capability. Computer-use + webhooks + mutations are ALL halted by the kill
// switch (the global off-switch is the governance hard stop -- increment 3
// extends this to a cluster-wide switch via the same flag).
func (c AuthoredCapability) killSwitchGoverned() bool {
	return true
}

// scopeOrder gives an ordering on computer-use scopes mirroring the worker
// path (integrations/agent/worker/scope.go): empty < observe < full, with the
// legacy `interact` tier upgraded to full on read.
var scopeOrder = map[string]int{
	"":         0,
	"observe":  1,
	"interact": 2, // legacy: treated as full
	"full":     2,
}

// scopeSatisfies reports whether the author's effective scope clears the
// required tier.
func scopeSatisfies(have, need string) bool {
	return scopeOrder[have] >= scopeOrder[need]
}

// AuthoredEnvelope is the author's standing authz envelope the gate evaluates
// against: the kill-switch flag + the standing computer-use scope grant. It is
// the author's, never the automation's -- the no-escalation guarantee.
type AuthoredEnvelope struct {
	OwnerUserId string `json:"ownerUserId"`
	// KillSwitchEnabled is v1:identity:user.preferences.computerUseEnabled --
	// true = automations may act, false = halted.
	KillSwitchEnabled bool `json:"killSwitchEnabled"`
	// Scope is the author's standing computer-use scope grant (observe / full).
	Scope string `json:"scope"`
}

// CapabilityDecision is the gate outcome: allowed, or denied with a typed
// reason the caller surfaces (and audits). It mirrors the worker dispatcher's
// gateResult error codes so the two paths report denials identically.
type CapabilityDecision struct {
	Allow      bool   `json:"allow"`
	ErrorCode  string `json:"errorCode,omitempty"`
	Message    string `json:"message,omitempty"`
	Capability string `json:"capability"`
	NeedScope  string `json:"needScope,omitempty"`
	HaveScope  string `json:"haveScope,omitempty"`
}

// Decide error codes -- aligned with the worker dispatcher.
const (
	CapErrKillSwitch = "kill_switch_engaged"
	CapErrScope      = "denied_by_scope"
)

// EvaluateCapability is the pure gate: given the author's envelope and the
// capability the automation wants, decide allow/deny. It applies the kill
// switch first (a hard stop), then the scope grant. Deterministic + table
// tested; the engine-backed AuthorizeCapability loads the envelope and calls it.
func EvaluateCapability(env AuthoredEnvelope, cap AuthoredCapability) CapabilityDecision {
	dec := CapabilityDecision{Capability: string(cap), NeedScope: cap.requiredScope(), HaveScope: env.Scope}

	if cap.killSwitchGoverned() && !env.KillSwitchEnabled {
		dec.ErrorCode = CapErrKillSwitch
		dec.Message = "authored automations are halted by the user's kill switch"
		return dec
	}

	if need := cap.requiredScope(); need != "" {
		if !scopeSatisfies(env.Scope, need) {
			dec.ErrorCode = CapErrScope
			dec.Message = fmt.Sprintf("action requires scope %q under the author's envelope, author has %q", need, env.Scope)
			return dec
		}
	}

	dec.Allow = true
	return dec
}

// CapabilityStore loads the author's standing envelope from the graph: the
// kill-switch preference + the standing computer-use scope. *MemQLEngine
// satisfies it via engineCapabilityStore; tests fake it.
type CapabilityStore interface {
	// LoadEnvelope resolves the author's kill-switch flag + standing scope.
	LoadEnvelope(ctx context.Context, ownerUserId string) (AuthoredEnvelope, error)
}

// AuthorizeCapability is the engine-backed gate: load the author's envelope and
// evaluate the requested capability. Used by the authored runtime before an
// automation's privileged action runs. owner MUST be the bundle owner (the
// author) -- the envelope is theirs, so the automation cannot exceed it.
func (e *MemQLEngine) AuthorizeCapability(ctx context.Context, owner string, cap AuthoredCapability) (CapabilityDecision, error) {
	return AuthorizeCapabilityWithStore(ctx, &engineCapabilityStore{engine: e}, owner, cap)
}

// AuthorizeCapabilityWithStore is the store-driven core: usable with a custom
// CapabilityStore and fakeable in tests (no live DB).
func AuthorizeCapabilityWithStore(ctx context.Context, store CapabilityStore, owner string, cap AuthoredCapability) (CapabilityDecision, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return CapabilityDecision{Capability: string(cap)}, fmt.Errorf("authoring: capability gate requires an author")
	}
	env, err := store.LoadEnvelope(ctx, owner)
	if err != nil {
		return CapabilityDecision{Capability: string(cap)}, fmt.Errorf("authoring: load author envelope for %q: %w", owner, err)
	}
	env.OwnerUserId = owner
	return EvaluateCapability(env, cap), nil
}
