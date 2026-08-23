package planner

import (
	"context"
	"testing"

	componentplanner "github.com/znasllc-io/memql/component/planner"
)

type stubPolicies struct {
	pref componentplanner.DelegationPreference
}

func (s stubPolicies) DelegationPreference(context.Context, string) componentplanner.DelegationPreference {
	return s.pref
}

type stubProbe struct{ machine string }

func (p stubProbe) FindMachineForApp(context.Context, string, string) string { return p.machine }
func (p stubProbe) LiveSessionCount(context.Context, string) int             { return 0 }

// TestNoResolverMeansInProcessWithAReason is what a Task looks like on a
// node that never installed the triage: in-process, and SAYING SO. A
// blank delegationReason would be indistinguishable from "the user has
// delegation on and nothing was online", which is a different problem
// with a different fix.
func TestNoResolverMeansInProcessWithAReason(t *testing.T) {
	loop := &PlannerAgentLoop{}
	got := loop.decideDelegation(context.Background(), "user-1", "runCommand")

	if got.Delegate {
		t.Fatal("a loop with no resolver must not delegate")
	}
	if got.Surface() != componentplanner.ExecutionSurfaceInProcess {
		t.Fatalf("surface = %q, want inProcess", got.Surface())
	}
	if got.Reason != componentplanner.DelegationReasonPolicyOff {
		t.Fatalf("reason = %q, want %q", got.Reason, componentplanner.DelegationReasonPolicyOff)
	}
}

// TestResolverDelegatesWhenPolicyAndMachineAgree is the positive control
// for the test above -- without it, "does not delegate" would pass for a
// resolver that could never delegate at all.
func TestResolverDelegatesWhenPolicyAndMachineAgree(t *testing.T) {
	loop := &PlannerAgentLoop{}
	loop.SetDelegationResolver(&PolicyDelegationResolver{
		Policies: stubPolicies{pref: componentplanner.DelegationPreference{
			Enabled:       true,
			EligibleKinds: []string{"runCommand"},
			AppOrder:      []string{"claude-code"},
		}},
		Probe: stubProbe{machine: "reg-a"},
	})

	got := loop.decideDelegation(context.Background(), "user-1", "runCommand")
	if !got.Delegate {
		t.Fatalf("expected delegation, got reason %q", got.Reason)
	}
	if got.Backend != "cockpit-app:claude-code" {
		t.Fatalf("backend = %q, want cockpit-app:claude-code", got.Backend)
	}
	if got.Surface() != componentplanner.ExecutionSurfaceContainerExecutor {
		t.Fatalf("surface = %q, want containerExecutor", got.Surface())
	}

	// And an INELIGIBLE kind through the same resolver still runs
	// in-process, so the resolver is deciding rather than always
	// agreeing.
	if other := loop.decideDelegation(context.Background(), "user-1", "browseUrl"); other.Delegate {
		t.Fatal("an ineligible kind must not be delegated")
	}
}

// TestResolverWithNoPoliciesIsOff: a resolver missing its policy reader
// must fail closed, not delegate on defaults.
func TestResolverWithNoPoliciesIsOff(t *testing.T) {
	resolver := &PolicyDelegationResolver{Probe: stubProbe{machine: "reg-a"}}
	got := resolver.Resolve(context.Background(), "user-1", "runCommand")
	if got.Delegate {
		t.Fatal("a resolver with no policy reader must not delegate")
	}
	if got.Reason != componentplanner.DelegationReasonPolicyOff {
		t.Fatalf("reason = %q, want %q", got.Reason, componentplanner.DelegationReasonPolicyOff)
	}
}
