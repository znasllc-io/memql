package memql

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE ACCESS CODES PART REACHES THE SHARED ORGANIZATION GATE (deployable access
// codes design record, 2026-10-29, section 5 "Authority").
//
// EVERY CLIENT-CALLABLE READ AND WRITE ON ACCESS CODES IS A BUILTIN PASSING
// THE SAME GATE, and every query and mutation underneath is @serverOnly. That is one
// sentence with a reason and a test. The reason: the shared gate judges builtins
// and mutations and NEVER a query, so a client-callable query over these rows
// would be a read that skips the organization check -- a
// person holding the part in one organization could read the labels, hints and
// expiries of a site in another whose row tier admits them. The tests below are
// the proof, in two halves: the declarations (this file) and the gate actually
// refusing (site_access_contract_db_test.go).
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
// The split is the spec's: minting, listing the codes and listing the gates are
// asked about the SITE (minting has no code yet, and the other two ask about every
// code of one site), and every act on an existing code is asked about the CODE,
// whose accountId was copied off its site at mint time -- so an act on a code
// resolves its organization without reading the site again.
func TestSiteAccessBuiltinsNameTheirOrganizationTarget(t *testing.T) {
	fns := loadedSiteAccessTree(t)

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
		{"siteAccessCodesForSite", "integration.siteAccess.list", "v1:platform:site", "siteId"},
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

}

// THE LIST IS A BUILTIN, AND THE QUERY UNDER IT IS NOT REACHABLE. Pinned from both
// sides: a client-callable query is one the organization gate never judges, so
// siteAccessCodesForSite is the builtin above, and the rows come from
// siteAccessCodeRowsForSite, a @serverOnly query that carries no part because nothing
// a person calls reaches it.
func TestSiteAccessCodesForSiteIsABuiltinOverAServerOnlyQuery(t *testing.T) {
	fns := loadedSiteAccessTree(t)

	list, err := fns.Get("siteAccessCodesForSite")
	if err != nil || list == nil {
		t.Fatalf("siteAccessCodesForSite is not in the loaded tree: %v", err)
	}
	if list.FunctionKind != "builtin" {
		t.Fatalf("siteAccessCodesForSite is a %s: a client-callable query is a read the organization gate never judges", list.FunctionKind)
	}

	rows, err := fns.Get("siteAccessCodeRowsForSite")
	if err != nil || rows == nil {
		t.Fatalf("siteAccessCodeRowsForSite is not in the loaded tree: %v", err)
	}
	if rows.FunctionKind != "query" || !rows.ServerOnly {
		t.Errorf("siteAccessCodeRowsForSite is a %s with ServerOnly=%v, want a @serverOnly query", rows.FunctionKind, rows.ServerOnly)
	}
	if rows.RequiresCapability.declared() {
		t.Errorf("siteAccessCodeRowsForSite declares %v: a part on a read nobody calls only reads as if it were the gate", rows.RequiresCapability)
	}
}

// EVERYTHING A CLIENT CAN CALL OVER ACCESS CODES PASSES THE ONE GATE. Walks the
// loaded tree rather than a list of names, so a seventh construct added later is
// held to the same rule without anybody remembering to extend a table:
//
//   - every query and mutation bound to the code or session concept is @serverOnly;
//   - every builtin executed by the siteAccess integration declares the part and
//     names an organization target the shared gate can judge.
func TestEveryClientCallableAccessCodeActIsAGatedBuiltin(t *testing.T) {
	fns := loadedSiteAccessTree(t)

	boundConcepts := map[string]bool{"v1:platform:siteAccessCode": true, "v1:platform:siteAccessSession": true}
	var underneath, builtins int
	for _, fn := range fns.List() {
		if fn == nil {
			continue
		}
		if boundConcepts[fn.BoundConcept] && (fn.FunctionKind == "query" || fn.FunctionKind == "mutation") {
			underneath++
			if !fn.ServerOnly {
				t.Errorf("%s %s over %s is client-callable: the organization gate never judges a query, and a mutation here skips the audit event and the mint checks. Make it @serverOnly and reach it through a builtin",
					fn.FunctionKind, fn.Name, fn.BoundConcept)
			}
		}
		if fn.FunctionKind == "builtin" && strings.HasPrefix(fn.Executor, "integration.siteAccess.") {
			builtins++
			if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/access"}) {
				t.Errorf("builtin %s declares %v, want execute on app:deployables/access", fn.Name, fn.RequiresCapability)
			}
			if concept, argument := organizationCapabilityTarget(fn); concept == "" || argument == "" {
				t.Errorf("builtin %s names no organization target, so the shared gate judges it against the caller's global capability alone", fn.Name)
			}
		}
	}
	// THE REACHABLE POSITIVE. A walk that found nothing would pass for the wrong reason.
	if underneath < 11 {
		t.Errorf("found %d queries and mutations over access codes, want at least the 11 the contract declares; the walk or the bound concepts changed", underneath)
	}
	if builtins != 6 {
		t.Errorf("found %d siteAccess builtins, want the 6 the contract declares", builtins)
	}
}

func loadedSiteAccessTree(t *testing.T) *FunctionRegistry {
	t.Helper()
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
	return fns
}
