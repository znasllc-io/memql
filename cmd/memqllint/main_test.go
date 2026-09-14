package main

// Integration tests for the memqllint CLI (znasllc-io/memql#2509): the
// referential-integrity lanes must surface through run()'s exit code and
// report, since downstream product CI consumes exactly this surface.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
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
// Every domain directory declares the engine's own language line unless the
// fixture writes one itself: a bundle domain must carry its own (memql#5357),
// and each test here is about the one defect its fixture names.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	withLines := make(map[string]string, len(files))
	for rel, content := range files {
		withLines[rel] = content
	}
	for rel := range files {
		if dir, base, ok := strings.Cut(rel, "/"); ok && !strings.Contains(base, "/") && strings.HasSuffix(base, ".memql") {
			if _, declared := withLines[dir+"/"+dslfs.ManifestFile]; !declared {
				withLines[dir+"/"+dslfs.ManifestFile] = dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render()
			}
		}
	}
	return writeTreeAsIs(t, withLines)
}

// writeTreeAsIs materializes exactly the files given, language line or not.
func writeTreeAsIs(t *testing.T, files map[string]string) string {
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

const testQueries = `use demo.concepts.{ item }

@enabled
@description("A clean query.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`

const testShapes = `use demo.concepts.{ item }

@description("An item card.")
@row
shape item itemCard {
  row.id
  name
}`

func TestRun_CleanTreeExitsZero(t *testing.T) {
	root := writeTree(t, map[string]string{
		"demo/concepts.memql": testConcepts,
		"demo/queries.memql":  testQueries,
	})
	if code := run([]string{root}); code != 0 {
		t.Errorf("clean tree: run() = %d, want 0", code)
	}
}

// TestRun_BundleWithoutLanguageLineExitsOne: memqllint is the pre-boot gate a
// product bundle has, so a domain the engine would refuse for declaring no
// language line (memql#5357) must fail the lint with the line to add -- as
// the domain's ONE diagnostic. The fixture carries a shape and a query bound
// to the concept, so anything read from the refused domain would cascade
// ("binds concept ... which does not resolve") and show here.
func TestRun_BundleWithoutLanguageLineExitsOne(t *testing.T) {
	files := map[string]string{
		"demo/concepts.memql": testConcepts,
		"demo/shapes.memql":   testShapes,
		"demo/queries.memql":  testQueries,
	}
	code, out := captureRun(t, []string{"--json", writeTreeAsIs(t, files)})
	if code != 1 {
		t.Fatalf("a bundle domain with no memql.toml: run() = %d, want 1\n%s", code, out)
	}
	var report Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the --json report does not parse: %v\n%s", err, out)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("want exactly one diagnostic, the missing line, got %d:\n%s", len(report.Errors), out)
	}
	if msg := report.Errors[0].Message; !strings.Contains(msg, "[language_line_missing]") || !strings.Contains(msg, "add demo/memql.toml containing") {
		t.Errorf("the one diagnostic must be the missing line naming the file to add, got %q", msg)
	}

	// Positive control: the same bundle declaring its line lints clean, so
	// the shape and query are constructs that load, and their silence above
	// is the refused domain being read by no loader.
	if code, out := captureRun(t, []string{writeTree(t, files)}); code != 0 {
		t.Errorf("the same bundle with its language line: run() = %d, want 0\n%s", code, out)
	}
}

// TestRun_UnreadRootManifestIsReported: a memql.toml at the root of the
// linted tree is never read -- each domain carries its own -- so memqllint
// prints it rather than letting an author believe it governs the bundle.
func TestRun_UnreadRootManifestIsReported(t *testing.T) {
	root := writeTree(t, map[string]string{
		"memql.toml":          dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render(),
		"demo/concepts.memql": testConcepts,
	})
	code, out := captureRun(t, []string{"--json", root})
	if code != 1 || !strings.Contains(out, "[language_line_unread]") {
		t.Errorf("a root-level memql.toml: run() = %d, want 1 with a language_line_unread diagnostic:\n%s", code, out)
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

  @relationship(type="assignedTo", field="hubId", target=hub, direction="outgoing")
}`,
		})
		code, out := captureRun(t, []string{root})
		if code != 1 {
			t.Fatalf("run() = %d, want 1; output:\n%s", code, out)
		}
		if !strings.Contains(out, `relationship type "assignedTo" is invalid`) {
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

// TestRun_ReportsParitySkippedDomains is the defect memql#2782 reports.
//
// The engine-parity pass mounts the linted root as an overlay on the embedded
// core tree, and MountOverlayDomains skips any top-level directory whose name
// collides with a core embedded domain -- the embedded tree already owns that
// namespace and RegisterTree would panic on the collision. The skip itself is
// correct. What was wrong is that it was invisible: the Warn went to a logger
// LintUnifiedTree hard-wires to io.Discard, and the mount returned no skip
// list, so a domain that got zero parity coverage reported exactly the same
// "OK" as one that was checked and found clean.
//
// The failure mode this protects against is a product bundle shipping a
// directory named after a core domain: its contents are never parity-checked,
// and nothing said so.
func TestRun_ReportsParitySkippedDomains(t *testing.T) {
	// "cluster" is a core embedded domain, so this directory is skipped by
	// the parity overlay. It is otherwise clean, so the run still exits 0 --
	// a skip is information, not a diagnostic.
	coreNamed := map[string]string{
		"cluster/concepts.memql": `@version("1.0.0")
@namespace("cluster")
@description("A bundle-supplied row under a core domain name.")
concept shadowed {
  name  string  @required  @description("Row name.")
}`,
	}

	t.Run("names the skipped domain", func(t *testing.T) {
		code, out := captureRun(t, []string{writeTree(t, coreNamed)})
		if code != 0 {
			t.Fatalf("run() = %d, want 0 (a parity skip is informational, not a diagnostic); output:\n%s", code, out)
		}
		if !strings.Contains(out, "cluster") {
			t.Errorf("report must name the skipped domain %q so the gap is visible; output:\n%s", "cluster", out)
		}
		if !strings.Contains(out, "parity") {
			t.Errorf("report must say the domain was skipped from the engine-parity pass; output:\n%s", out)
		}
	})

	t.Run("stays quiet when nothing was skipped", func(t *testing.T) {
		// A non-colliding domain mounts normally; the report must not grow a
		// skip note, or the signal is noise on every clean run.
		code, out := captureRun(t, []string{writeTree(t, map[string]string{
			"demo/concepts.memql": testConcepts,
		})})
		if code != 0 {
			t.Fatalf("run() = %d, want 0; output:\n%s", code, out)
		}
		if strings.Contains(out, "parity") {
			t.Errorf("no domain was skipped, so the report must carry no parity-skip note; output:\n%s", out)
		}
	})
}

// The orphaned-preamble lane (memql#2965) must reach the CLI's exit code.
//
// This exists because the lane's own unit tests call Tree.VerifyPreambleAttachment
// directly, so deleting the one line in run() that calls it left the ENTIRE repo
// green -- measured during the memql#3041 review. A correct rule that nothing
// invokes is the same silent-absence shape memql#2965 is itself an instance of,
// which makes the wiring worth a test of its own rather than an assumption.
func TestRun_OrphanedPreambleFindingExitsOne(t *testing.T) {
	root := writeTree(t, map[string]string{
		"demo/concepts.memql": testConcepts,
		// The @public is orphaned by the block comment: the loader registers
		// queryItems WITHOUT it, and nothing else in the toolchain says so.
		"demo/queries.memql": `use demo.concepts.{ item }

@public
@description("intentionally caller-scope-free")
/*
@enabled
query item queryParked {
  args {
    name  string  @required
  }
  filter  name == args.name
}
*/
@enabled
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`,
	})

	code, out := captureRun(t, []string{root})
	if code != 1 {
		t.Fatalf("an orphaned preamble must fail the lint; run() = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "attached to nothing") {
		t.Errorf("the report must carry the orphaned-preamble diagnostic; output:\n%s", out)
	}
	if !strings.Contains(out, "@public") {
		t.Errorf("the report must quote what was orphaned, or the author cannot tell what was "+
			"lost; output:\n%s", out)
	}
	if !strings.Contains(out, "demo/queries.memql") {
		t.Errorf("the report must name the file; output:\n%s", out)
	}
}

// TestRun_UnregisteredConnectorRefusesTheMountedBundle is issue #4386's
// negative witness for epic memql#4378, and the reason it lives HERE
// rather than in test/dslconformance is the reason memqllint exists at
// all: this is the engine-parity pass, so the bundle is mounted as an
// overlay and MemQLEngine.Init runs over the merged tree -- the same
// path a product bundle takes through MEMQL_DSL_PATH at boot.
//
// A conformance test over this repo's own dsl/ cannot cover it. The
// declarations that matter to an operator are the ones in THEIR bundle,
// and no Go test in this repository walks those (memql#4051). What makes
// the check reach them is that it runs inside Init.
//
// The gate is sighted here because cmd/memqllint blank-imports the
// connector packages: a build that declares no connectors cannot tell a
// typo from a correct name and says so instead of refusing.
func TestRun_UnregisteredConnectorRefusesTheMountedBundle(t *testing.T) {
	t.Run("an origin naming a connector nobody serves", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"storefront/concepts.memql": `@version("1.0.0")
@namespace("storefront")
@description("A mirror of a system this build has never heard of.")
@origin("nowhere")
concept phantom {
  label  string  @required  @description("Label.")
}`,
		})
		code, out := captureRun(t, []string{root})
		if code != 1 {
			t.Fatalf("run() = %d, want 1 -- a mirror nobody fills reads as an empty catalog, which is why "+
				"an unresolvable @origin refuses boot; output:\n%s", code, out)
		}
		for _, want := range []string{"nowhere", "does not serve"} {
			if !strings.Contains(out, want) {
				t.Fatalf("the report never says %q; an operator has to learn WHICH name failed and that this "+
					"build does not serve it. Output:\n%s", want, out)
			}
		}
	})

	t.Run("a mirror target nobody drains", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"storefront/concepts.memql": `@version("1.0.0")
@namespace("storefront")
@description("MemQL-origin data pushed to a system nothing drains.")
@mirroredTo("nowhere")
concept ledger {
  label  string  @required  @description("Label.")
}`,
		})
		code, out := captureRun(t, []string{root})
		if code != 1 {
			t.Fatalf("run() = %d, want 1 -- an undrained mirror target accumulates outbox entries forever; output:\n%s", code, out)
		}
		if !strings.Contains(out, "nowhere") {
			t.Fatalf("the report never names the unresolvable target; output:\n%s", out)
		}
	})

	// The positive control. Without it a refusal above could equally be
	// caused by the fixture failing to load for some unrelated reason,
	// and the test would pass while measuring nothing.
	t.Run("a REGISTERED connector name loads", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"storefront/concepts.memql": `@version("1.0.0")
@namespace("storefront")
@description("A mirror of a system this build does serve.")
@origin("shopify")
concept mirrored {
  label  string  @required  @description("Label.")
}`,
		})
		code, out := captureRun(t, []string{root})
		if code != 0 {
			t.Fatalf("run() = %d, want 0 -- \"shopify\" is declared by this build, so the same fixture shape "+
				"must load. If this fails the refusals above prove nothing about connector names; output:\n%s", code, out)
		}
	})
}

// TestRun_V1RefusalNamesTheAuthorsLineAndColumn: memqllint reports a refusal
// inside a construct the rewriter lowered at the file's line and column --
// from the parse pass and from the engine-parity pass alike, for a construct
// that is not the file's first (memql#5364). Both passes used to print the
// lowered text's coordinates, and the parity pass's were relative to the
// construct's slice besides.
func TestRun_V1RefusalNamesTheAuthorsLineAndColumn(t *testing.T) {
	queries := `use demo.concepts.{ item }

@enabled
@description("A clean query.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}

@enabled
@description("Items missing a status, written with the retired null.")
query item unstatusedItems {
  args {
    name  string  @required
  }
  filter row => row.name == args.name && row.status == null
  paginate 10
}`
	specs := `use demo.concepts.{ item }

@enabled
@description("An item with a name.")
spec item isNamed {
  return name != ""
}

@enabled
@description("An item with a status, written with the retired null.")
spec item hasStatus = row => row.status != null`
	root := writeTree(t, map[string]string{
		"demo/concepts.memql": testConcepts,
		"demo/queries.memql":  queries,
		"demo/specs.memql":    specs,
	})
	code, out := captureRun(t, []string{root})
	if code != 1 {
		t.Fatalf("run() = %d, want 1; output:\n%s", code, out)
	}
	// at is the refusal of the `null` that ends the comparison cmp, at the
	// line and column it has in src as written.
	at := func(src, cmp string) string {
		off := strings.Index(src, cmp) + len(cmp) - len("null")
		lineStart := strings.LastIndex(src[:off], "\n") + 1
		return "parse error at line " + strconv.Itoa(1+strings.Count(src[:off], "\n")) + ", column " + strconv.Itoa(1+utf8.RuneCountInString(src[lineStart:off])) + ": null is retired"
	}
	for _, want := range []string{
		"demo/queries.memql: parse: parser error: " + at(queries, "row.status == null"),
		`demo/queries.memql: query "unstatusedItems" (parse): ` + at(queries, "row.status == null"),
		"demo/specs.memql: parse: parser error: " + at(specs, "row.status != null"),
		`demo/specs.memql: spec "hasStatus" (parse): unified:demo/specs.memql:hasStatus: ` + at(specs, "row.status != null"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q; output:\n%s", want, out)
		}
	}
}
