package compiler

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestMigrationHarness_CompileAllMemQLSources(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	// Walk the .memql source trees whose functions use the standard
	// parameterised form `func (Receiver) name(args) {...}` so the
	// compile gate catches parser / grammar drift at CI time the moment
	// a file changes. Previously only automations / queries / mutations
	// were covered; specs now included too.
	//
	// Skipped today: shapes, prompts, providers, tools, builtins. They
	// use a schema-in-body form (`func (Shape|Tool|Builtin) name {
	// fields }` without arg parens) parsed by a different pipeline.
	// Bringing them under this harness is a follow-up -- tracked in
	// docs/planning/memql-language-improvements.md Phase 2.
	roots := []string{
		"../../../dsl/v1/automations/v1",
		"../../../dsl/v1/queries/v1",
		"../../../dsl/v1/mutations/v1",
		"../../../dsl/v1/specs/v1",
	}
	var paths []string
	for _, root := range roots {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		paths = append(paths, collectMemQLFiles(t, root)...)
	}

	// Filter out builtin .memql files - these use func (Builtin) syntax
	// which is parsed by the self-contained builtin_parser, not the language compiler.
	filtered := paths[:0]
	for _, p := range paths {
		if strings.Contains(p, "/builtin/") && strings.HasPrefix(filepath.Base(p), "builtin") {
			continue
		}
		filtered = append(filtered, p)
	}
	paths = filtered

	sort.Strings(paths)

	if len(paths) == 0 {
		t.Fatal("no .memql files found for migration harness")
	}

	for _, path := range paths {
		path := path
		t.Run(path, func(t *testing.T) {
			sourceBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			source := string(sourceBytes)

			if _, err := CompileSource(source); err != nil {
				t.Fatalf("compile %s: %v", path, err)
			}
			if err := ValidateMemQL(source); err != nil {
				t.Fatalf("validate %s: %v", path, err)
			}
		})
	}
}

func TestMigrationHarness_InlineBlockInventory(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; this test scanned a path that no longer exists")
	paths := collectMemQLFiles(t, "../../../dsl/v1/automations/v1")
	sort.Strings(paths)

	inlinePatterns := []string{
		":= query {",
		":= mutation {",
		":= shape {",
		":= webhook {",
		":= event {",
		":= query if ",
		":= mutation if ",
		":= shape if ",
		":= webhook if ",
		":= event if ",
	}

	totalMatches := 0
	for _, path := range paths {
		contentBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		content := string(contentBytes)
		fileMatches := 0
		for _, pat := range inlinePatterns {
			fileMatches += strings.Count(content, pat)
		}
		if fileMatches > 0 {
			t.Logf("%s inline-blocks=%d", path, fileMatches)
			totalMatches += fileMatches
		}
	}

	if totalMatches == 0 {
		t.Log("inline block inventory: clean (no inline blocks)")
		return
	}

	t.Logf("inline block inventory total=%d", totalMatches)
}

// BenchmarkParseAllMemQL is the parse-time regression gate. It measures
// the combined parse + validate cost across every .memql file under
// the covered roots on every run, so changes to the parser that
// regress performance are visible without needing to instrument the
// engine boot. Baseline at Phase 2 landing (commit after 4fe9fc6) is
// roughly 20ms for the full inventory on an M-series laptop; run
// `go test -bench=BenchmarkParseAllMemQL ./component/language/compiler`
// to capture a new baseline after any parser change.
func BenchmarkParseAllMemQL(b *testing.B) {
	roots := []string{
		"../../../dsl/v1/automations/v1",
		"../../../dsl/v1/queries/v1",
		"../../../dsl/v1/mutations/v1",
		"../../../dsl/v1/specs/v1",
	}
	var paths []string
	for _, root := range roots {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() || filepath.Ext(path) != ".memql" {
				return nil
			}
			base := filepath.Base(path)
			if strings.HasPrefix(base, "_") {
				return nil
			}
			// Skip builtin/ directories -- those use the schema-in-body
			// form parsed by a different pipeline. Same exclusion the
			// compile harness applies.
			if strings.Contains(path, "/builtin/") && strings.HasPrefix(base, "builtin") {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			b.Fatalf("walk %s: %v", root, err)
		}
	}
	sources := make([][]byte, 0, len(paths))
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			b.Fatalf("read %s: %v", p, err)
		}
		sources = append(sources, src)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for idx, src := range sources {
			if _, err := CompileSource(string(src)); err != nil {
				b.Fatalf("compile %s: %v", paths[idx], err)
			}
		}
	}
}

func collectMemQLFiles(t *testing.T, root string) []string {
	t.Helper()

	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".memql" {
			if strings.HasPrefix(filepath.Base(path), "_") {
				return nil
			}
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	return paths
}
