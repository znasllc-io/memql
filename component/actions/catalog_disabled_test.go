package actions

import (
	"testing"
	"testing/fstest"
)

// #2607: the catalog is the dispatch-side capability walk (the validation
// side is component/memql's name loader; both filter via
// ast.CapabilityDecl.IsDisabled so the sets cannot diverge). A @disabled
// capability must miss Lookup, report Disabled, and never reach dispatch.
func TestLoadCatalogFromFS_DisabledSkippedButRecorded(t *testing.T) {
	tree := fstest.MapFS{"capabilities.memql": {Data: []byte(`@disabled
@sideEffect("read")
capability fs.readFile {
  args {
    subject string @required
  }
}

@sideEffect("exec")
capability shell.exec {
  args {
    subject string @required
  }
}
`)}}
	cat, err := LoadCatalogFromFS(tree)
	if err != nil {
		t.Fatalf("LoadCatalogFromFS: %v", err)
	}
	if _, ok := cat.Lookup("fs.readFile"); ok {
		t.Error("@disabled capability resolvable via catalog Lookup; dispatch set diverged from validation set")
	}
	if !cat.Disabled("fs.readFile") {
		t.Error("@disabled capability not recorded as disabled; the action loader cannot reject references loudly")
	}
	if _, ok := cat.Lookup("shell.exec"); !ok {
		t.Error("enabled peer capability missing from the catalog")
	}
	if cat.Disabled("shell.exec") {
		t.Error("enabled capability wrongly reported disabled")
	}
}
