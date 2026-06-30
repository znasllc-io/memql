package parser

import (
	"strings"
	"testing"
)

// The simplified action grammar (construct-invocation ADR Decision 3, Story 4):
// an `args` block + a single `capability <verb>(...)` call parses to an
// ActionDecl carrying the bare verb + lowered call args.
func TestParseActionDecl_SimplifiedForm(t *testing.T) {
	src := `@description("Check out a repo at a ref.")
action cloneRepoAtVersion {
  args {
    workdir string @required
    ref     string @required
  }
  capability script(script: "deploy.cloneRepo", workdir: args.workdir, ref: args.ref)
}`
	decl, err := ParseActionDecl(src)
	if err != nil {
		t.Fatalf("ParseActionDecl: %v", err)
	}
	if decl.Name != "cloneRepoAtVersion" {
		t.Errorf("name = %q", decl.Name)
	}
	if decl.Capability != "script" {
		t.Errorf("capability verb = %q, want script", decl.Capability)
	}
	if decl.Args == nil || len(decl.Args.Fields) != 2 {
		t.Fatalf("args = %+v", decl.Args)
	}
	if len(decl.CallArgs) != 3 {
		t.Fatalf("callArgs = %+v", decl.CallArgs)
	}
	// The literal `script` selector lowers to a LiteralExpr; the args.X refs to ArgRefExpr.
	byKey := map[string]any{}
	for _, ca := range decl.CallArgs {
		byKey[ca.Key] = ca.Value
	}
	if lit, ok := byKey["script"].(*LiteralExpr); !ok || lit.Value != "deploy.cloneRepo" {
		t.Errorf("script call arg = %#v, want LiteralExpr(deploy.cloneRepo)", byKey["script"])
	}
	if ref, ok := byKey["workdir"].(*ArgRefExpr); !ok || ref.Path != "workdir" {
		t.Errorf("workdir call arg = %#v, want ArgRefExpr(workdir)", byKey["workdir"])
	}
}

// Each retired legacy action surface is rejected with a migration-pointing
// parse error.
func TestParseActionDecl_LegacySurfacesRejected(t *testing.T) {
	cases := map[string]string{
		"@kind": `@kind("primitive")
action a { args { } capability script(script: "x") }`,
		"@sideEffect": `@sideEffect("exec")
action a { args { } capability script(script: "x") }`,
		"@reliability": `@reliability("best_effort")
action a { args { } capability script(script: "x") }`,
		"capability-string": `action a { capability "shell.script" args { } }`,
		"intent":            `action a { args { } intent "x" capability script(script: "x") }`,
		"params":            `action a { params { x string @required } capability script(script: "x") }`,
		"argTemplate":       `action a { args { } capability script(script: "x") argTemplate { k: "v" } }`,
		"body":              `action a { args { } body { return 1 } }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseActionDecl(src); err == nil {
				t.Fatalf("expected %s legacy surface to be rejected", name)
			}
		})
	}
}

// An action body missing its capability call is rejected.
func TestParseActionDecl_MissingCapabilityRejected(t *testing.T) {
	if _, err := ParseActionDecl(`action a { args { x string @required } }`); err == nil {
		t.Fatal("expected error for an action with no capability call")
	}
}

// Form-D migration: the kind-prefixed `action <name>(<colon-args>)` step lowers
// to the action config `action { ref: "<name>", args: { ... } }` (resolving by
// name, no @version suffix).
func TestTranslateActionStep_KindPrefixedParenForm(t *testing.T) {
	got, err := translateActionStepCall("cloneRepoAtVersion(workdir: event.payload.workdir, ref: event.payload.ref)")
	if err != nil {
		t.Fatalf("translateActionStepCall: %v", err)
	}
	if !strings.Contains(got, `ref: "cloneRepoAtVersion"`) {
		t.Errorf("missing ref; got %q", got)
	}
	if !strings.Contains(got, "args: {") || !strings.Contains(got, "workdir: event.payload.workdir") {
		t.Errorf("missing/wrong args clause; got %q", got)
	}
}

// The new step form parses end-to-end through the automation normaliser.
func TestNormaliseAutomation_NewActionStepForm(t *testing.T) {
	src := `automation deploy {
  step clone {
    action cloneRepoAtVersion(workdir: event.payload.workdir, ref: event.payload.ref)
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("NormaliseAutomationSource: %v", err)
	}
	if !strings.Contains(out, `ref: "cloneRepoAtVersion"`) {
		t.Errorf("normalised output missing action ref; got:\n%s", out)
	}
}

// A no-arg paren form `action <name>()` lowers to a ref-only action config.
func TestTranslateActionStep_NoArgs(t *testing.T) {
	got, err := translateActionStepCall("doThing()")
	if err != nil {
		t.Fatalf("translateActionStepCall: %v", err)
	}
	if !strings.Contains(got, `ref: "doThing"`) || strings.Contains(got, "args:") {
		t.Errorf("expected ref-only config, got %q", got)
	}
}
