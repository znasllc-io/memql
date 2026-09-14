package parser

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// mutation_write_block_v1_test.go -- the struct-form mutation rewriter and the
// edition-2026 payload (epic memql#5363, memql#5367).
//
// A write block's two sugars -- `accept { a, b }` and the bare-mirror line
// `args.name` (authoring rule 15) -- are write-block SYNTAX, not expressions,
// so the rewriter resolves both into explicit `name: args.name` entries before
// any expression parser sees the payload. The edition-2026 map literal has no
// key-less entry, so an unexpanded bare mirror would be refused there.

// v1MutationPayload normalises a struct-form mutation and parses it with the
// edition-2026 grammar, returning the payload map literal the statement holds.
func v1MutationPayload(t *testing.T, src string) (*MutationStmt, *ast.MapExpr) {
	t.Helper()
	normalised, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	file, err := ParseFile(normalised)
	if err != nil {
		t.Fatalf("parse:\n%s\nerror: %v", normalised, err)
	}
	if len(file.Definitions) != 1 {
		t.Fatalf("want one definition, got %d", len(file.Definitions))
	}
	fn, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("want a FunctionDef, got %#v", file.Definitions[0])
	}
	stmt, ok := fn.Body.(*MutationStmt)
	if !ok {
		t.Fatalf("want a MutationStmt body, got %T", fn.Body)
	}
	payload, ok := stmt.PayloadExpr.(*ast.MapExpr)
	if !ok {
		t.Fatalf("want the payload as a v1 map literal, got %T", stmt.PayloadExpr)
	}
	return stmt, payload
}

// entryKeys returns a map literal's keys with each value printed.
func entryKeys(m *ast.MapExpr) map[string]string {
	out := map[string]string{}
	for _, en := range m.Entries {
		out[en.Key] = ast.FormatExpr(en.Value)
	}
	return out
}

// TestV1MutationBareMirrorExpandsToAnExplicitEntry: a bare `args.name` line in
// a longhand block reaches the v1 map as `name: args.name`, and parses.
func TestV1MutationBareMirrorExpandsToAnExplicitEntry(t *testing.T) {
	stmt, payload := v1MutationPayload(t, `mutate space createSpace {
  args {
    spaceId  string!
    name     string!
    status   string
  }
  insert {
    id: args.spaceId
    args.name
    args.status,
    active: true
  }
}`)
	got := entryKeys(payload)
	for key, want := range map[string]string{"name": "args.name", "status": "args.status", "active": "true"} {
		if got[key] != want {
			t.Errorf("entry %q = %q, want %q (all entries: %v)", key, got[key], want, got)
		}
	}
	if stmt.IDTemplate == nil || ast.FormatExpr(stmt.IDTemplate.(ast.ExpressionNode)) != "args.spaceId" {
		t.Errorf("the id line is the id= slot, as a v1 node; got %#v", stmt.IDTemplate)
	}
}

// TestV1MutationAcceptStampEmitsExplicitEntries: the accept/stamp desugar --
// nested in a write block, and in the bare top-level form -- already emits
// explicit entries; both parse as v1 map literals.
func TestV1MutationAcceptStampEmitsExplicitEntries(t *testing.T) {
	for name, src := range map[string]string{
		"nested": `mutate space createSpace {
  args {
    spaceId  string!
    name     string!
    status   string
  }
  insert {
    accept { name, status }
    stamp {
      id: args.spaceId
      mode: args.mode ?? "live"
      done: false
    }
  }
}`,
		"top-level": `mutate space createSpace {
  args {
    spaceId  string!
    name     string!
    status   string
  }
  accept { name, status }
  stamp {
    id: args.spaceId
    mode: args.mode ?? "live"
    done: false
  }
}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, payload := v1MutationPayload(t, src)
			got := entryKeys(payload)
			for key, want := range map[string]string{"name": "args.name", "status": "args.status", "mode": `args.mode ?? "live"`, "done": "false"} {
				if got[key] != want {
					t.Errorf("entry %q = %q, want %q (all entries: %v)", key, got[key], want, got)
				}
			}
		})
	}
}

// TestBareMirrorExpansionIsAnExplicitEntry: the rewriter's output carries the
// bare mirror as its explicit entry, the text the payload parser reads.
func TestBareMirrorExpansionIsAnExplicitEntry(t *testing.T) {
	out, err := NormaliseMutationSource(`mutate space createSpace {
  args {
    name  string!
  }
  insert {
    args.name
    kind: "document"
  }
}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "{ name: args.name, kind: \"document\" }") {
		t.Fatalf("the bare mirror must reach the payload as its explicit entry; got:\n%s", out)
	}
}

// TestBareMirrorOfANestedArgIsRefused: rule 15 takes a single-segment arg
// only; a dotted path has no one key to infer, so the rewriter refuses it,
// naming the explicit spelling.
func TestBareMirrorOfANestedArgIsRefused(t *testing.T) {
	_, err := NormaliseMutationSource(`mutate space createSpace {
  args {
    user  object!
  }
  insert {
    args.user.id
  }
}`)
	if err == nil {
		t.Fatal("a bare dotted mirror must be refused")
	}
	for _, want := range []string{"authoring rule 15", "id: args.user.id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q; got %v", want, err)
		}
	}
}
