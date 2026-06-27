package parser

import (
	"strings"
	"testing"
)

// memql#2246 -- the struct-form automation rewriter supports a per-item
// iteration step body, closing the author-surface gap that blocked moving
// the per-row sweep logics into automation steps (#2235). Two authoring
// shapes are accepted inside a `step { ... }`:
//
//	forEach <var> in <expr> [where <cond>] { <call> ... }
//	for <var> := range <expr> [if <cond>] { <call> ... }
//
// Both lower to the procedural top-level loop statement
//
//	for item := range <expr> [if <cond>] { <step>_do1 := <call> ... }
//
// which parseForRangeStep already compiles into a StepTypeForEach StepDef.
// The author's iteration variable is renamed to the canonical `item`.

func TestNormaliseAutomationSource_ForEachStep(t *testing.T) {
	src := `@description("Prune stale nodes.")
automation pruneStaleNodes {
  step decide {
    logic findStaleNodes { event: event }
  }
  step prune {
    forEach node in decide.result {
      mutate updateNodeHealth { id: node.id, health: "stopped" }
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The decide step keeps the assignment shape.
	if !strings.Contains(out, "decide := findStaleNodes({ event: event })") {
		t.Fatalf("decide step did not rewrite to an assignment; got:\n%s", out)
	}
	// The forEach step lowers to a top-level for-range loop, with the
	// iteration variable renamed to the canonical `item`.
	if !strings.Contains(out, "for item := range decide.result {") {
		t.Fatalf("forEach step did not rewrite to a for-range loop; got:\n%s", out)
	}
	if !strings.Contains(out, `prune_do1 := updateNodeHealth({ id: item.id, health: "stopped" })`) {
		t.Fatalf("forEach inner call must be renamed + assigned a synthesized name; got:\n%s", out)
	}
	// The forEach loop must NOT be emitted as a `prune := ...` assignment.
	if strings.Contains(out, "prune := for") {
		t.Fatalf("forEach step must be a top-level statement, not an assignment; got:\n%s", out)
	}
}

// The go-style `for <var> := range <expr>` shape lowers identically.
func TestNormaliseAutomationSource_ForRangeStep(t *testing.T) {
	src := `automation sweep {
  step prune {
    for n := range decide.result {
      mutate retire { id: n.id }
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "for item := range decide.result {") {
		t.Fatalf("for-range step did not rewrite; got:\n%s", out)
	}
	if !strings.Contains(out, "prune_do1 := retire({ id: item.id })") {
		t.Fatalf("for-range inner call must be renamed; got:\n%s", out)
	}
}

// An author may already use `item` as the iteration variable -- the rename
// is then a no-op.
func TestNormaliseAutomationSource_ForEachStep_ItemVarPassthrough(t *testing.T) {
	src := `automation sweep {
  step prune {
    forEach item in decide.result {
      mutate retire { id: item.id }
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "prune_do1 := retire({ id: item.id })") {
		t.Fatalf("item-var passthrough must work; got:\n%s", out)
	}
}

// Multiple inner calls each get a distinct synthesized name and all are
// renamed.
func TestNormaliseAutomationSource_ForEachStep_MultipleInnerCalls(t *testing.T) {
	src := `automation sweep {
  step prune {
    forEach node in decide.result {
      mutate retire { id: node.id }
      mutate audit { nodeId: node.id }
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "prune_do1 := retire({ id: item.id })") {
		t.Fatalf("first inner call missing; got:\n%s", out)
	}
	if !strings.Contains(out, "prune_do2 := audit({ nodeId: item.id })") {
		t.Fatalf("second inner call missing; got:\n%s", out)
	}
}

// The optional filter clause (`where` for forEach, `if` for for) lowers to
// the procedural `if <cond>` filter and is renamed.
func TestNormaliseAutomationSource_ForEachStep_Filter(t *testing.T) {
	src := `automation sweep {
  step prune {
    forEach node in decide.result where node.health == "stale" {
      mutate retire { id: node.id }
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `for item := range decide.result if item.health == "stale" {`) {
		t.Fatalf("forEach filter must lower to an `if` clause with the renamed var; got:\n%s", out)
	}
}

func TestNormaliseAutomationSource_ForEachStep_Errors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing in keyword",
			body:    `forEach node decide.result { mutate y { } }`,
			wantErr: "expected `in`",
		},
		{
			name:    "missing range keyword",
			body:    `for n := decide.result { mutate y { } }`,
			wantErr: "expected `range`",
		},
		{
			name:    "empty body",
			body:    `forEach node in decide.result { }`,
			wantErr: "the loop body is empty",
		},
		{
			name:    "inner call without brace body",
			body:    `forEach node in decide.result { node }`,
			wantErr: "must have a `{ ... }` body",
		},
		{
			name:    "empty where filter",
			body:    `forEach node in decide.result where { mutate y { } }`,
			wantErr: "empty `where` filter",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "automation bad {\n  step s {\n    " + tc.body + "\n  }\n}"
			_, err := NormaliseAutomationSource(src)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
