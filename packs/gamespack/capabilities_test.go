package gamespack

import (
	"io/fs"
	"regexp"
	"sort"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// Every builtin the DSL declares has a Go capability, and every capability
// is declared: an executor with no handler is "unknown builtin executor" at
// the first call, and a handler with no declaration is unreachable.
func TestCapabilitiesMatchTheDeclaredBuiltins(t *testing.T) {
	raw, err := fs.ReadFile(Tree(), "builtins.memql")
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, m := range regexp.MustCompile(`@executor\("integration\.games\.([A-Za-z]+)"\)`).FindAllStringSubmatch(string(raw), -1) {
		declared = append(declared, m[1])
	}
	var handled []string
	for _, c := range (&Provider{}).Capabilities() {
		if c.Handler == nil {
			t.Errorf("capability %s has no handler", c.Name)
		}
		handled = append(handled, c.Name)
	}
	sort.Strings(declared)
	sort.Strings(handled)
	if len(declared) == 0 || !sameStrings(declared, handled) {
		t.Fatalf("declared executors %v != capabilities %v", declared, handled)
	}
}

func TestContractCompat(t *testing.T) {
	if err := memql.CheckPluginContractCompat(ContractVersion); err != nil {
		t.Fatal(err)
	}
}
