// add-imports-to-legacy walks every .memql file under dsl/v1/ outside
// of concepts/, finds @useConcept(<name>...) annotations, looks up
// the concept's directory, and prepends an `import (...)` block at
// the file top with each concept aliased to its bare name.
//
// This is the additive @useConcept -> import migration agreed for
// Commit 2's syntax sweep. It does NOT remove the @useConcept
// annotations -- those continue to drive the engine's legacy
// concept resolver. When the engine cutover lands (Pass 2 of the
// restructure), the @useConcept annotations get retired in favor
// of the imports. Until then, both coexist; the legacy resolver
// reads @useConcept, the new resolver reads imports.
//
// Usage:
//
//	go run ./scripts/add-imports-to-legacy           # apply
//	go run ./scripts/add-imports-to-legacy --dry-run # report only
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const root = "dsl/v1"

var (
	useConceptRe     = regexp.MustCompile(`(?m)^@useConcept\(([^)]+)\)`)
	existingImportRe = regexp.MustCompile(`(?m)^import[ \t]*\(`)

	// reservedAliases collide with engine-provided body identifiers.
	// Concepts whose bare name matches one of these can't be aliased
	// to themselves without confusing the new resolver. Until the
	// body references get rewritten (separate sweep), skip the import
	// for these concepts -- the legacy @useConcept resolver continues
	// to handle them.
	reservedAliases = map[string]bool{
		"actor": true, "now": true, "partition": true, "config": true, "trace": true,
	}
)

func main() {
	dryRun := flag.Bool("dry-run", false, "report changes without writing")
	flag.Parse()

	conceptIndex, err := buildConceptIndex()
	if err != nil {
		die("build concept index: %v", err)
	}
	fmt.Printf("Built concept index: %d entries\n", len(conceptIndex))

	files, err := findEligibleFiles()
	if err != nil {
		die("find files: %v", err)
	}
	fmt.Printf("Scanning %d eligible .memql files\n", len(files))

	var modified, skipped, alreadyMigrated int
	var failures []string

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: read: %v", f, err))
			continue
		}
		content := string(raw)

		if existingImportRe.MatchString(content) {
			alreadyMigrated++
			continue
		}

		concepts := extractUseConcepts(content)
		if len(concepts) == 0 {
			skipped++
			continue
		}

		entries, missing := resolveConceptPaths(f, concepts, conceptIndex)
		if len(missing) > 0 {
			// Unresolved concepts are pre-existing bugs (stale
			// @useConcept references for concepts that no longer
			// exist). Log as warning + continue with what we can
			// resolve so the migration isn't blocked by author
			// bugs in the legacy tree.
			fmt.Fprintf(os.Stderr, "WARNING: %s: unresolved concepts: %s (skipping those entries)\n",
				f, strings.Join(missing, ", "))
		}
		if len(entries) == 0 {
			skipped++
			continue
		}

		newContent := insertImportBlock(content, entries)

		if *dryRun {
			fmt.Printf("WOULD ADD imports to %s (%d concept(s))\n", f, len(entries))
		} else {
			if err := os.WriteFile(f, []byte(newContent), 0644); err != nil {
				failures = append(failures, fmt.Sprintf("%s: write: %v", f, err))
				continue
			}
		}
		modified++
	}

	fmt.Printf("\nSummary: modified=%d, alreadyMigrated=%d, skipped=%d, failures=%d\n",
		modified, alreadyMigrated, skipped, len(failures))
	if len(failures) > 0 {
		fmt.Println("\nFailures:")
		for _, msg := range failures {
			fmt.Println("  -", msg)
		}
		os.Exit(1)
	}
}

// buildConceptIndex walks dsl/v1/concepts/v1/ and maps each concept's
// trailing-segment name to its directory path relative to dsl/v1/.
// Same matching rule the legacy concept resolver uses: bare name vs
// trailing path segment.
//
// Returns the map plus an error if any name collides (two concepts
// with the same trailing segment in different namespaces). In the
// current tree this shouldn't happen but the loader treats it as a
// fatal author error today; the migration should match that rule.
func buildConceptIndex() (map[string]string, error) {
	out := make(map[string]string)
	conceptsRoot := filepath.Join(root, "concepts", "v1")
	err := filepath.Walk(conceptsRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Name() != "concept.memql" {
			return nil
		}
		// p is e.g. dsl/v1/concepts/v1/cognition/space/concept.memql.
		// Bare name is the parent directory's name. Path is the
		// directory relative to dsl/v1/.
		dir := filepath.Dir(p)
		name := filepath.Base(dir)
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		if existing, ok := out[name]; ok && existing != rel {
			return fmt.Errorf("concept name collision: %q matches both %q and %q",
				name, existing, rel)
		}
		out[name] = filepath.ToSlash(rel)
		return nil
	})
	return out, err
}

// findEligibleFiles returns every .memql under dsl/v1/ outside of
// concepts/ and outside schema-reference files (those prefixed with `_`).
func findEligibleFiles() ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if filepath.Base(p) == "concepts" && filepath.Dir(p) == root {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".memql") {
			return nil
		}
		if strings.HasPrefix(name, "_") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// extractUseConcepts returns every concept name listed across all
// @useConcept(...) annotations in the source. Duplicates are
// deduplicated. Comma-separated entries are split.
func extractUseConcepts(content string) []string {
	var raw []string
	for _, m := range useConceptRe.FindAllStringSubmatch(content, -1) {
		for _, name := range strings.Split(m[1], ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			raw = append(raw, name)
		}
	}
	// Dedupe + sort for stable output.
	seen := make(map[string]bool, len(raw))
	var out []string
	for _, n := range raw {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// resolveConceptPaths maps each concept name to its (importPath,
// alias) pair, computing a relative path from the importing file's
// directory to the concept.memql.
//
// The import path always uses "concept" as the basename (the
// concept files are named concept.memql) and explicitly aliases to
// the concept name so body references like `space.spaceId` continue
// to work without re-targeting.
func resolveConceptPaths(importingFile string, concepts []string, index map[string]string) ([]importEntry, []string) {
	var entries []importEntry
	var missing []string
	importingDir := filepath.Dir(importingFile)
	for _, name := range concepts {
		// Skip reserved-alias concepts (partition, config, ...).
		// The legacy @useConcept resolver keeps handling them; the
		// body-rewrite sweep that retargets `<name>.X` to a non-
		// reserved alias happens in the engine-cutover session.
		if reservedAliases[name] {
			continue
		}
		conceptDir, ok := index[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		conceptDirAbs := filepath.Join(root, conceptDir)
		rel, err := filepath.Rel(importingDir, conceptDirAbs)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		relSlash := filepath.ToSlash(rel)
		// The parser auto-appends .memql. We point at the "concept"
		// basename inside the concept's directory.
		importPath := relSlash + "/concept"
		entries = append(entries, importEntry{
			Path:  importPath,
			Alias: name,
		})
	}
	return entries, missing
}

type importEntry struct {
	Path  string
	Alias string
}

// insertImportBlock prepends an `import (...)` block at the top of
// the file, after any leading comment block but before any other
// content. Preserves the file's existing header comments.
func insertImportBlock(content string, entries []importEntry) string {
	// Find insertion point: after the leading run of comment lines
	// + blank lines. The block goes there.
	lines := strings.SplitAfter(content, "\n")
	idx := 0
	for idx < len(lines) {
		line := strings.TrimSpace(strings.TrimRight(lines[idx], "\n"))
		if line == "" || strings.HasPrefix(line, "//") {
			idx++
			continue
		}
		break
	}

	var block strings.Builder
	block.WriteString("import (\n")
	for _, e := range entries {
		// Always alias so body refs resolve to the concept name
		// (the basename is "concept", which would shadow nothing).
		block.WriteString(fmt.Sprintf("\t%q as %s\n", e.Path, e.Alias))
	}
	block.WriteString(")\n")
	block.WriteString("\n")

	prefix := strings.Join(lines[:idx], "")
	suffix := strings.Join(lines[idx:], "")
	if prefix != "" && !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	// Ensure exactly one blank line between header comments and the import block.
	prefix = strings.TrimRight(prefix, "\n") + "\n"
	if prefix != "\n" && prefix != "" {
		prefix += "\n"
	}
	return prefix + block.String() + suffix
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(2)
}
