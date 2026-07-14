package main

// Integration tests for the memqllint CLI (znasllc-io/memql#2509): the
// referential-integrity lanes must surface through run()'s exit code and
// report, since downstream product CI consumes exactly this surface.

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTree materializes a DSL fixture under a temp dir and returns its root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

const testConcepts = `@version("1.0.0")
@namespace("demo")
@description("A demo item.")
concept item {
  name    string  @required @description("Item name.")
  status  string  @description("Item status.")
}`

func TestRun_CleanTreeExitsZero(t *testing.T) {
	root := writeTree(t, map[string]string{
		"demo/concepts.memql": testConcepts,
		"demo/queries.memql": `use demo.concepts.{ item }

@enabled
@description("A clean query.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`,
	})
	if code := run([]string{root}); code != 0 {
		t.Errorf("clean tree: run() = %d, want 0", code)
	}
}

func TestRun_ReferentialIntegrityFindingsExitOne(t *testing.T) {
	// One representative defect per lane; each must flip the exit code.
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "missing use module",
			files: map[string]string{
				"demo/concepts.memql": testConcepts,
				"demo/queries.memql": `use demo.nonexistentfile.{ ghost }

@enabled
@description("Ghost module; ghost referenced here: ghost.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`,
			},
		},
		{
			name: "missing imported symbol",
			files: map[string]string{
				"demo/concepts.memql": testConcepts,
				"demo/queries.memql": `use demo.concepts.{ item, deletedConcept }

@enabled
@description("deletedConcept was removed from the module; referenced here: deletedConcept.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`,
			},
		},
		{
			name: "insert field not on concept schema",
			files: map[string]string{
				"demo/concepts.memql": testConcepts,
				"demo/mutations.memql": `use demo.concepts.{ item }

@enabled
@description("Writes an undeclared field.")
mutate item createItem {
  args {
    itemId  string  @required
  }
  insert {
    id: args.itemId
    bogusField: "not-on-schema"
  }
}`,
			},
		},
		{
			name: "stranded import after call rename",
			files: map[string]string{
				"demo/concepts.memql": testConcepts,
				"demo/logic.memql": `@enabled
@description("Decides something.")
logic decideThing {
  args {
    event object @required
  }
  body {
    return true
  }
}`,
				"demo/automations.memql": `use demo.logic.{ decideThing }

@enabled
@trigger(event="graph.node.created.v1:demo:item")
@description("Step call renamed away from the import.")
automation onItemCreated {
  step decide {
    logic decideThingX ( event: event )
  }
}`,
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.files)
			if code := run([]string{root}); code != 1 {
				t.Errorf("run() = %d, want 1 (diagnostics found)", code)
			}
		})
	}
}
