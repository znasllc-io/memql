package parser

import (
	"strings"
	"testing"
)

// TestParseCapabilityDecl_GoldenPath locks the canonical capability shape
// (construct-invocation ADR Decision 4, Story 5 / memql#2325): @sideEffect
// + @description annotations, a dotted/namespaced name, and an optional
// args block with the same field grammar as action/builtin.
func TestParseCapabilityDecl_GoldenPath(t *testing.T) {
	source := `@sideEffect("write")
@description("Create a git tag + GitHub release for a version.")
capability integration.github.tagRelease {
  args {
    repo string @required
    tag  string @required
  }
}`
	decl, err := ParseCapabilityDecl(source)
	if err != nil {
		t.Fatalf("ParseCapabilityDecl: %v", err)
	}
	if decl.Name != "integration.github.tagRelease" {
		t.Errorf("Name = %q, want integration.github.tagRelease", decl.Name)
	}
	// @sideEffect + @description land on the capability-level attributes.
	gotSideEffect, gotDescription := "", ""
	for _, a := range decl.Attributes {
		switch a.Name {
		case "sideEffect":
			if s, ok := a.Value.(string); ok {
				gotSideEffect = s
			}
		case "description":
			if s, ok := a.Value.(string); ok {
				gotDescription = s
			}
		}
	}
	if gotSideEffect != "write" {
		t.Errorf("@sideEffect = %q, want write", gotSideEffect)
	}
	if gotDescription == "" {
		t.Error("@description not captured on the capability")
	}
	if decl.Args == nil {
		t.Fatal("Args block not parsed")
	}
	if len(decl.Args.Fields) != 2 {
		t.Fatalf("Args.Fields = %d, want 2", len(decl.Args.Fields))
	}
	if decl.Args.Fields[0].Name != "repo" || decl.Args.Fields[0].Optional {
		t.Errorf("field[0] = %+v, want repo @required", decl.Args.Fields[0])
	}
	if decl.Args.Fields[1].Name != "tag" || decl.Args.Fields[1].Optional {
		t.Errorf("field[1] = %+v, want tag @required", decl.Args.Fields[1])
	}
}

// TestParseCapabilityDecl_NoArgsBlock confirms a capability may omit the
// args block entirely (a surface-backed verb with no parameters).
func TestParseCapabilityDecl_NoArgsBlock(t *testing.T) {
	decl, err := ParseCapabilityDecl(`@sideEffect("exec")
capability shell.script { }`)
	if err != nil {
		t.Fatalf("ParseCapabilityDecl: %v", err)
	}
	if decl.Name != "shell.script" {
		t.Errorf("Name = %q, want shell.script", decl.Name)
	}
	if decl.Args != nil {
		t.Errorf("Args = %+v, want nil for an empty body", decl.Args)
	}
}

// TestParseCapabilityDecl_RejectsBodyBlock confirms a capability with a
// `body { ... }` block is rejected: a capability is surface-backed and
// never procedural.
func TestParseCapabilityDecl_RejectsBodyBlock(t *testing.T) {
	_, err := ParseCapabilityDecl(`@sideEffect("write")
capability integration.github.tagRelease {
  body {
    return 1
  }
}`)
	if err == nil {
		t.Fatal("expected a parse error for a capability with a body block, got nil")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("error %q does not mention the rejected `body` block", err.Error())
	}
}

// TestParseCapabilityDecl_RejectsBareName confirms a non-namespaced
// (un-dotted) capability name is rejected at parse time.
func TestParseCapabilityDecl_RejectsBareName(t *testing.T) {
	_, err := ParseCapabilityDecl(`@sideEffect("read")
capability tagRelease {
  args { repo string @required }
}`)
	if err == nil {
		t.Fatal("expected a parse error for a bare (un-dotted) capability name, got nil")
	}
	if !strings.Contains(err.Error(), "namespaced") && !strings.Contains(err.Error(), "dotted") {
		t.Errorf("error %q does not explain the namespaced/dotted requirement", err.Error())
	}
}

// TestParseCapabilityDecl_RejectsUnknownKey confirms a body key other
// than `args` is rejected.
func TestParseCapabilityDecl_RejectsUnknownKey(t *testing.T) {
	_, err := ParseCapabilityDecl(`capability fs.readFile {
  params { path string }
}`)
	if err == nil {
		t.Fatal("expected a parse error for an unknown body key, got nil")
	}
}

// TestTopLevelDeclKeywordsIncludeCapability pins that the dispatch table
// (and thus the #2124 drift surface) now carries `capability`.
func TestTopLevelDeclKeywordsIncludeCapability(t *testing.T) {
	found := false
	for _, kw := range TopLevelDeclKeywords {
		if kw == "capability" {
			found = true
			break
		}
	}
	if !found {
		t.Error("TopLevelDeclKeywords does not include `capability`")
	}
}
