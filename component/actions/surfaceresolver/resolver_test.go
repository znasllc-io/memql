package surfaceresolver

import (
	"reflect"
	"testing"
)

func wb(prio int, avail bool, caps ...string) Surface {
	return Surface{ID: "wb", Slug: "workbench", Kind: "workbench", Capabilities: caps, Available: avail, Priority: prio}
}
func cu(prio int, avail bool, caps ...string) Surface {
	return Surface{ID: "cu1", Slug: "computer-use:m1", Kind: "computer-use", MachineID: "m1", Capabilities: caps, Available: avail, Priority: prio}
}

func TestResolve_PrecedenceExplicitOverPolicyOverAvailability(t *testing.T) {
	surfaces := []Surface{wb(0, true, "fs.writeFile"), cu(1, true, "fs.writeFile")}
	// availability failover: lowest priority (workbench) wins by default.
	s, err := Resolve("fs.writeFile", "", "", surfaces)
	if err != nil || s.Slug != "workbench" {
		t.Fatalf("availability default: want workbench, got %v (%v)", s.Slug, err)
	}
	// policy default overrides availability order.
	s, _ = Resolve("fs.writeFile", "", "computer-use:m1", surfaces)
	if s.Slug != "computer-use:m1" {
		t.Fatalf("policy default: want computer-use:m1, got %v", s.Slug)
	}
	// explicit overrides policy.
	s, _ = Resolve("fs.writeFile", "workbench", "computer-use:m1", surfaces)
	if s.Slug != "workbench" {
		t.Fatalf("explicit: want workbench, got %v", s.Slug)
	}
}

func TestResolve_AvailabilityFailover(t *testing.T) {
	// workbench down -> next available (computer-use) is chosen.
	surfaces := []Surface{wb(0, false, "fs.writeFile"), cu(1, true, "fs.writeFile")}
	s, err := Resolve("fs.writeFile", "", "", surfaces)
	if err != nil || s.Slug != "computer-use:m1" {
		t.Fatalf("failover: want computer-use:m1, got %v (%v)", s.Slug, err)
	}
}

func TestResolve_NoSurfaceServes(t *testing.T) {
	surfaces := []Surface{wb(0, true, "fs.readFile")}
	if _, err := Resolve("shell.exec", "", "", surfaces); err == nil {
		t.Fatalf("expected error when no surface serves the capability")
	}
}

func TestResolve_ExplicitUnavailableErrors(t *testing.T) {
	surfaces := []Surface{wb(0, false, "fs.writeFile"), cu(1, true, "fs.writeFile")}
	if _, err := Resolve("fs.writeFile", "workbench", "", surfaces); err == nil {
		t.Fatalf("explicit-but-unavailable must error, not silently failover")
	}
}

func TestWorldGroups_UnionFind(t *testing.T) {
	// calls 0->1 coupled (write then read /a); call 2 independent.
	edges := []ResourceEdge{{FromCallIndex: 0, ToCallIndex: 1, Resource: "/a"}}
	got := WorldGroups(3, edges)
	if got[0] != got[1] {
		t.Fatalf("coupled calls 0,1 must share a group: %v", got)
	}
	if got[2] == got[0] {
		t.Fatalf("independent call 2 must be its own group: %v", got)
	}
}

func TestWorldGroups_TransitiveChain(t *testing.T) {
	// 0->1, 1->2 couples all three into one world.
	edges := []ResourceEdge{
		{FromCallIndex: 0, ToCallIndex: 1, Resource: "/a"},
		{FromCallIndex: 1, ToCallIndex: 2, Resource: "/b"},
	}
	got := WorldGroups(3, edges)
	if !(got[0] == got[1] && got[1] == got[2]) {
		t.Fatalf("transitive chain must be one group: %v", got)
	}
}

func TestResolvePlan_DecisionA_GroupPinsToSingleSurface(t *testing.T) {
	// Coupled write+read; only computer-use serves BOTH caps -> whole group
	// pins to computer-use even though workbench serves the write.
	surfaces := []Surface{
		wb(0, true, "fs.writeFile"),
		cu(1, true, "fs.writeFile", "fs.readFile"),
	}
	calls := []Call{
		{Index: 0, Capability: "fs.writeFile"},
		{Index: 1, Capability: "fs.readFile"},
	}
	edges := []ResourceEdge{{FromCallIndex: 0, ToCallIndex: 1, Resource: "/a"}}
	plan, err := ResolvePlan(calls, edges, "", "", surfaces)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Resolutions[0].Surface.ID != plan.Resolutions[1].Surface.ID {
		t.Fatalf("decision A: coupled calls must pin to ONE surface, got %v and %v",
			plan.Resolutions[0].Surface.Slug, plan.Resolutions[1].Surface.Slug)
	}
	if plan.Resolutions[0].Surface.Slug != "computer-use:m1" {
		t.Fatalf("group must pin to the surface serving both caps, got %v", plan.Resolutions[0].Surface.Slug)
	}
	if len(plan.Transfers) != 0 {
		t.Fatalf("a single-surface group needs no transfer, got %v", plan.Transfers)
	}
}

func TestResolvePlan_ForcedCrossWorld_EmitsTransfer(t *testing.T) {
	// Coupled write+read, but workbench serves ONLY write and computer-use
	// serves ONLY read: no single surface serves the whole group, so the
	// calls split and the forced crossing is reported as a transfer.
	surfaces := []Surface{
		wb(0, true, "fs.writeFile"),
		cu(1, true, "fs.readFile"),
	}
	calls := []Call{
		{Index: 0, Capability: "fs.writeFile"},
		{Index: 1, Capability: "fs.readFile"},
	}
	edges := []ResourceEdge{{FromCallIndex: 0, ToCallIndex: 1, Resource: "/a"}}
	plan, err := ResolvePlan(calls, edges, "", "", surfaces)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Resolutions[0].Surface.ID == plan.Resolutions[1].Surface.ID {
		t.Fatalf("forced cross-world calls must land on different surfaces")
	}
	if len(plan.Transfers) != 1 {
		t.Fatalf("forced cross-world must emit exactly one transfer, got %v", plan.Transfers)
	}
	tr := plan.Transfers[0]
	if tr.Resource != "/a" || tr.FromCall != 0 || tr.ToCall != 1 {
		t.Fatalf("transfer mis-described: %+v", tr)
	}
}

func TestResolvePlan_IndependentCallsResolveSeparately(t *testing.T) {
	surfaces := []Surface{wb(0, true, "fs.writeFile", "shell.exec")}
	calls := []Call{
		{Index: 0, Capability: "fs.writeFile"},
		{Index: 1, Capability: "shell.exec"},
	}
	plan, err := ResolvePlan(calls, nil, "", "", surfaces)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := []string{plan.Resolutions[0].Surface.Slug, plan.Resolutions[1].Surface.Slug}
	if !reflect.DeepEqual(got, []string{"workbench", "workbench"}) {
		t.Fatalf("want both on workbench, got %v", got)
	}
	// distinct groups (no edges)
	if plan.Resolutions[0].GroupID == plan.Resolutions[1].GroupID {
		t.Fatalf("uncoupled calls should be in distinct groups")
	}
}

// cockpitRunner mirrors the engine's cockpit/runner control surface: it
// declares namespace WILDCARDS, which must match concrete verbs.
func cockpitRunner(prio int, avail bool) Surface {
	return Surface{
		ID:           "cockpit/runner",
		Slug:         "cockpit/runner",
		Kind:         "cockpit-runner",
		Capabilities: []string{"shell.*", "fs.*", "integration.argocd.*", "integration.github.*"},
		Available:    avail,
		Priority:     prio,
	}
}

func TestResolve_WildcardCapabilityMatch(t *testing.T) {
	surfaces := []Surface{cockpitRunner(50, true)}
	for _, capability := range []string{"shell.exec", "fs.writeFile", "integration.argocd.sync", "integration.github.tagRelease"} {
		s, err := Resolve(capability, "", "", surfaces)
		if err != nil || s.Slug != "cockpit/runner" {
			t.Fatalf("wildcard match %q: want cockpit/runner, got %v (%v)", capability, s.Slug, err)
		}
	}
	// A sibling namespace must NOT match "fs.*".
	if _, err := Resolve("fsx.writeFile", "", "", surfaces); err == nil {
		t.Fatalf("fs.* must not match the fsx namespace")
	}
	// An unlisted namespace is unserved.
	if _, err := Resolve("mcp.invoke", "", "", surfaces); err == nil {
		t.Fatalf("cockpit/runner must not serve mcp.*")
	}
}

// TestResolve_DeployActionPrecedenceOverCockpitRunner exercises the §7.2
// precedence with cockpit/runner as the policy default (the deploy-action
// case): explicit wins; else the cockpit/runner policy default; else
// availability.
func TestResolve_DeployActionPrecedenceOverCockpitRunner(t *testing.T) {
	surfaces := []Surface{cockpitRunner(50, true), wb(10, true, "shell.exec")}

	// policy default = cockpit/runner pins shell.exec to the runner even though
	// the lower-priority workbench is available (would win on availability).
	s, err := Resolve("shell.exec", "", "cockpit/runner", surfaces)
	if err != nil || s.Slug != "cockpit/runner" {
		t.Fatalf("policy default: want cockpit/runner, got %v (%v)", s.Slug, err)
	}
	// explicit overrides the cockpit/runner policy default.
	s, _ = Resolve("shell.exec", "workbench", "cockpit/runner", surfaces)
	if s.Slug != "workbench" {
		t.Fatalf("explicit over policy: want workbench, got %v", s.Slug)
	}
	// no policy: availability failover (workbench lower priority) wins.
	s, _ = Resolve("shell.exec", "", "", surfaces)
	if s.Slug != "workbench" {
		t.Fatalf("availability: want workbench, got %v", s.Slug)
	}
}
