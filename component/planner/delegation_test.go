package planner

import (
	"context"
	"testing"
)

type stubProbe struct {
	online map[string]string
	live   int
	asked  []string
}

func (p *stubProbe) FindMachineForApp(_ context.Context, _, appId string) string {
	p.asked = append(p.asked, appId)
	return p.online[appId]
}

func (p *stubProbe) LiveSessionCount(context.Context, string) int { return p.live }

func enabledPref(kinds []string, apps []string) DelegationPreference {
	return DelegationPreference{Enabled: true, EligibleKinds: kinds, AppOrder: apps}
}

// TestDelegationFallsBackWhenNothingIsOnline is D6's core promise: a
// plan never waits for a laptop to wake up. With no machine online the
// decision is in-process, not a park.
func TestDelegationFallsBackWhenNothingIsOnline(t *testing.T) {
	got := DecideDelegation(context.Background(), "user-1", "runCommand",
		enabledPref([]string{"runCommand"}, []string{"claude-code"}),
		&stubProbe{online: map[string]string{}})

	if got.Delegate {
		t.Fatal("delegated with no machine online")
	}
	if got.Surface() != ExecutionSurfaceInProcess {
		t.Fatalf("surface = %q, want inProcess", got.Surface())
	}
	if got.Reason != DelegationReasonNoMachine {
		t.Fatalf("reason = %q, want %q", got.Reason, DelegationReasonNoMachine)
	}
}

// TestDelegationHonoursAppOrder: appOrder is an ORDER, and an app
// absent from it is never selected even when a machine has it -- the
// list is how the user says which subscription they want spent.
func TestDelegationHonoursAppOrder(t *testing.T) {
	probe := &stubProbe{online: map[string]string{"claude-code": "reg-a", "codex": "reg-b"}}
	got := DecideDelegation(context.Background(), "user-1", "runCommand",
		enabledPref([]string{"runCommand"}, []string{"codex", "claude-code"}), probe)

	if !got.Delegate {
		t.Fatal("expected delegation")
	}
	if got.Backend != "cockpit-app:codex" {
		t.Fatalf("backend = %q, want cockpit-app:codex (first in appOrder)", got.Backend)
	}
	if got.WorkerId != "reg-b" {
		t.Fatalf("workerId = %q, want reg-b", got.WorkerId)
	}
	if len(probe.asked) != 1 {
		t.Fatalf("probed %v; the first available app must win without probing further", probe.asked)
	}

	// An app the user did NOT list is never selected.
	unlisted := DecideDelegation(context.Background(), "user-1", "runCommand",
		enabledPref([]string{"runCommand"}, []string{"claude-code"}),
		&stubProbe{online: map[string]string{"codex": "reg-b"}})
	if unlisted.Delegate {
		t.Fatal("selected an app the user did not list in appOrder")
	}
}

// TestDelegationReasonsAreSpecific: the recorded reason is what
// answers "why did this run in-process". Checking cheap local facts
// before live probes is what keeps that answer pointed at the real
// cause -- a user with delegation switched off must not be told to go
// check a laptop that was never going to be consulted.
func TestDelegationReasonsAreSpecific(t *testing.T) {
	online := &stubProbe{online: map[string]string{"claude-code": "reg-a"}}

	cases := []struct {
		name  string
		owner string
		kind  string
		pref  DelegationPreference
		probe DelegationProbe
		want  string
	}{
		{"no owner", "", "runCommand", enabledPref([]string{"runCommand"}, []string{"claude-code"}), online, DelegationReasonNoOwner},
		{"switched off", "u", "runCommand", DelegationPreference{EligibleKinds: []string{"runCommand"}, AppOrder: []string{"claude-code"}}, online, DelegationReasonPolicyOff},
		{"kind not eligible", "u", "browseUrl", enabledPref([]string{"runCommand"}, []string{"claude-code"}), online, DelegationReasonKindNotEligible},
		{"no apps configured", "u", "runCommand", enabledPref([]string{"runCommand"}, nil), online, DelegationReasonNoAppOrder},
		{"delegated", "u", "runCommand", enabledPref([]string{"runCommand"}, []string{"claude-code"}), online, DelegationReasonDelegated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideDelegation(context.Background(), tc.owner, tc.kind, tc.pref, tc.probe)
			if got.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.want)
			}
		})
	}
}

// TestDelegationChecksConcurrencyBeforeSelecting: selecting first and
// then refusing would report "no machine online" for a user who has
// several, sending them to investigate the wrong thing.
func TestDelegationChecksConcurrencyBeforeSelecting(t *testing.T) {
	probe := &stubProbe{online: map[string]string{"claude-code": "reg-a"}, live: 2}
	pref := enabledPref([]string{"runCommand"}, []string{"claude-code"})
	pref.MaxConcurrentSessions = 2

	got := DecideDelegation(context.Background(), "u", "runCommand", pref, probe)
	if got.Delegate {
		t.Fatal("delegated past the concurrency cap")
	}
	if got.Reason != DelegationReasonConcurrencyReached {
		t.Fatalf("reason = %q, want %q", got.Reason, DelegationReasonConcurrencyReached)
	}
	if len(probe.asked) != 0 {
		t.Fatalf("probed %v before checking the cap", probe.asked)
	}

	// Zero reads as the DEFAULT of 1, not as "none" -- a zero here
	// would silently disable a feature the user turned on.
	pref.MaxConcurrentSessions = 0
	idle := &stubProbe{online: map[string]string{"claude-code": "reg-a"}, live: 0}
	if got := DecideDelegation(context.Background(), "u", "runCommand", pref, idle); !got.Delegate {
		t.Fatalf("maxConcurrentSessions=0 must read as the default, got %q", got.Reason)
	}
	busy := &stubProbe{online: map[string]string{"claude-code": "reg-a"}, live: 1}
	if got := DecideDelegation(context.Background(), "u", "runCommand", pref, busy); got.Delegate {
		t.Fatal("maxConcurrentSessions=0 must still cap at 1")
	}
}
