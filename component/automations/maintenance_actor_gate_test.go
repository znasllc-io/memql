package automations

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// THE GATE on the cluster's maintenance principal (memql#4366 / memql#4406).
//
// component/auth/maintenance_actor.go holds a compiled-in list of automations
// that run as a synthetic CLUSTER OWNER instead of the ordinary RoleReader
// system actor. That is the largest privilege any automation can hold, and the
// list is one line per grant, so this gate makes an addition a visible, argued
// change rather than an edit nobody has to defend.
//
// Four properties, and each one closes a way the list could quietly go wrong:
//
//  1. Every entry is PINNED here. Adding a grant means editing two files, one
//     of which is a test whose whole subject is why the grant exists.
//  2. Every entry carries a non-empty reason.
//  3. Every entry names an automation that ACTUALLY LOADS from this repo's dsl/
//     tree. That is what "engine-owned" means operationally: a product bundle
//     mounted at MEMQL_DSL_PATH contributes automations at runtime, and a name
//     that resolves to nothing here would be a grant waiting for whoever
//     happens to define it.
//  4. The JOIN holds -- contextWithSystemActor really does yield a cluster
//     owner for a listed name and really does not for anything else. The list
//     and the constructor are in a different package from the executor that
//     consumes them, so nothing else asserts that the wire is connected.
func TestMaintenanceAutomationsAreArgued(t *testing.T) {
	// (1) The pinned set.
	want := []string{"workerInvocationRetentionSweep"}
	got := auth.MaintenanceAutomationNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the maintenance list is %v, pinned as %v.\n\n"+
			"An addition is the most privileged change available to an automation: it runs as a "+
			"synthetic CLUSTER OWNER, past every owned-tier @rowAuthz in the tree. Two properties an "+
			"entry must have (component/auth/maintenance_actor.go states them in full):\n"+
			"  - the automation is ENGINE-OWNED, living in this repo's dsl/ tree;\n"+
			"  - its reads span owners BY NATURE -- a sweep, a cross-owner roll-up -- not merely\n"+
			"    finding an owner-scoped read inconvenient. For that, borrow ONE owner's authority\n"+
			"    with auth.ContextWithUserActor at the call site instead.\n\n"+
			"If the change is right, update this pin and say why in the commit.", got, want)
	}

	// (2) Reasons.
	for _, name := range got {
		if strings.TrimSpace(auth.MaintenanceAutomationReason(name)) == "" {
			t.Errorf("%q is on the maintenance list with no reason recorded", name)
		}
	}

	// (3) Engine-owned: the name resolves in the embedded tree.
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	loaded, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	names := map[string]bool{}
	for _, a := range loaded {
		if a != nil {
			names[a.Name] = true
		}
	}
	// REACHABLE POSITIVE: without this, an empty load makes every assertion
	// below vacuous and the gate reports success over nothing.
	if len(names) == 0 {
		t.Fatal("the embedded tree yielded no automations, so the membership checks below would pass over nothing")
	}
	for _, name := range got {
		if !names[name] {
			t.Errorf("%q is on the maintenance list but no automation of that name loads from this repo's "+
				"dsl/ tree (%d loaded). A grant that names nothing is a grant waiting for whoever defines "+
				"the name -- including a product bundle mounted at MEMQL_DSL_PATH.", name, len(names))
		}
	}

	// (4) The join, in both directions.
	for _, name := range got {
		ac, ok := auth.AccessFromContext(contextWithSystemActor(context.Background(), name))
		if !ok || !ac.IsClusterOwner() {
			t.Errorf("contextWithSystemActor(%q) is not a cluster owner, so the grant is declared and not "+
				"wired -- and every read it exists to enable returns zero rows, silently", name)
		}
	}
	unlisted := "anAutomationNobodyGranted"
	if auth.IsMaintenanceAutomation(unlisted) {
		t.Fatalf("%q reads as listed; pick a name that is not", unlisted)
	}
	ac, ok := auth.AccessFromContext(contextWithSystemActor(context.Background(), unlisted))
	if !ok {
		t.Fatal("contextWithSystemActor stamped no AccessContext at all")
	}
	if ac.IsClusterOwner() {
		t.Errorf("contextWithSystemActor(%q) is a CLUSTER OWNER and that name is on no list. Every "+
			"automation in the tree would hold the grant, which is the blanket elevation the named "+
			"principal exists instead of.", unlisted)
	}
	if ac.Role != auth.RoleReader {
		t.Errorf("the ordinary system actor carries role %q, want %q -- memql#2801's guarantee for a "+
			"caller with no identity of its own", ac.Role, auth.RoleReader)
	}
}

// TestMaintenanceActorIsNotMintableFromARequest pins the property that makes an
// identity safer than an enforcement bypass: the elevation comes from the
// compiled-in list, so nothing a caller supplies can reach it.
func TestMaintenanceActorIsNotMintableFromARequest(t *testing.T) {
	for _, name := range []string{
		"",
		"   ",
		"workerInvocationRetentionSweep ",    // trailing space: trimmed, so this one IS granted
		"workerInvocationRetentionSweepEvil", // prefix of a listed name
		"xworkerInvocationRetentionSweep",    // suffix of a listed name
		"system:maintenance:anything",        // the stamp's own prefix, asked for by name
		"WORKERINVOCATIONRETENTIONSWEEP",     // case
	} {
		ac := auth.MaintenanceActor(name)
		granted := ac != nil
		wantGranted := strings.TrimSpace(name) == "workerInvocationRetentionSweep"
		if granted != wantGranted {
			t.Errorf("MaintenanceActor(%q) granted=%v, want %v", name, granted, wantGranted)
		}
	}
	// Trimming is the ONE normalisation, and it is deliberate: a name arrives
	// from an automation's own declaration, where trailing whitespace is a
	// formatting artefact rather than a different automation. Case folding is
	// NOT applied, because two automations differing only in case would be two
	// automations.
	sorted := auth.MaintenanceAutomationNames()
	if !sort.StringsAreSorted(sorted) {
		t.Errorf("MaintenanceAutomationNames is not sorted: %v", sorted)
	}
}
