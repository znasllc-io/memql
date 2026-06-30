package actions

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestCatalogLoadsEmbeddedAndReconciles confirms the embedded DSL catalog
// loads and reconciles cleanly against the Go vocabulary, and that the
// declared deployment capabilities carry the authoritative side-effect class.
func TestCatalogLoadsEmbeddedAndReconciles(t *testing.T) {
	if err := DefaultCatalogError(); err != nil {
		t.Fatalf("embedded capability catalog failed to load/reconcile: %v", err)
	}
	cat := DefaultCatalog()
	want := map[string]string{
		"shell.script":                  "exec",
		"fs.readFile":                   "read",
		"fs.writeFile":                  "write",
		"integration.github.tagRelease": "write",
	}
	for name, class := range want {
		info, ok := cat.Lookup(name)
		if !ok {
			t.Errorf("capability %q not in embedded catalog (have %v)", name, cat.Names())
			continue
		}
		if info.Class != class {
			t.Errorf("capability %q class = %q, want %q", name, info.Class, class)
		}
	}
	// shell.* is open/pass-through; integration.* is closed.
	if info, _ := cat.Lookup("shell.script"); info == nil || !info.Open {
		t.Error("shell.script should be an open/pass-through capability")
	}
	if info, _ := cat.Lookup("integration.github.tagRelease"); info == nil || info.Open {
		t.Error("integration.github.tagRelease should be a closed/typed capability")
	}
}

// TestCatalogRejectsSideEffectMismatch: a capability decl whose @sideEffect
// lies about the authoritative Go class (fs.readFile is read, not write) is
// rejected at catalog load -- the side-effect class is unspoofable.
func TestCatalogRejectsSideEffectMismatch(t *testing.T) {
	tree := fstest.MapFS{
		"caps.memql": &fstest.MapFile{Data: []byte(`@sideEffect("write")
capability fs.readFile { args { path string @required } }`)},
	}
	_, err := LoadCatalogFromFS(tree)
	if err == nil {
		t.Fatal("expected reconciliation error for a @sideEffect that mismatches the Go class")
	}
	if !strings.Contains(err.Error(), "sideEffect") || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("error should explain the sideEffect mismatch; got %v", err)
	}
}

// TestCatalogRejectsUnknownCapability: a capability dotted-name not in the Go
// closed vocabulary cannot be declared -- the catalog cannot invent
// capabilities outside the surface-backed set.
func TestCatalogRejectsUnknownCapability(t *testing.T) {
	tree := fstest.MapFS{
		"caps.memql": &fstest.MapFile{Data: []byte(`@sideEffect("read")
capability integration.madeup.frobnicate { args { x string @required } }`)},
	}
	_, err := LoadCatalogFromFS(tree)
	if err == nil {
		t.Fatal("expected load error for a capability outside the Go vocabulary")
	}
	if !strings.Contains(err.Error(), "Go capability vocabulary") {
		t.Errorf("error should explain the unknown-capability reason; got %v", err)
	}
}

// TestStrictTyping_MissingRequiredArg: an action calling a closed capability
// without a declared @required arg is rejected at load.
func TestStrictTyping_MissingRequiredArg(t *testing.T) {
	src := `use capabilities.integration.github.{ tagRelease }
action tagOnly {
  args { repo string @required }
  capability tagRelease(repo: args.repo)
}`
	_, err := LoadSource(src, "t.memql")
	if err == nil {
		t.Fatal("expected error: tagRelease requires both repo and tag")
	}
	if !strings.Contains(err.Error(), "missing required arg") || !strings.Contains(err.Error(), "tag") {
		t.Errorf("error should name the missing required arg; got %v", err)
	}
}

// TestStrictTyping_UnknownArgClosedCapability: an action passing an undeclared
// arg to a CLOSED capability is rejected.
func TestStrictTyping_UnknownArgClosedCapability(t *testing.T) {
	src := `use capabilities.integration.github.{ tagRelease }
action tagBogus {
  args { repo string @required
    tag string @required
    extra string @required }
  capability tagRelease(repo: args.repo, tag: args.tag, bogus: args.extra)
}`
	_, err := LoadSource(src, "t.memql")
	if err == nil {
		t.Fatal("expected error: tagRelease is closed and rejects the unknown arg 'bogus'")
	}
	if !strings.Contains(err.Error(), "unknown arg") || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the unknown arg; got %v", err)
	}
}

// TestStrictTyping_OpenCapabilityToleratesExtraArgs: shell.script is open, so
// extra structured args (validated downstream by the capability-script
// contract) are tolerated as long as the @required selector is present.
func TestStrictTyping_OpenCapabilityToleratesExtraArgs(t *testing.T) {
	src := `use capabilities.shell.{ script }
action runScript {
  args { a string @required
    b string @required }
  capability script(script: "deploy.thing", alpha: args.a, beta: args.b)
}`
	acts, err := LoadSource(src, "t.memql")
	if err != nil {
		t.Fatalf("open capability should tolerate extra args: %v", err)
	}
	if len(acts) != 1 {
		t.Fatalf("expected 1 action, got %d", len(acts))
	}
}

// TestStrictTyping_OpenCapabilityStillRequiresSelector: an open capability
// still enforces its declared @required arg (shell.script needs `script`).
func TestStrictTyping_OpenCapabilityStillRequiresSelector(t *testing.T) {
	src := `use capabilities.shell.{ script }
action noSelector {
  args { workdir string @required }
  capability script(workdir: args.workdir)
}`
	_, err := LoadSource(src, "t.memql")
	if err == nil {
		t.Fatal("expected error: shell.script requires the @required 'script' selector")
	}
	if !strings.Contains(err.Error(), "missing required arg") || !strings.Contains(err.Error(), "script") {
		t.Errorf("error should name the missing 'script' selector; got %v", err)
	}
}

// TestStrictTyping_TypeMismatch: a type-incompatible bound arg (object given to
// a string capability arg) is rejected.
func TestStrictTyping_TypeMismatch(t *testing.T) {
	src := `use capabilities.integration.github.{ tagRelease }
action tagTypeBad {
  args { repo object @required
    tag string @required }
  capability tagRelease(repo: args.repo, tag: args.tag)
}`
	_, err := LoadSource(src, "t.memql")
	if err == nil {
		t.Fatal("expected type-mismatch error: repo is object, capability declares string")
	}
	if !strings.Contains(err.Error(), "expects type") {
		t.Errorf("error should explain the type mismatch; got %v", err)
	}
}
