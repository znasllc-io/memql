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

	"github.com/znasllc-io/memql/component/language/deprecation"
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
@description("A demo item.")
concept item {
  name    string  @required @description("Item name.")
  status  string  @description("Item status.")
}`

const testQueries = `use demo.concepts.{ item }

@description("A clean query.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  row => row.name == args.name
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
// word no construct is spelled with is refused by both passes -- by Load,
// which runs the construct-keyword gate over each whole file and whose parser
// raises the same refusal, and by the gate the parity pass runs at Init
// (memql#5356). memqllint printed two, with two keyword lists that disagreed:
// the parser's listed the internal `func`, the gate's listed `use`. The
// parser now raises the gate's own refusal from the one table
// (ConstructKeywords), and memqllint prints it once: Load's copy, which names
// the line of the file. The retired `import ( ... )` block is the same
// statement-level refusal, naming its replacement.
func TestRun_AMistypedKeywordIsOneRefusal(t *testing.T) {
	cases := []struct {
		name, queries string
		want          []string
	}{
		{
			name:    "a typo'd construct keyword",
			queries: strings.Replace(testQueries, "query item queryItems", "qurey item queryItems", 1),
			want: []string{
				"demo/queries.memql:", "line 4: qurey is not a construct keyword: did you mean query?",
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

// TestRun_ARefusalBothPassesMakeNamesTheFileLine (memql#5356): an annotation
// the registry refuses is refused by Load, which parses the whole file, and by
// the parity pass, whose query loader parses one construct at a time -- so the
// parity copy counts its line from the top of that construct ("line 3" here),
// and the rewriter that lowers the first query moves the parser's own count a
// line down. memqllint prints the refusal once, naming line 11, the line of
// the file the author wrote it on.
func TestRun_ARefusalBothPassesMakeNamesTheFileLine(t *testing.T) {
	queries := testQueries + `

@bogus
@description("A second query.")
query item queryByStatus {
  args {
    status  string  @required
  }
  filter  row => row.status == args.status
}`
	if got := strings.Split(queries, "\n")[10]; got != "@bogus" {
		t.Fatalf("the fixture's line 11 is %q, want @bogus", got)
	}
	code, report, out := jsonReport(t, writeTree(t, map[string]string{
		"demo/concepts.memql": testConcepts,
		"demo/queries.memql":  queries,
	}))
	if code != 1 || len(report.Errors) != 1 {
		t.Fatalf("one refused annotation: run() = %d, want 1 with exactly one error:\n%s", code, out)
	}
	msg := report.Errors[0].Message
	for _, want := range []string{"demo/queries.memql:", "at line 11, column 1:", `query "queryByStatus": unknown annotation @bogus`, "[annotation_unknown]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must carry %q, got %q", want, msg)
		}
	}
}

// TestRun_RefusedLineOutsideTheParityMountIsStillReported: boot mounts every
// domain directory, but the parity pass mounts only one that directly holds a
// .memql file (MountOverlayDomains) -- so a domain holding only a
// sub-namespace refuses boot without its line and is Load's alone to report.
// memqllint prints a refusal both passes make once, keeping Load's copy and
// dropping the parity pass's echo of it: here it reports demo's (mounted,
// refused by both passes, printed once) and beta's (Load's alone). A dedupe
// that kept both copies would print demo's twice, and one that dropped Load's
// copies whenever the parity pass ran would lose beta's.
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

// TestRun_ASubNamespaceOfADeclaredDomainGetsNoLanguageLineCheck: beta carries
// the memql.toml and keeps its files one level down, in beta/sub, a namespace
// of beta. Linting beta/sub -- or a file in it -- must not ask for the line of
// a domain "sub" that no mount reads: the parent's memql.toml makes the
// parent the domain.
func TestRun_ASubNamespaceOfADeclaredDomainGetsNoLanguageLineCheck(t *testing.T) {
	bundle := writeTreeAsIs(t, map[string]string{
		"beta/memql.toml":         dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render(),
		"beta/sub/concepts.memql": "/// A widget.\nconcept widget {\n  label  string\n}\n",
	})
	sub := filepath.Join(bundle, "beta", "sub")
	for _, target := range []string{sub, filepath.Join(sub, "concepts.memql")} {
		code, report, out := jsonReport(t, target)
		if code != 0 || len(report.Errors) != 0 || len(report.Warnings) != 0 || strings.Contains(out, "language line") ||
			strings.Contains(out, "language_line") {
			t.Errorf("%s: run() = %d, want 0 with no language-line output:\n%s", target, code, out)
		}
	}
	// Positive control: the bundle, which mounts beta, reads it as boot does,
	// and it is clean -- so the silence above is not a tree that fails to load.
	if code, report, out := jsonReport(t, bundle); code != 0 || len(report.Errors) != 0 {
		t.Errorf("the bundle: run() = %d, want 0 with no error:\n%s", code, out)
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

@description("Ghost module; ghost referenced here: ghost.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  row => row.name == args.name
}`,
			},
		},
		{
			name: "missing imported symbol",
			files: map[string]string{
				"demo/concepts.memql": testConcepts,
				"demo/queries.memql": `use demo.concepts.{ item, deletedConcept }

@description("deletedConcept was removed from the module; referenced here: deletedConcept.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  row => row.name == args.name
}`,
			},
		},
		{
			name: "insert field not on concept schema",
			files: map[string]string{
				"demo/concepts.memql": testConcepts,
				"demo/mutations.memql": `use demo.concepts.{ item }

@description("Writes an undeclared field.")
mutation item createItem {
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
				"demo/logic.memql": `
@description("Decides something.")
logic decideThing {
  args {
    event object @required
  }
  return true
}`,
				"demo/automations.memql": `use demo.logic.{ decideThing }

@trigger(event="graph.node.created.v1:demo:item")
@description("Step call renamed away from the import.")
automation onItemCreated {
  decide := logic decideThingX(event: event)
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
@description("A hub other rows point at.")
concept hub {
  name  string  @required  @description("Hub name.")
}

@version("1.0.0")
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
@description("A widget.")
concept widget {
  label  string  @required  @description("Widget label.")
}`,
			"warehouse/mutations.memql": `use warehouse.concepts.{ widget }

@description("Create a widget; declares an arg the body never references.")
mutation widget createWidget {
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
@description("A gizmo.")
concept gizmo {
  label  string  @required  @description("Gizmo label.")
}`,
			"warehouse/mutations.memql": `use warehouse.concepts.{ gizmo }

@description("Create a gizmo.")
mutation gizmo createGizmo {
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
query item queryParked {
  args {
    name  string  @required
  }
  filter  name == args.name
}
*/
query item queryItems {
  args {
    name  string  @required
  }
  filter  row => row.name == args.name
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

@description("A clean query.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  row => row.name == args.name
}

@description("Items missing a status, written with the retired null.")
query item unstatusedItems {
  args {
    name  string  @required
  }
  filter row => row.name == args.name && row.status == null
  paginate 10
}`
	specs := `use demo.concepts.{ item }

@description("An item with a name.")
spec item isNamed = row => row.name != ""

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

// lintProbeTrigger is the trigger the automation cases fire on.
const lintProbeTrigger = "@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\n"

var lintRewriteRefusalCases = []struct {
	name   string
	src    string
	needle string
	nth    int
	want   string
}{
	// Query clauses.
	{"refine without paginate", "query thing q {\n  filter row => row.a == 1\n  refine row => row.b == 2\n}\n", "refine", 1, "`refine` requires `paginate`"},
	{"refine with count", "query thing q {\n  filter row => row.a == 1\n  refine row => row.b == 2\n  count\n}\n", "refine", 1, "`refine` cannot be combined with `count`"},
	{"count with shape", "query thing q {\n  filter row => row.a == 1\n  shape thingCard\n  count\n}\n", "count", 1, "`count` and `shape` are mutually exclusive"},
	{"count with paginate", "query thing q {\n  filter row => row.a == 1\n  count\n  paginate 10\n}\n", "count", 1, "`count` cannot be combined with `sort` or `paginate`"},
	{"@unbounded with an empty reason", "@unbounded(\"  \")\nquery thing q {\n  filter row => row.a == 1\n}\n", "@unbounded", 1, "requires a non-empty reason string"},
	{"@unbounded with paginate", "@unbounded(\"every one\")\nquery thing q {\n  filter row => row.a == 1\n  paginate 10\n}\n", "@unbounded", 1, "cannot be combined with `paginate` or `sort`"},
	{"@unbounded with count", "@unbounded(\"every one\")\nquery thing q {\n  filter row => row.a == 1\n  count\n}\n", "@unbounded", 1, "cannot be combined with `count`"},
	{"an inline concept line", "query thing q {\n  concept thing\n  filter row => row.a == 1\n}\n", "concept", 1, "inline `concept` line is no longer supported"},
	{"an unknown clause", "query thing q {\n  filter row => row.a == 1\n  limit 10\n}\n", "limit", 1, "unknown struct-query field"},
	{"a body block in a query", "query thing q {\n  body {\n    return 1\n  }\n}\n", "body", 1, "must not declare a `body { }` block"},
	{"a query with no concept", "query listThings {\n  filter row => row.a == 1\n}\n", "listThings", 1, "missing concept binding"},
	{"an unclosed query", "query thing q {\n  filter row => row.a == 1\n", "{", 1, "missing closing brace"},

	// Mutation blocks and fields.
	{"two write blocks", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n  }\n  update {\n    id: args.id\n  }\n}\n", "update", 1, "exactly one write block"},
	{"a write block restating its concept", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert thing {\n    id: args.id\n  }\n}\n", "insert", 1, "is retired -- drop the restated concept"},
	{"an unknown block", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n  }\n  extra {\n    a: 1\n  }\n}\n", "extra", 1, "unexpected `extra { ... }` block"},
	{"a field outside the write block", "mutation thing m {\n  args {\n    id string @required\n  }\n  status: \"x\"\n  insert {\n    id: args.id\n  }\n}\n", "status: \"x\"", 1, "unexpected field"},
	{"a second nested accept", "mutation thing m {\n  args {\n    id string @required\n    name string\n  }\n  insert {\n    accept { id }\n    accept { name }\n  }\n}\n", "accept", 2, "more than one nested `accept"},
	{"a field beside a nested accept", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert {\n    accept { id }\n    status: \"x\"\n  }\n}\n", "status: \"x\"", 1, "carries the field"},
	{"an accept entry that is a key: value", "mutation thing m {\n  args {\n    id string @required\n  }\n  accept { id, status: \"active\" }\n}\n", "status: \"active\"", 1, "looks like a `key: value` pair"},
	{"an accepted field with no arg", "mutation thing m {\n  args {\n    id string @required\n  }\n  accept { id, name }\n}\n", "name", 1, "has no matching arg"},
	{"an empty accept", "mutation thing m {\n  args {\n    id string @required\n  }\n  accept { }\n}\n", "accept", 1, "block is empty"},
	{"a top-level accept beside an insert", "mutation thing m {\n  args {\n    id string @required\n  }\n  accept { id }\n  insert {\n    id: args.id\n  }\n}\n", "accept", 1, "cannot mix the accept/stamp form with an explicit"},
	{"a second id", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n    id: args.id\n  }\n}\n", "id:", 2, "duplicate `id:` line"},
	{"a bare mirror of a path", "mutation thing m {\n  args {\n    id string @required\n    user object\n  }\n  insert {\n    id: args.id\n    args.user.id\n  }\n}\n", "args.user.id", 1, "has no key"},
	{"an update with no id", "mutation thing m {\n  args {\n    name string\n  }\n  update {\n    name: args.name\n  }\n}\n", "update", 1, "update block requires an `id: <expr>` line"},
	{"a body block in a mutation", "mutation thing m {\n  body {\n    return 1\n  }\n}\n", "body", 1, "must not declare a `body { }` block"},

	// A logic and an automation are written in statements: their refusals
	// are the statement parser's and the load gate's rather than the
	// rewriter's (epic memql#5370), in
	// TestRun_StatementRefusalNamesTheAuthorsLineAndColumn.

	// Text the stages before moved: a spec StripNonProceduralBlocks folds to
	// one line, and a query the query stage lowers to fewer lines.
	{"a refine below a stripped spec and a lowered query", `use probe.concepts.{ thing }

spec thing isOpen = row => row.status == "open"

query thing first {
  args {
    a string @required
  }
  filter row => row.a == args.a
  sort "row.createdAt", "desc"
  paginate 10
}

query thing second {
  filter row => row.a == "x"
  refine row => row.b == 2
}
`, "refine", 1, "`refine` requires `paginate`"},
}

// TestRun_RewriteRefusalNamesTheAuthorsLineAndColumn: memqllint reports each
// refusal the struct-form rewriter makes of authored text at the file's line
// and column of that text -- `refine` without `paginate` at the refine clause,
// a second write block at its keyword -- where it used to print no position at
// all (memql#5364). Every case is its own file,
// since a file's rewrite stops at its first refusal; the table is the parser's
// (component/language/parser/rewrite_errors_test.go).
func TestRun_RewriteRefusalNamesTheAuthorsLineAndColumn(t *testing.T) {
	files := map[string]string{
		"probe/concepts.memql": `@version("1.0.0")
@description("A probe thing.")
concept thing {
  a       string  @description("A.")
  b       string  @description("B.")
  name    string  @description("Name.")
  status  string  @description("Status.")
}`,
	}
	for i, c := range lintRewriteRefusalCases {
		files["probe/case"+strconv.Itoa(i)+".memql"] = c.src
	}
	root := writeTree(t, files)
	code, out := captureRun(t, []string{root})
	if code != 1 {
		t.Fatalf("run() = %d, want 1; output:\n%s", code, out)
	}
	parity := 0
	for i, c := range lintRewriteRefusalCases {
		off, from := -1, 0
		for n := 0; n < c.nth; n++ {
			j := strings.Index(c.src[from:], c.needle)
			if j < 0 {
				t.Fatalf("%s: %q occurs fewer than %d times", c.name, c.needle, c.nth)
			}
			off, from = from+j, from+j+1
		}
		lineStart := strings.LastIndex(c.src[:off], "\n") + 1
		line, col := 1+strings.Count(c.src[:off], "\n"), 1+utf8.RuneCountInString(c.src[lineStart:off])
		file := "probe/case" + strconv.Itoa(i) + ".memql: "
		at := "rewrite error at line " + strconv.Itoa(line) + ", column " + strconv.Itoa(col) + ": "
		want := file + "parse: " + at
		idx := strings.Index(out, want)
		if idx < 0 {
			t.Errorf("%s: the report does not carry %q", c.name, want)
			continue
		}
		if rest := out[idx:]; !strings.Contains(rest[:strings.IndexByte(rest+"\n", '\n')], c.want) {
			t.Errorf("%s: the refusal at %d:%d is not the one the case makes (%q): %s", c.name, line, col, c.want, rest[:strings.IndexByte(rest+"\n", '\n')])
		}
		// The engine-parity pass, which compiles each construct from its own
		// slice of the file, names the same place.
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, file) && strings.Contains(l, "(parse): ") {
				parity++
				if !strings.Contains(l, "(parse): "+at) {
					t.Errorf("%s: the parity pass places it elsewhere: %s", c.name, l)
				}
			}
		}
	}
	if parity == 0 {
		t.Error("no case reached the engine-parity pass: the assertion on its positions checked nothing")
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}

// TestRun_StatementRefusalNamesTheAuthorsLineAndColumn: a refusal of a body
// written in statements (epic memql#5370) -- the statement parser's, and the
// load gate's (compiler.CheckBody) -- is reported at the line and column the
// author wrote, with its code. These are the inputs the rewriter refused
// before a logic and an automation were written in statements.
func TestRun_StatementRefusalNamesTheAuthorsLineAndColumn(t *testing.T) {
	files := map[string]string{
		"probe/emptyAutomation.memql": lintProbeTrigger + "automation noSteps {\n  args {\n    x string\n  }\n}\n",
		"probe/bodyBlock.memql":       lintProbeTrigger + "automation bodyBlock {\n  body {\n    x := 1\n  }\n}\n",
		"probe/emptyLogic.memql":      "logic noStatement {\n}\n",
	}
	code, out := captureRun(t, []string{writeTree(t, files)})
	if code != 1 {
		t.Fatalf("run() = %d, want 1; output:\n%s", code, out)
	}
	for _, c := range []struct{ file, at, code string }{
		{"probe/emptyAutomation.memql: ", "line 6, column 1", "[body_empty]"},
		{"probe/bodyBlock.memql: ", "line 3, column 3", "[body_block_retired]"},
		{"probe/emptyLogic.memql: ", "line 1:1", "[body_logic_return]"},
	} {
		found := false
		for _, l := range strings.Split(out, "\n") {
			found = found || (strings.Contains(l, c.file) && strings.Contains(l, c.at) && strings.HasSuffix(strings.TrimSpace(l), c.code))
		}
		if !found {
			t.Errorf("the report has no line for %s at %s ending in %s", c.file, c.at, c.code)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}

func TestRun_UncoveredAutomationCycleRefusesDirectory(t *testing.T) {
	root := writeTree(t, map[string]string{
		"demo/automations.memql": `@trigger(event="demo.loop")
automation loopProbe {
  publish "demo.loop" { value: 1 }
}`,
	})
	code, out := captureRun(t, []string{"--json", root})
	if code != 1 || !strings.Contains(out, "[loop_cycle]") {
		t.Fatalf("want loop_cycle refusal, code=%d output=%s", code, out)
	}
}

// deprecatedArrayConcepts declares a field in the deprecated `array(T)` form
// (memql#5390): `array(` starts at line 5, column 9.
const deprecatedArrayConcepts = `@version("1.0.0")
@description("A demo item.")
concept item {
  name  string  @required @description("Item name.")
  tags  array(string)  @description("Item tags.")
}`

// TestRun_ADeprecatedFormIsAWarningThatLeavesTheExitCodeAlone: a use of a
// deprecated form still inside its window loads, so the lint says where it is
// and what to write, as a WARNING line and in the --json warnings array, and
// exits as it would without it.
func TestRun_ADeprecatedFormIsAWarningThatLeavesTheExitCodeAlone(t *testing.T) {
	root := writeTree(t, map[string]string{
		"demo/concepts.memql": deprecatedArrayConcepts,
		"demo/queries.memql":  testQueries,
	})
	form, _ := deprecation.Lookup(deprecation.ArrayType)
	want := "demo/concepts.memql:5:9: " + form.Warning()

	code, out := captureRun(t, []string{root})
	if code != 0 {
		t.Fatalf("run() = %d, want 0: a deprecated form loads; output:\n%s", code, out)
	}
	if !strings.Contains(out, "WARNING: "+want+"\n") || strings.Count(out, "WARNING:") != 1 {
		t.Errorf("want exactly one line %q; output:\n%s", "WARNING: "+want, out)
	}

	code, report, out := jsonReport(t, root)
	if code != 0 || len(report.Errors) != 0 {
		t.Fatalf("--json: run() = %d with %d error(s), want 0 and none; output:\n%s", code, len(report.Errors), out)
	}
	if len(report.Warnings) != 1 || report.Warnings[0].Level != "warning" || report.Warnings[0].Message != want {
		t.Errorf("--json warnings = %+v, want one warning %q", report.Warnings, want)
	}
}

// TestRun_ADeprecatedFormDoesNotSilenceAnError: a warning is not a finding that
// stops the lanes behind it. The loop lint runs only on a tree the parity pass
// found clean, and a warning leaves it clean.
func TestRun_ADeprecatedFormDoesNotSilenceAnError(t *testing.T) {
	root := writeTree(t, map[string]string{
		"demo/concepts.memql": deprecatedArrayConcepts,
		"demo/automations.memql": `@trigger(event="demo.loop")
automation loopProbe {
  publish "demo.loop" { value: 1 }
}`,
	})
	code, out := captureRun(t, []string{root})
	if code != 1 || !strings.Contains(out, "[loop_cycle]") || !strings.Contains(out, "WARNING: demo/concepts.memql:5:9: ") {
		t.Fatalf("want the loop_cycle refusal and the warning, code=%d output=%s", code, out)
	}
}

// Past its window the same tree is REFUSED, with a non-zero exit and the
// replacement and the rewrite named. Only the release deciding changes.
func TestRun_AnExpiredFormIsAnErrorNamingTheReplacement(t *testing.T) {
	restore := deprecation.SetCurrent("0.25.0")
	defer restore()
	root := writeTree(t, map[string]string{
		"demo/concepts.memql": deprecatedArrayConcepts,
		"demo/queries.memql":  testQueries,
	})
	code, out := captureRun(t, []string{root})
	if code != 1 {
		t.Fatalf("run() = %d, want 1: past its window the form does not load; output:\n%s", code, out)
	}
	for _, want := range []string{"`[]T`", "memqlmigrate --rewrite=slice-syntax", deprecation.ArrayType} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name %q; output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WARNING: demo/concepts.memql") {
		t.Errorf("a form past its window is an error, not a warning; output:\n%s", out)
	}
}
