package parambind

import (
	"reflect"
	"testing"
)

func TestBindArgs_RebindsFromStepInput(t *testing.T) {
	call := Call{
		Index:      0,
		Capability: "fs.writeFile",
		Bindings: []Binding{
			{Name: "path", Kind: SourceStepInput, Ref: "dir", Literal: "/old"},
			{Name: "content", Kind: SourceLiteral, Literal: "hello"},
		},
	}
	// New input -> path re-binds to the new dir; content stays literal.
	got := BindArgs(call, map[string]any{"dir": "/new"})
	want := map[string]any{"path": "/new", "content": "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestBindArgs_MissingRefFallsBackToLiteral(t *testing.T) {
	call := Call{Bindings: []Binding{{Name: "path", Kind: SourceStepInput, Ref: "dir", Literal: "/recorded"}}}
	got := BindArgs(call, map[string]any{}) // dir absent
	if got["path"] != "/recorded" {
		t.Fatalf("missing ref must fall back to literal, got %v", got["path"])
	}
}

func TestBindArgs_NestedRef(t *testing.T) {
	call := Call{Bindings: []Binding{{Name: "name", Kind: SourceStepInput, Ref: "meta.project.name"}}}
	in := map[string]any{"meta": map[string]any{"project": map[string]any{"name": "memql"}}}
	if got := BindArgs(call, in); got["name"] != "memql" {
		t.Fatalf("nested ref: want memql, got %v", got["name"])
	}
}

func TestBindArgs_SliceIndexRef(t *testing.T) {
	call := Call{Bindings: []Binding{{Name: "first", Kind: SourceStepInput, Ref: "items.0"}}}
	in := map[string]any{"items": []any{"a", "b"}}
	if got := BindArgs(call, in); got["first"] != "a" {
		t.Fatalf("slice ref: want a, got %v", got["first"])
	}
}

func TestBindArgs_FragmentSubstitution(t *testing.T) {
	// Recorded: path = "/work/proj-A/go.mod", fragment "/work/proj-A" traced
	// to input dir. New input dir -> the fragment is substituted in-place.
	call := Call{Bindings: []Binding{{
		Name: "path", Kind: SourceStepInput, Ref: "dir", Fragment: true,
		Template: "/work/proj-A/go.mod", FragmentValue: "/work/proj-A",
	}}}
	got := BindArgs(call, map[string]any{"dir": "/work/proj-B"})
	if got["path"] != "/work/proj-B/go.mod" {
		t.Fatalf("fragment substitution: want /work/proj-B/go.mod, got %v", got["path"])
	}
}

func TestIsParameterized(t *testing.T) {
	lit := Call{Bindings: []Binding{{Name: "x", Kind: SourceLiteral, Literal: 1}}}
	if lit.IsParameterized() {
		t.Fatalf("literal-only call must not be parameterized")
	}
	par := Call{Bindings: []Binding{{Name: "x", Kind: SourceStepInput, Ref: "a"}}}
	if !par.IsParameterized() {
		t.Fatalf("step-input call must be parameterized")
	}
}

func TestTemplateFingerprintKeys_StableAndStructural(t *testing.T) {
	a := TemplateFingerprintKeys(map[string]any{"dir": "/x", "module": "m"})
	b := TemplateFingerprintKeys(map[string]any{"module": "n", "dir": "/y"})
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same key set must yield same template keys: %v vs %v", a, b)
	}
	c := TemplateFingerprintKeys(map[string]any{"dir": "/x"})
	if reflect.DeepEqual(a, c) {
		t.Fatalf("different key sets must differ")
	}
}
