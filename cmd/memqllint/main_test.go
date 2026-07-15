package main

// Integration tests for the memqllint CLI (znasllc-io/memql#2509): the
// referential-integrity lanes must surface through run()'s exit code and
// report, since downstream product CI consumes exactly this surface.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureRun runs the CLI with args while capturing everything it writes to
// stdout, returning the exit code + the captured output. The engine-parity
// pass emits its per-construct log lines to a discarded logger, so stdout
// carries only the report.
func captureRun(t *testing.T, args []string) (int, string) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := run(args)
	_ = w.Close()
	os.Stdout = orig
	return code, <-done
}

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

// TestRun_EngineParityFindings covers the init-time validation classes the
// engine-parity pass (#2520) adds on top of the dslimports lanes: each fixture
// parses + import-resolves cleanly (it would pass the pre-#2520 tool) but the
// engine rejects or skips it at boot. Both lead witness classes plus the
// positive control run end-to-end through run()'s exit code + report.
func TestRun_EngineParityFindings(t *testing.T) {
	// Witness 1: a non-canonical @relationship type. Clean under dslimports
	// (parse + import graph), rejected by the engine's relationship
	// normalizer at Init.
	t.Run("non-canonical relationship type", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"warehouse/concepts.memql": `@version("1.0.0")
@namespace("warehouse")
@description("A hub other rows point at.")
concept hub {
  name  string  @required  @description("Hub name.")
}

@version("1.0.0")
@namespace("warehouse")
@description("A gadget pointing at a hub via a NON-canonical relationship type.")
concept gadget {
  hubId  string  @required  @description("FK to the owning hub.")

  @relationship(type="references", field="hubId", target=hub, direction="outgoing")
}`,
		})
		code, out := captureRun(t, []string{root})
		if code != 1 {
			t.Fatalf("run() = %d, want 1; output:\n%s", code, out)
		}
		if !strings.Contains(out, `relationship type "references" is invalid`) {
			t.Fatalf("expected the report to name the invalid relationship type; output:\n%s", out)
		}
	})

	// Witness 2: a mutation that declares an arg it never references. Clean
	// under dslimports, skipped by the engine's declared-usage validator.
	t.Run("declared but unused mutation arg", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"warehouse/concepts.memql": `@version("1.0.0")
@namespace("warehouse")
@description("A widget.")
concept widget {
  label  string  @required  @description("Widget label.")
}`,
			"warehouse/mutations.memql": `use warehouse.concepts.{ widget }

@enabled
@description("Create a widget; declares an arg the body never references.")
mutate widget createWidget {
  args {
    widgetId   string  @required
    label      string  @required
    unusedArg  string  @required
  }
  insert {
    id: args.widgetId
    args.label
  }
}`,
		})
		code, out := captureRun(t, []string{root})
		if code != 1 {
			t.Fatalf("run() = %d, want 1; output:\n%s", code, out)
		}
		if !strings.Contains(out, "unusedArg") {
			t.Fatalf("expected the report to name the unused arg; output:\n%s", out)
		}
	})

	// Positive control: the same pack shape, well-formed, mounts + lints clean.
	t.Run("clean pack exits zero", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"warehouse/concepts.memql": `@version("1.0.0")
@namespace("warehouse")
@description("A gizmo.")
concept gizmo {
  label  string  @required  @description("Gizmo label.")
}`,
			"warehouse/mutations.memql": `use warehouse.concepts.{ gizmo }

@enabled
@description("Create a gizmo.")
mutate gizmo createGizmo {
  args {
    gizmoId  string  @required
    label    string  @required
  }
  insert {
    id: args.gizmoId
    args.label
  }
}`,
		})
		if code := run([]string{root}); code != 0 {
			t.Errorf("clean pack: run() = %d, want 0", code)
		}
	})
}
