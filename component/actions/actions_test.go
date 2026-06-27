package actions

import (
	"testing"
)

const cloneSrc = `@kind("primitive")
@sideEffect("exec")
@description("Check out a repo at a ref.")
action cloneRepoAtVersion {
  capability "shell.exec"
  intent "Clone/checkout a repo working tree at the requested ref."
  params {
    workdir string @required @description("repo working tree")
    ref     string @required @description("git ref")
  }
  argTemplate {
    cmd: "git -C $params.workdir checkout $params.ref"
  }
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
	if a.Capability != "shell.exec" {
		t.Errorf("capability = %q", a.Capability)
	}
	if a.Kind != "primitive" {
		t.Errorf("kind = %q", a.Kind)
	}
	if a.SideEffect != "exec" {
		t.Errorf("sideEffect = %q", a.SideEffect)
	}
	if !a.Enabled {
		t.Error("expected enabled")
	}
	if len(a.Params) != 2 || !a.Params[0].Required || a.Params[1].Name != "ref" {
		t.Fatalf("params = %+v", a.Params)
	}
	if len(a.ArgTemplate) != 1 || a.ArgTemplate[0].Key != "cmd" {
		t.Fatalf("argTemplate = %+v", a.ArgTemplate)
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
	got, _ := rendered["cmd"].(string)
	want := "git -C /work/memql checkout v1.2.3"
	if got != want {
		t.Errorf("rendered cmd = %q, want %q", got, want)
	}
}

func TestBindMissingRequired(t *testing.T) {
	a := loadOne(t, cloneSrc)
	if _, err := Bind(a, map[string]any{"workdir": "/work"}); err == nil {
		t.Fatal("expected error for missing required param ref")
	}
}

func TestRenderWholeValuePreservesType(t *testing.T) {
	src := `action passNumber {
  capability "integration.x.y"
  intent "..."
  params { count int @required }
  argTemplate { n: "$params.count" }
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

func TestRenderUndeclaredParamErrors(t *testing.T) {
	src := `action bad {
  capability "shell.exec"
  intent "..."
  params { a string @required }
  argTemplate { cmd: "echo $params.b" }
}`
	a := loadOne(t, src)
	bound, _ := Bind(a, map[string]any{"a": "x"})
	if _, err := Render(a, bound); err == nil {
		t.Fatal("expected error for undeclared $params.b reference")
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
	src := `@disabled
action offThing {
  capability "shell.exec"
  intent "..."
  params {}
  argTemplate {}
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

func TestActionStepFormsNotSlicedAsDefinitions(t *testing.T) {
	// An automation body containing action STEP forms must NOT be mistaken
	// for a top-level action definition by the slicer.
	src := `automation deploy {
  step clone {
    action("cloneRepoAtVersion@1") { args { ref: "main" } }
  }
  step legacy {
    action { ref: "act_x@3" }
  }
}`
	if acts, err := LoadSource(src, "auto.memql"); err != nil || len(acts) != 0 {
		t.Fatalf("expected zero sliced actions, got %d (err=%v)", len(acts), err)
	}
}
