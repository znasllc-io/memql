package memql

import (
	"io"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE ACCESS CODES PART REACHES THE SHARED ORGANIZATION GATE (deployable access
// codes design record, 2026-10-29, section 5 "Authority").
//
// `execute app:deployables/access` is a part like `preview`: a deployable
// belongs to an organization, and a person granted the part in ONE organization
// must not act on another's codes with it. The shared gate
// (refuseOrganizationActionCapability) can only check the selected organization
// when it knows which row the call is about, and it learns that from two tables
// in organization_action_capabilities.go -- which resources it judges at all,
// and which argument of which concept each builtin's target is. A builtin in
// neither table is judged against the caller's GLOBAL capability alone, which
// looks identical on the page and silently lets an `execute` held in one
// organization act on a code in another.
//
// So the targets are pinned here against the SHIPPED tree rather than against a
// hand-built Function: the names and the argument spellings below are the ones
// dsl/platform/builtins.memql declares, and a rename on either side fails this
// before it can turn the gate off.
//
// The split is the spec's: minting and listing gates are asked about the SITE
// (there is no code yet), and every act on an existing code is asked about the
// CODE, whose accountId was copied off its site at mint time -- so an act on a
// code resolves its organization without reading the site again.
func TestSiteAccessBuiltinsNameTheirOrganizationTarget(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fns := newFunctionRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, _, err := LoadUnifiedFunctions(logger, fns, memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if _, err := LoadUnifiedBuiltins(logger, fns); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}

	if !organizationDeployableCapability("app:deployables/access") {
		t.Fatal("app:deployables/access is not an organization deployable capability, so every act under it is judged against the caller's global capability alone")
	}

	for _, tc := range []struct {
		builtin  string
		executor string
		concept  string
		argument string
	}{
		{"siteAccessCodeCreate", "integration.siteAccess.create", "v1:platform:site", "siteId"},
		{"siteAccessGates", "integration.siteAccess.gates", "v1:platform:site", "siteId"},
		{"siteAccessCodeRegenerate", "integration.siteAccess.regenerate", "v1:platform:siteAccessCode", "codeId"},
		{"siteAccessCodeSetExpiry", "integration.siteAccess.setExpiry", "v1:platform:siteAccessCode", "codeId"},
		{"siteAccessCodeRemove", "integration.siteAccess.remove", "v1:platform:siteAccessCode", "codeId"},
	} {
		t.Run(tc.builtin, func(t *testing.T) {
			fn, err := fns.Get(tc.builtin)
			if err != nil || fn == nil {
				t.Fatalf("%s is not in the loaded tree: %v", tc.builtin, err)
			}
			if fn.FunctionKind != "builtin" {
				t.Fatalf("%s is a %s, want a builtin", tc.builtin, fn.FunctionKind)
			}
			if fn.Executor != tc.executor {
				t.Errorf("%s runs %q, want %q", tc.builtin, fn.Executor, tc.executor)
			}
			if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/access"}) {
				t.Errorf("%s declares %v, want execute on app:deployables/access", tc.builtin, fn.RequiresCapability)
			}
			concept, argument := organizationCapabilityTarget(fn)
			if concept != tc.concept || argument != tc.argument {
				t.Errorf("%s resolves its organization target to (%q, %q), want (%q, %q)",
					tc.builtin, concept, argument, tc.concept, tc.argument)
			}
		})
	}

	// THE READ IS NOT A TARGETED ACT. siteAccessCodesForSite is a query, which
	// organizationCapabilityTarget never judges; the organization boundary on
	// the rows it returns is the concept's tier, so the read is pinned as
	// untargeted rather than left to look like an omission.
	read, err := fns.Get("siteAccessCodesForSite")
	if err != nil || read == nil {
		t.Fatalf("siteAccessCodesForSite is not in the loaded tree: %v", err)
	}
	if concept, argument := organizationCapabilityTarget(read); concept != "" || argument != "" {
		t.Errorf("the query siteAccessCodesForSite acquired an organization target (%q, %q)", concept, argument)
	}
}
