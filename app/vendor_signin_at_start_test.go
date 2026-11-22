package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestOnlyTheIdentityNodeOptsOutOfVendorSignInAtStart holds which node types
// sign in to a vendor at start (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md, A5).
//
// Every node that calls models signs in once at start, so that
// `memql provider-auth check` has a record to report. The identity node calls
// none and runs under a service account a vendor's rule does not admit, so its
// Build opts out. Each Build is behind a build tag and only one compiles into a
// test binary, which is why this reads the source: an opt-out added to a second
// node type would silently leave that node with nothing recorded, and one
// removed from the identity node would put a refusal in the vendor's console on
// every boot. The behaviour of the opt-out itself is tested where it lives
// (component/memql, TestSignInAtStartIsSkippedOnANodeThatCallsNoModels).
func TestOnlyTheIdentityNodeOptsOutOfVendorSignInAtStart(t *testing.T) {
	files, err := filepath.Glob("build_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var optedOut []string
	builds := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		builds++
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "DisableVendorSignInAtStart" {
				optedOut = append(optedOut, file)
			}
			return true
		})
	}
	if builds < 5 {
		t.Fatalf("found only %d build_*.go files; this test would prove nothing", builds)
	}
	sort.Strings(optedOut)
	if len(optedOut) != 1 || optedOut[0] != "build_identity.go" {
		t.Fatalf("node types that opt out of signing in to a vendor at start: %v, want exactly [build_identity.go]", optedOut)
	}
}
