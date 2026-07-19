package actions

import (
	"strings"
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

// G1 from the #2642 review: the action-loader rejection is the guard that
// makes "disabled means undispatchable, loudly" true, and it must be pinned.
// Driven through the injected-catalog seam because DefaultCatalog is a
// process-wide singleton over the embedded tree (zero disabled capabilities).
func TestValidateAgainstCatalog_RejectsDisabledCapability(t *testing.T) {
	tree := fstest.MapFS{"capabilities.memql": {Data: []byte(`@disabled
@sideEffect("read")
capability fs.readFile {
  args {
    path string @required
  }
}
`)}}
	cat, err := LoadCatalogFromFS(tree)
	if err != nil {
		t.Fatalf("LoadCatalogFromFS: %v", err)
	}

	a := &Action{Capability: "fs.readFile"}
	err = validateAgainstCatalog(cat, nil, a, "probeAction", "test:probe")
	if err == nil {
		t.Fatal("action referencing a @disabled capability must be rejected at load")
	}
	if got := err.Error(); !strings.Contains(got, `capability "fs.readFile" is @disabled`) {
		t.Errorf("rejection must name the disabled capability; got %q", got)
	}

	live := &Action{Capability: "integration.unknown.verb"}
	if err := validateAgainstCatalog(cat, nil, live, "probeAction", "test:probe"); err != nil {
		t.Errorf("undeclared capability must fall through to the namespace check, got %v", err)
	}
}

// N1 from the same review: a @disabled capability must still reconcile --
// disabling must not smuggle vocabulary-invalid or risk-class-mismatched
// decls past the ADR unspoofable-class contract.
func TestLoadCatalogFromFS_DisabledStillReconciled(t *testing.T) {
	for name, src := range map[string]string{
		"bogus-namespace": `@disabled
@sideEffect("read")
capability totally.bogus.verb {
  args {
    x string @required
  }
}
`,
		"sideeffect-mismatch": `@disabled
@sideEffect("read")
capability fs.writeFile {
  args {
    path string @required
  }
}
`,
	} {
		t.Run(name, func(t *testing.T) {
			tree := fstest.MapFS{"capabilities.memql": {Data: []byte(src)}}
			if _, err := LoadCatalogFromFS(tree); err == nil {
				t.Error("a @disabled capability must still fail reconciliation; disabling is not a validation bypass")
			}
		})
	}
}

// Reviewer round-3 queue item: the dup check must be order-independent now
// that it covers disabled names -- declaring the same dotted name twice
// errors whichever of the pair is @disabled, and neither silently wins.
func TestLoadCatalogFromFS_DuplicateAcrossLifecycleStates(t *testing.T) {
	for name, src := range map[string]string{
		"disabled-then-enabled": `@disabled
@sideEffect("read")
capability fs.readFile {
  args {
    path string @required
  }
}

@sideEffect("read")
capability fs.readFile {
  args {
    path string @required
  }
}
`,
		"enabled-then-disabled": `@sideEffect("read")
capability fs.readFile {
  args {
    path string @required
  }
}

@disabled
@sideEffect("read")
capability fs.readFile {
  args {
    path string @required
  }
}
`,
	} {
		t.Run(name, func(t *testing.T) {
			tree := fstest.MapFS{"capabilities.memql": {Data: []byte(src)}}
			if _, err := LoadCatalogFromFS(tree); err == nil {
				t.Error("duplicate capability across lifecycle states must error; neither declaration may silently win")
			}
		})
	}
}
