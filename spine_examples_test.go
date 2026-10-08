package main

import (
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

func TestCompanySpineBundleIsAnInstallableLanguageBundle(t *testing.T) {
	manifest, err := os.ReadFile("examples/spine/company/memql.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, problems := parser.CheckLanguageLineFile("company", "examples/spine/company/memql.toml", manifest); len(problems) != 0 {
		t.Fatalf("company language line: %v", problems)
	}
	tree, err := dslimports.Load(os.DirFS("examples/spine"))
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range append(tree.VerifyReferentialIntegrity(), tree.VerifyAllSymbolReferences()...) {
		t.Error(err)
	}
}
