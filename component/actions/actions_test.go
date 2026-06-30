package actions

import (
	"testing"
)

// cloneSrc is a simplified-form authored action (construct-invocation ADR
// Decision 3, Story 4): an `args` block + a single `capability <verb>(...)`
// call, with the verb imported via `use capabilities.*`.
const cloneSrc = `use capabilities.shell.{ script }
@description("Check out a repo at a ref.")
action cloneRepoAtVersion {
  args {
    workdir string @required @description("repo working tree")
    ref     string @required @description("git ref")
  }
  capability script(script: "deploy.cloneRepo", workdir: args.workdir, ref: args.ref)
}`

func loadOne(t *testing.T, src string) *Action {
	t.Helper()
	acts, err := LoadSource(src, "test.memql")
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	if len(acts) != 1 {
		t.Fatalf("expected 1 action, got %d", len(acts))
	}
	return acts[0]
}

func TestLoadSourceParsesAuthoredAction(t *testing.T) {
	a := loadOne(t, cloneSrc)
	if a.Name != "cloneRepoAtVersion" {
		t.Errorf("name = %q", a.Name)
	}
	if a.Version != 1 {
		t.Errorf("version = %d, want 1", a.Version)
	}
	// The bare verb `script` resolves to the full dotted capability via the
	// file's `use capabilities.shell.{ script }` import.
	if a.Capability != "shell.script" {
		t.Errorf("capability = %q, want shell.script", a.Capability)
	}
	if a.Kind != "primitive" {
		t.Errorf("kind = %q", a.Kind)
	}
	// sideEffect is DERIVED from the capability (shell.* -> exec), never declared.
	if a.SideEffect != "exec" {
		t.Errorf("sideEffect = %q, want exec", a.SideEffect)
	}
	if !a.Enabled {
		t.Error("expected enabled")
	}
	if len(a.Params) != 2 || !a.Params[0].Required || a.Params[1].Name != "ref" {
		t.Fatalf("params = %+v", a.Params)
	}
	// CallArgs: one literal (script) + two args.X references (workdir, ref).
	if len(a.CallArgs) != 3 {
		t.Fatalf("callArgs = %+v", a.CallArgs)
	}
}

func TestBindAndRender(t *testing.T) {
	a := loadOne(t, cloneSrc)
	bound, err := Bind(a, map[string]any{"workdir": "/work/memql", "ref": "v1.2.3", "extra": "ignored"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	rendered, err := Render(a, bound)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got, _ := rendered["script"].(string); got != "deploy.cloneRepo" {
		t.Errorf("rendered script = %q, want deploy.cloneRepo", got)
	}
	if got, _ := rendered["workdir"].(string); got != "/work/memql" {
		t.Errorf("rendered workdir = %q, want /work/memql", got)
	}
	if got, _ := rendered["ref"].(string); got != "v1.2.3" {
		t.Errorf("rendered ref = %q, want v1.2.3", got)
	}
}

func TestBindMissingRequired(t *testing.T) {
	a := loadOne(t, cloneSrc)
	if _, err := Bind(a, map[string]any{"workdir": "/work"}); err == nil {
		t.Fatal("expected error for missing required param ref")
	}
}

func TestRenderWholeValuePreservesType(t *testing.T) {
	// A bare args.X reference passes the bound value through verbatim,
	// preserving its (non-string) type.
	src := `use capabilities.integration.x.{ y }
action passNumber {
  args { count int @required }
  capability y(n: args.count)
}`
	a := loadOne(t, src)
	bound, err := Bind(a, map[string]any{"count": 42})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	rendered, err := Render(a, bound)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if v, ok := rendered["n"].(int); !ok || v != 42 {
		t.Errorf("whole-value render = %#v, want int 42", rendered["n"])
	}
}

func TestUndeclaredArgRefRejectedAtLoad(t *testing.T) {
	// An action whose capability arg references an args.X that is not declared
	// is a loud LOAD error (the $params.X string-template form is retired).
	src := `use capabilities.shell.{ script }
action bad {
  args { a string @required }
  capability script(cmd: args.b)
}`
	if _, err := LoadSource(src, "t.memql"); err == nil {
		t.Fatal("expected load error for undeclared args.b reference")
	}
}

func TestRegistryDuplicateRejected(t *testing.T) {
	r := NewRegistry()
	a := loadOne(t, cloneSrc)
	if err := r.Register(a); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(loadOne(t, cloneSrc)); err == nil {
		t.Fatal("expected duplicate (name,version) registration to fail")
	}
}

func TestRegistryLookup(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(loadOne(t, cloneSrc)); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Lookup("cloneRepoAtVersion", 1); !ok {
		t.Error("pinned lookup failed")
	}
	if _, ok := r.Lookup("cloneRepoAtVersion", 2); ok {
		t.Error("expected miss for version 2")
	}
	if a, ok := r.LookupLatest("cloneRepoAtVersion"); !ok || a.Version != 1 {
		t.Error("latest lookup failed")
	}
}

func TestDisabledActionDropped(t *testing.T) {
	src := `use capabilities.shell.{ script }
@disabled
action offThing {
  args { }
  capability script(script: "deploy.noop")
}`
	acts, err := LoadSource(src, "t.memql")
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Enabled {
		t.Fatalf("expected one disabled action, got %+v", acts)
	}
	// LoadFromFS would skip it; Register here only to confirm the flag.
}

// The retired legacy action surfaces fail loud with a migration-pointing error.
func TestLegacyActionSurfacesRejected(t *testing.T) {
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
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSource("use capabilities.shell.{ script }\n"+src, "t.memql"); err == nil {
				t.Fatalf("expected %s legacy surface to be rejected", name)
			}
		})
	}
}

func TestActionStepFormsNotSlicedAsDefinitions(t *testing.T) {
	// An automation body containing action STEP forms (legacy and the new
	// kind-prefixed form) must NOT be mistaken for top-level action definitions
	// by the slicer.
	src := `automation deploy {
  step clone {
    action cloneRepoAtVersion(workdir: "/w", ref: "main")
  }
  step legacyA {
    action("cloneRepoAtVersion@1") { args { ref: "main" } }
  }
  step legacyB {
    action { ref: "act_x@3" }
  }
}`
	if acts, err := LoadSource(src, "auto.memql"); err != nil || len(acts) != 0 {
		t.Fatalf("expected zero sliced actions, got %d (err=%v)", len(acts), err)
	}
}
