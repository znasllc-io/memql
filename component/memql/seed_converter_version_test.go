package memql

import (
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
)

// TestSeedDeclASTToInternal_VersionDefault kills the #2657-review mutant:
// reverting the seedDeclASTToInternal call site to `version: decl.Version`
// must fail here, so the test goes through the converter rather than
// calling coalesceSeedVersion directly.
func TestSeedDeclASTToInternal_VersionDefault(t *testing.T) {
	absent := &languageAst.SeedDecl{Name: "probe"}
	got, err := seedDeclASTToInternal(absent)
	if err != nil {
		t.Fatalf("seedDeclASTToInternal without version: %v", err)
	}
	if got.version != "1.0.0" {
		t.Errorf("seed version = %q, want the 1.0.0 default (#2613)", got.version)
	}

	explicit := &languageAst.SeedDecl{Name: "probe", Version: "2.0.0"}
	got, err = seedDeclASTToInternal(explicit)
	if err != nil {
		t.Fatalf("seedDeclASTToInternal with explicit version: %v", err)
	}
	if got.version != "2.0.0" {
		t.Errorf("seed version = %q, want the explicit 2.0.0", got.version)
	}
}
