package wholesalepack_test

import (
	"io/fs"
	"strings"
	"testing"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// adapter_constructs_test.go -- THE SECOND THING A FAKE CALLER CANNOT CATCH
// (epic memql#5533, issues memql#5558 and memql#5559).
//
// The pack reaches Shopify through construct NAMES, deliberately: it must
// not import integrations/shopify, so the seam is a string. A string seam
// buys client-agnosticism and costs exactly one thing -- the compiler stops
// checking it. Every test that drives an adapter does so through a
// fakeCaller, which answers whatever name it is handed, so a typo in
// "shopifyTagWholesaleCustomer" would leave this package entirely green and
// fail at runtime the first time a merchant approved somebody.
//
// So the names are checked against the ENGINE'S OWN EMBEDDED TREE, which is
// where they have to exist for the call to resolve.

// adapterConstructs is every construct name this pack's shipped adapters
// call. Kept here rather than derived, because deriving it from the adapter
// source would re-introduce exactly the indirection this test exists to
// remove.
var adapterConstructs = []string{
	"shopifyProvisionWholesale",
	"shopifyRevokeWholesale",
	"shopifyTagWholesaleCustomer",
	"shopifyUntagWholesaleCustomer",
}

func TestEveryConstructAnAdapterCallsIsDeclared(t *testing.T) {
	raw, err := fs.ReadFile(memqldsl.Tree(), "shopify/overlay/builtins.memql")
	if err != nil {
		t.Fatalf("the Shopify overlay's builtins must be in the embedded tree: %v", err)
	}
	declared := map[string]struct{}{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "builtin" {
			declared[strings.TrimSuffix(fields[1], "{")] = struct{}{}
		}
	}
	for _, name := range adapterConstructs {
		if _, ok := declared[name]; !ok {
			t.Errorf("the %q construct an adapter calls is not declared in "+
				"dsl/shopify/overlay/builtins.memql. The pack reaches Shopify by NAME so it "+
				"need not import the integration, and the compiler cannot check a string -- "+
				"this test is what does", name)
		}
	}
}

// AND THE PACK'S OWN BUILTINS MUST NAME CAPABILITIES THAT EXIST. Same class
// of gap, one layer in: @executor("integration.wholesale.x") resolves to the
// capability named x on this pack's provider, and nothing but a running
// engine would otherwise notice a mismatch.
func TestEveryPackBuiltinNamesARealCapability(t *testing.T) {
	raw, err := fs.ReadFile(wholesaleTree(t), "builtins.memql")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = `@executor("integration.wholesale.`
	var executors []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		rest := strings.TrimPrefix(trimmed, prefix)
		if idx := strings.Index(rest, `"`); idx > 0 {
			executors = append(executors, rest[:idx])
		}
	}
	if len(executors) == 0 {
		t.Fatal("the pack declares no wholesale executors; builtins.memql is not being read")
	}
	capabilities := map[string]struct{}{}
	for _, c := range newTestProvider(t).Capabilities() {
		capabilities[c.Name] = struct{}{}
	}
	for _, name := range executors {
		if _, ok := capabilities[name]; !ok {
			t.Errorf("builtins.memql routes to integration.wholesale.%s, which this pack's "+
				"provider does not expose as a capability", name)
		}
	}
}
