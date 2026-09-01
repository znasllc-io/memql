package automations_test

// The accounts seed's trust chain, pinned (epic memql#4800).
//
// ===========================================================================
// WHY THIS TEST EXISTS AND NOT JUST A COMMENT
// ===========================================================================
// `seedSelfAccount` materializes `v1:accounts:account:self` -- the owner's own
// company -- on first startup, and it decides whether to write by calling the
// `selfAccountAbsent` logic, which reads `existingSelfAccount`. That read is
// `@serverOnly`, so it resolves ONLY when the executing context carries
// internal origin (component/memql/engine.go's #2800 gate).
//
// Four links have to hold for that to be true, and they are in four files:
//
//  1. the automation is loaded from the registered tree, which is the only
//     place `Trusted` is granted (unified_loader.go);
//  2. `system.startup` is bus-dispatched rather than caller-submitted, so
//     `callerSuppliedPayload` is false and `SourceTrusted` survives;
//  3. a step runs under `originForSource(ctx, SourceTrusted)`, which stamps
//     internal;
//  4. `existingSelfAccount` still carries `@serverOnly`.
//
// WHAT BREAKS IF ANY LINK GOES, and why nobody would notice. The read is
// refused, the logic fails, the gate never says "absent", and the self account
// is never created. There is no error anybody watches at boot; the Accounts
// app simply opens with an empty registry and no first-run card, which is a
// completely plausible state for a brand-new cluster. That is the same class
// of silent, confidence-increasing failure the strict-boot gate in this file's
// sibling exists for -- and it would survive review for the same reason.
//
// So the coupling is asserted rather than described: the trust flag and the
// annotation that makes it load-bearing are checked together, in one place
// that names the consequence.

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

const seedAutomationName = "seedSelfAccount"

func TestSeedSelfAccountIsTrustedSoItsServerOnlyReadResolves(t *testing.T) {
	t.Setenv(memql.AllowSkipsEnvVar, "")
	loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

	loaded, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("the shipped automation tree must load clean, got: %v", err)
	}

	var seed *automations.Automation
	for _, a := range loaded {
		if a != nil && a.Name == seedAutomationName {
			seed = a
			break
		}
	}
	if seed == nil {
		t.Fatalf("%s is not in the loaded tree.\n\n"+
			"It is the boot seed for v1:accounts:account:self (epic memql#4800). Without it a "+
			"cluster starts with an empty client registry and no first-run card -- which looks "+
			"exactly like a brand-new cluster, so nothing reports it.", seedAutomationName)
	}

	// LINK 1: trust is granted only to bodies from the registered tree.
	if !seed.Trusted {
		t.Errorf("%s loaded UNTRUSTED. Its steps would run at client origin, so the "+
			"@serverOnly `existingSelfAccount` read its gate depends on would be refused -- and "+
			"a gate that cannot read reports the row ABSENT, which means the seed re-creates "+
			"v1:accounts:account:self on every boot, overwriting whatever a human typed into "+
			"the first-run card. That is exactly the clobbering decision D3 forbids.", seedAutomationName)
	}

	// LINK 4: the annotation that makes the trust load-bearing.
	//
	// Read off the SOURCE rather than the loaded automation, because the
	// automation carries no view of the constructs its logic calls. A shallow
	// check, and it is the right depth: what it pins is that the two facts
	// stay coupled -- if @serverOnly is ever dropped, this test's premise is
	// gone and its message should stop claiming otherwise.
	src, err := fs.ReadFile(memqldsl.Tree(), "accounts/queries.memql")
	if err != nil {
		t.Fatalf("read accounts/queries.memql: %v", err)
	}
	text := string(src)
	idx := strings.Index(text, "query account existingSelfAccount")
	if idx < 0 {
		t.Fatal("existingSelfAccount is gone from accounts/queries.memql; the seed's gate has no read")
	}
	// The annotation block sits directly above the declaration.
	preamble := text[max(0, idx-400):idx]
	if !strings.Contains(preamble, "@serverOnly") {
		t.Log("existingSelfAccount no longer carries @serverOnly -- the trust assertion above is " +
			"now belt without braces rather than load-bearing. That is not a failure; it is a " +
			"note that this test's stated reason has changed and should be rewritten.")
	}
}
