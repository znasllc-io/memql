package main

// Integration tests for the memqllint CLI (znasllc-io/memql#2509): the
// referential-integrity lanes must surface through run()'s exit code and
// report, since downstream product CI consumes exactly this surface.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestRun_AMistypedKeywordIsOneRefusal: a top-level statement opened by a
// word no construct is spelled with is refused twice over -- by the parser,
// which Load runs over the whole file, and by the construct-keyword gate the
// parity pass runs at Init (memql#5356). memqllint printed both, with two
// keyword lists that disagreed: the parser's listed the internal `func`, the
// gate's listed `use`. The parser now raises the gate's own refusal from the
// one table (ConstructKeywords), and memqllint prints it once: the gate's
// copy, which carries the authored line. The retired `import ( ... )` block
// is the same statement-level refusal, naming its replacement.
func TestRun_AMistypedKeywordIsOneRefusal(t *testing.T) {
	cases := []struct {
		name, queries string
		want          []string
	}{
		{
			name:    "a typo'd construct keyword",
			queries: strings.Replace(testQueries, "query item queryItems", "qurey item queryItems", 1),
			want: []string{
				"demo/queries.memql:", "line 5: qurey is not a construct keyword: did you mean query?",
				"The constructs are: " + strings.Join(langparser.ConstructKeywords(), ", ") + " [construct_unknown]",
			},
		},
		{
			name:    "the retired import block",
			queries: "import (\n\t\"./concepts\"\n)\n\n" + testQueries,
			want: []string{
				"demo/queries.memql:", "line 1: the import ( ... ) block is retired: a construct is imported with a file-top use line",
				"[construct_unknown]",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := captureRun(t, []string{"--json", writeTree(t, map[string]string{
				"demo/concepts.memql": testConcepts,
				"demo/queries.memql":  tc.queries,
			})})
			if code != 1 {
				t.Fatalf("run() = %d, want 1\n%s", code, out)
			}
			var report Report
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("the --json report does not parse: %v\n%s", err, out)
			}
			if len(report.Errors) != 1 {
				t.Fatalf("one statement must be one refusal, got %d:\n%s", len(report.Errors), out)
			}
			for _, w := range tc.want {
				if !strings.Contains(report.Errors[0].Message, w) {
					t.Errorf("the refusal must carry %q, got %q", w, report.Errors[0].Message)
				}
			}
		})
	}

	// Positive control: the same query, spelled right and with no import
	// block, lints clean -- so the refusals above are the statement, and not
	// a fixture that fails for some other reason.
	if code, out := captureRun(t, []string{writeTree(t, map[string]string{
		"demo/concepts.memql": testConcepts,
		"demo/queries.memql":  testQueries,
	})}); code != 0 {
		t.Errorf("the control: run() = %d, want 0\n%s", code, out)
	}
}

// TestRun_RefusedLineOutsideTheParityMountIsStillReported: boot mounts every
// domain directory, but the parity pass mounts only one that directly holds a
// .memql file (MountOverlayDomains) -- so a domain holding only a
// sub-namespace refuses boot without its line and is Load's alone to report.
// memqllint drops Load's copy of a refusal only when the parity pass carries
// the same one, never merely because the parity pass ran: here it reports
// demo's (mounted, refused by both passes, printed once) and beta's (Load's
// alone), and a dedupe that dropped every Load refusal would lose beta's.
func TestRun_RefusedLineOutsideTheParityMountIsStillReported(t *testing.T) {
	files := map[string]string{
		"demo/concepts.memql":     testConcepts,
		"beta/sub/concepts.memql": "/// A widget.\nconcept widget {\n  label  string\n}\n",
	}
	code, out := captureRun(t, []string{"--json", writeTreeAsIs(t, files)})
	if code != 1 {
		t.Fatalf("two domains with no memql.toml: run() = %d, want 1\n%s", code, out)
	}
	var report Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the --json report does not parse: %v\n%s", err, out)
	}
	if len(report.Errors) != 2 {
		t.Fatalf("want exactly two diagnostics, the missing line of demo and of beta, got:\n%s", out)
	}
	for _, domain := range []string{"demo", "beta"} {
		n := 0
		for _, e := range report.Errors {
			if strings.Contains(e.Message, "[language_line_missing]") && strings.Contains(e.Message, `domain "`+domain+`" declares no language line`) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("want the missing line of domain %s exactly once, got %d:\n%s", domain, n, out)
		}
	}

	// Positive control: declaring both lines leaves nothing to report.
	line := dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render()
	files["demo/memql.toml"], files["beta/memql.toml"] = line, line
	if code, out := captureRun(t, []string{writeTreeAsIs(t, files)}); code != 0 {
		t.Errorf("the same domains declaring their lines: run() = %d, want 0\n%s", code, out)
	}
}

// jsonReport runs the CLI with --json and parses what it prints.
func jsonReport(t *testing.T, args ...string) (int, Report, string) {
	t.Helper()
	code, out := captureRun(t, append([]string{"--json"}, args...))
	var report Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the --json report does not parse: %v\n%s", err, out)
	}
	return code, report, out
}

// TestRun_ADomainDirectoryChecksItsOwnLanguageLine (memql#5356): pointed at
// one domain directory -- or a file inside it -- memqllint lints it as the
// bare root it always did, and checks the domain's OWN memql.toml, which no
// other pass can see from inside the domain. A missing file is one WARNING
// that leaves the exit code alone, because whether the directory is a
// mounted domain depends on the tree it is mounted in; a malformed or newer
// one is an error with its code. The bundle root still refuses the missing
// line as an error, as boot does.
func TestRun_ADomainDirectoryChecksItsOwnLanguageLine(t *testing.T) {
	bundle := writeTreeAsIs(t, map[string]string{
		"demo/concepts.memql": testConcepts,
		"demo/queries.memql":  testQueries,
	})
	domainDir := filepath.Join(bundle, "demo")
	for _, target := range []string{domainDir, filepath.Join(domainDir, "queries.memql")} {
		code, report, out := jsonReport(t, target)
		if code != 0 || len(report.Errors) != 0 {
			t.Errorf("%s with no memql.toml: run() = %d, want 0 with no error -- a missing line here is a warning:\n%s", target, code, out)
		}
		if len(report.Warnings) != 1 {
			t.Fatalf("%s: want exactly one warning, got:\n%s", target, out)
		}
		for _, want := range []string{
			`domain "demo" declares no language line, and boot refuses a mounted domain without one`,
			"Add demo/memql.toml containing",
			"memqlmigrate --rewrite=language-line -w " + bundle,
			"Linting " + bundle + ", the directory that holds it, checks it exactly as boot does",
		} {
			if !strings.Contains(report.Warnings[0].Message, want) {
				t.Errorf("%s: the warning must say %q, got %q", target, want, report.Warnings[0].Message)
			}
		}
	}
	// The human report prints it once, and exits as before.
	if code, out := captureRun(t, []string{domainDir}); code != 0 || strings.Count(out, "WARNING:") != 1 {
		t.Errorf("human output: run() = %d, want 0 with one WARNING line:\n%s", code, out)
	}

	// A file that is there is held to the loader's own checks.
	for text, code := range map[string]string{
		"memql = \"1.1\"\nedition = \"2026\"\n": "[language_version_newer]",
		"[language]\n":                          "[language_line_malformed]",
	} {
		if err := os.WriteFile(filepath.Join(domainDir, dslfs.ManifestFile), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		exit, report, out := jsonReport(t, domainDir)
		if exit != 1 || len(report.Errors) != 1 || !strings.Contains(report.Errors[0].Message, code) ||
			!strings.Contains(report.Errors[0].Message, `domain "demo"`) || len(report.Warnings) != 0 {
			t.Errorf("memql.toml %q: want exit 1 with one %s error naming demo and no warning, got %d:\n%s", text, code, exit, out)
		}
	}

	// Positive control: the line memqlmigrate writes, at the root, is clean.
	line := dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render()
	if err := os.WriteFile(filepath.Join(domainDir, dslfs.ManifestFile), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{domainDir, filepath.Join(domainDir, "queries.memql")} {
		if code, report, out := jsonReport(t, target); code != 0 || len(report.Errors) != 0 || len(report.Warnings) != 0 {
			t.Errorf("%s with its line declared: run() = %d, want 0 with nothing to report:\n%s", target, code, out)
		}
	}

	// And the bundle root, which boot mounts, still refuses a missing line.
	if err := os.Remove(filepath.Join(domainDir, dslfs.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	if code, report, out := jsonReport(t, bundle); code != 1 || len(report.Errors) != 1 ||
		!strings.Contains(report.Errors[0].Message, "[language_line_missing]") {
		t.Errorf("the bundle root: want exit 1 with the missing line as the one error, got %d:\n%s", code, out)
	}
}

// TestRun_ADirectoryNoMountReadsAsADomainGetsNoLanguageLineCheck: three
// targets that linted green before this epic, and must again -- with no
// language-line output at all, because none is a domain a mount would read:
// a single file inside a sub-namespace of a domain that holds .memql files
// (dsl/agents/roles), a sub-namespace of a core domain (dsl/shopify/
// generated), and a pack's dsl/ directory, which carries its own valid line.
// The last also printed a parity error while the root was mounted under its
// directory's name: the pack registers that tree as shopifypack, so a guessed
// name judged its @namespace against "dsl".
func TestRun_ADirectoryNoMountReadsAsADomainGetsNoLanguageLineCheck(t *testing.T) {
	for _, target := range []string{
		filepath.Join("..", "..", "dsl", "agents", "roles", "agriculture.memql"),
		filepath.Join("..", "..", "dsl", "shopify", "generated"),
		filepath.Join("..", "..", "examples", "shopifypack", "dsl"),
	} {
		code, report, out := jsonReport(t, target)
		if code != 0 || len(report.Errors) != 0 || len(report.Warnings) != 0 || strings.Contains(out, "language line") ||
			strings.Contains(out, "language_line") {
			t.Errorf("%s: run() = %d, want 0 with no language-line output:\n%s", target, code, out)
		}
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
