package memql_test

// authoring_capability_gate_test.go -- coverage for the authority + capability
// gate (epic memql#954, issue #961, increment 2).
//
// The gate confines an authored automation to its AUTHOR's envelope: the kill
// switch halts everything; the standing scope grant caps computer-use; no
// escalation. Pure decision tests + the engine-backed gate through a fake
// CapabilityStore.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// fakeCapabilityStore serves a fixed envelope.
type fakeCapabilityStore struct {
	env memql.AuthoredEnvelope
	err error
}

func (s fakeCapabilityStore) LoadEnvelope(_ context.Context, owner string) (memql.AuthoredEnvelope, error) {
	if s.err != nil {
		return memql.AuthoredEnvelope{}, s.err
	}
	e := s.env
	e.OwnerUserId = owner
	return e, nil
}

// TestEvaluateCapability_KillSwitchHaltsEverything: with the kill switch off,
// every privileged capability is denied -- including mutations.
func TestEvaluateCapability_KillSwitchHaltsEverything(t *testing.T) {
	env := memql.AuthoredEnvelope{KillSwitchEnabled: false, Scope: "full"}
	for _, cap := range []memql.AuthoredCapability{
		memql.CapComputerUseObserve, memql.CapComputerUseFull, memql.CapWebhook, memql.CapMutation,
	} {
		dec := memql.EvaluateCapability(env, cap)
		if dec.Allow {
			t.Errorf("%s should be denied when the kill switch is off", cap)
		}
		if dec.ErrorCode != memql.CapErrKillSwitch {
			t.Errorf("%s expected kill_switch_engaged, got %q", cap, dec.ErrorCode)
		}
	}
}

// TestEvaluateCapability_ScopeCeiling: with the kill switch on, computer-use is
// capped by the author's standing scope -- observe scope allows observe but not
// full / webhook; full allows all.
func TestEvaluateCapability_ScopeCeiling(t *testing.T) {
	observe := memql.AuthoredEnvelope{KillSwitchEnabled: true, Scope: "observe"}
	if dec := memql.EvaluateCapability(observe, memql.CapComputerUseObserve); !dec.Allow {
		t.Errorf("observe scope should allow an observe action: %+v", dec)
	}
	if dec := memql.EvaluateCapability(observe, memql.CapComputerUseFull); dec.Allow || dec.ErrorCode != memql.CapErrScope {
		t.Errorf("observe scope must NOT allow a full action: %+v", dec)
	}
	if dec := memql.EvaluateCapability(observe, memql.CapWebhook); dec.Allow {
		t.Errorf("observe scope must NOT allow a webhook (needs full): %+v", dec)
	}

	full := memql.AuthoredEnvelope{KillSwitchEnabled: true, Scope: "full"}
	for _, cap := range []memql.AuthoredCapability{memql.CapComputerUseObserve, memql.CapComputerUseFull, memql.CapWebhook} {
		if dec := memql.EvaluateCapability(full, cap); !dec.Allow {
			t.Errorf("full scope should allow %s: %+v", cap, dec)
		}
	}
}

// TestEvaluateCapability_MutationNeedsNoScopeButNeedsKillSwitch: a mutation
// needs no computer-use scope (per-row authz gates it) but is still halted by
// the kill switch.
func TestEvaluateCapability_MutationNeedsNoScopeButNeedsKillSwitch(t *testing.T) {
	noScope := memql.AuthoredEnvelope{KillSwitchEnabled: true, Scope: ""}
	if dec := memql.EvaluateCapability(noScope, memql.CapMutation); !dec.Allow {
		t.Errorf("a mutation should be allowed with the kill switch on regardless of scope: %+v", dec)
	}
	killed := memql.AuthoredEnvelope{KillSwitchEnabled: false, Scope: "full"}
	if dec := memql.EvaluateCapability(killed, memql.CapMutation); dec.Allow {
		t.Errorf("a mutation must be halted by the kill switch: %+v", dec)
	}
}

// TestAuthorizeCapability_LoadsEnvelopeAndDecides: the engine-backed gate loads
// the author's envelope from the store and applies the decision.
func TestAuthorizeCapability_LoadsEnvelopeAndDecides(t *testing.T) {
	store := fakeCapabilityStore{env: memql.AuthoredEnvelope{KillSwitchEnabled: true, Scope: "observe"}}
	dec, err := memql.AuthorizeCapabilityWithStore(context.Background(), store, "user-a", memql.CapComputerUseFull)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if dec.Allow || dec.ErrorCode != memql.CapErrScope {
		t.Errorf("expected scope denial for a full action under observe scope, got %+v", dec)
	}

	// A scope-clearing capability is allowed.
	okStore := fakeCapabilityStore{env: memql.AuthoredEnvelope{KillSwitchEnabled: true, Scope: "full"}}
	if dec, _ := memql.AuthorizeCapabilityWithStore(context.Background(), okStore, "user-a", memql.CapWebhook); !dec.Allow {
		t.Errorf("expected a webhook to be allowed under full scope, got %+v", dec)
	}
}

// TestAuthorizeCapability_RejectsEmptyOwner: no author -> error (no anonymous
// authored action).
func TestAuthorizeCapability_RejectsEmptyOwner(t *testing.T) {
	store := fakeCapabilityStore{env: memql.AuthoredEnvelope{KillSwitchEnabled: true, Scope: "full"}}
	if _, err := memql.AuthorizeCapabilityWithStore(context.Background(), store, "", memql.CapMutation); err == nil {
		t.Error("expected an empty-owner capability check to error")
	}
}
