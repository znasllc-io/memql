package wholesalepack

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/parser"
)

// gate_test.go -- EVERY ACT A STORE'S PERSON TAKES PASSES THE ORGANIZATION GATE.
//
// `execute app:deployables/wholesale` is a Deployables part: a person granted it
// in one organization must not act on another's applications with it. The
// shared gate (component/memql/organization_action_capabilities.go) can only
// check the right organization when it knows which row a call is about, so each
// gated builtin declares the part AND requires the argument the gate resolves
// from: the application (whose accountId was copied off its site) or the site.
// Pinned against the shipped DSL, so a rename on either side fails here; the
// gate's own table is pinned by component/memql/wholesale_capability_test.go.
// The pattern is packs/reviewspack/gate_test.go's.

// gatedBuiltins is every builtin the wholesale part gates, with the concept and
// argument the organization gate resolves its target from. A name a later task
// declares is listed now and checked once the tree declares it.
var gatedBuiltins = map[string][2]string{
	"wholesaleRecordDecision":  {"v1:wholesale:application", "applicationId"},
	"wholesaleGet":             {"v1:wholesale:application", "applicationId"},
	"wholesaleRetryAccess":     {"v1:wholesale:application", "applicationId"},
	"wholesaleRemoveBuyer":     {"v1:wholesale:application", "applicationId"},
	"wholesaleResendEmail":     {"v1:wholesale:application", "applicationId"},
	"wholesaleList":            {"v1:platform:site", "siteId"},
	"wholesaleDecisionLog":     {"v1:platform:site", "siteId"},
	"wholesaleBuyers":          {"v1:platform:site", "siteId"},
	"wholesaleStoreSettings":   {"v1:platform:site", "siteId"},
	"wholesaleUpdateSettings":  {"v1:platform:site", "siteId"},
	"wholesaleDocuments":       {"v1:platform:site", "siteId"},
	"wholesalePublishDocument": {"v1:platform:site", "siteId"},
	"wholesaleAddBuyer":        {"v1:platform:site", "siteId"},
	"wholesaleExportStore":     {"v1:platform:site", "siteId"},
}

func parsedBuiltins(t *testing.T) map[string]*ast.BuiltinDecl {
	t.Helper()
	raw, err := fs.ReadFile(Tree(), "builtins.memql")
	if err != nil {
		t.Fatal(err)
	}
	src, err := parser.NormaliseAll(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(src)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.BuiltinDecl{}
	for _, def := range file.Definitions {
		if b, ok := def.(*ast.BuiltinDecl); ok {
			out[b.Name] = b
		}
	}
	return out
}

func declaresWholesalePart(b *ast.BuiltinDecl) bool {
	for _, a := range b.Attributes {
		if a.Name == "requiresCapability" && strings.Contains(fmt.Sprint(a.Value, a.Args), "app:deployables/wholesale") &&
			strings.Contains(fmt.Sprint(a.Value, a.Args), "execute") {
			return true
		}
	}
	return false
}

func TestEveryGatedBuiltinDeclaresTheWholesalePartAndItsTarget(t *testing.T) {
	builtins := parsedBuiltins(t)
	declared := 0
	for name, target := range gatedBuiltins {
		b, ok := builtins[name]
		if !ok {
			continue // a later task declares it
		}
		declared++
		if !declaresWholesalePart(b) {
			t.Errorf("%s does not declare @requiresCapability(\"execute\", \"app:deployables/wholesale\")", name)
		}
		required := false
		for _, f := range b.Fields {
			if f.Name == target[1] {
				required = f.Required
			}
		}
		if !required {
			t.Errorf("%s does not require %q, the argument the organization gate resolves its target from", name, target[1])
		}
	}
	// THE REACHABLE POSITIVE: the four this pack declares today must be found,
	// or a walk that found nothing would pass for the wrong reason.
	for _, name := range []string{"wholesaleRecordDecision", "wholesaleRetryAccess", "wholesaleStoreSettings", "wholesaleUpdateSettings"} {
		if _, ok := builtins[name]; !ok {
			t.Errorf("%s is not declared", name)
		}
	}
	if declared < 4 {
		t.Errorf("found %d gated builtins in the tree, want at least 4", declared)
	}
	// A BUILTIN CARRYING THE PART THAT THE TABLE DOES NOT NAME would be judged
	// against the caller's global capability alone.
	for name, b := range builtins {
		if _, listed := gatedBuiltins[name]; declaresWholesalePart(b) && !listed {
			t.Errorf("%s declares the wholesale part but names no organization target in gatedBuiltins", name)
		}
	}
	// The shopper's builtin carries no part, and neither do the automation's and
	// the operators': a shopper holds no capability, an automation arrives with
	// internal origin, and setting a store up is the operators' alone.
	for _, name := range []string{"wholesaleSubmitApplication", "wholesaleProvisionEntitlement", "wholesaleRevokeEntitlement",
		"wholesaleSetSettings", "wholesaleBackfillAccounts"} {
		if b, ok := builtins[name]; !ok || declaresWholesalePart(b) {
			t.Errorf("%s must be declared, and carry no part", name)
		}
	}
}
