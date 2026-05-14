// audit-legacy-migration walks the new domain-first DSL tree
// (dsl/<domain>/...) and reads the "Sources:" headers each
// consolidated file carries (emitted by scripts/restructure-dsl).
// Each legacy path listed in those headers is recorded as
// "migrated." The tool then walks dsl/v1/ and reports every
// .memql / .tmpl file as either MIGRATED or UNMIGRATED.
//
// With --delete-migrated, the tool removes legacy files whose
// content is already present in the new tree. UNMIGRATED files
// are left in place so the operator can address them.
//
// Usage:
//
//	go run ./scripts/audit-legacy-migration                  # report only
//	go run ./scripts/audit-legacy-migration --delete-migrated # remove migrated legacy files
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

const (
	legacyRoot = "dsl/v1"
	newRoot    = "dsl"
)

var (
	sourceLineRe = regexp.MustCompile(`(?m)^//[ \t]*-[ \t]+(.+)$`)
)

func main() {
	deleteMigrated := flag.Bool("delete-migrated", false, "delete legacy files that have been migrated")
	flag.Parse()

	migrated, err := buildMigratedSet()
	if err != nil {
		die("build migrated set: %v", err)
	}
	fmt.Printf("Migrated source paths found in new tree headers: %d\n", len(migrated))

	tmplBasenames, err := buildTmplBasenameSet()
	if err != nil {
		die("build tmpl basename set: %v", err)
	}
	fmt.Printf("Template basenames present in new tree: %d\n", len(tmplBasenames))

	legacyFiles, err := findLegacyFiles()
	if err != nil {
		die("find legacy files: %v", err)
	}
	fmt.Printf("Legacy .memql + .tmpl files: %d\n", len(legacyFiles))

	var migratedFiles, unmigratedFiles []string
	for _, f := range legacyFiles {
		rel, err := filepath.Rel(legacyRoot, f)
		if err != nil {
			continue
		}
		key := filepath.ToSlash(rel)
		if migrated[key] || isSchemaReference(key) {
			migratedFiles = append(migratedFiles, f)
			continue
		}
		// .tmpl files are copied 1:1 by the restructure script and
		// don't carry source-headers in the new tree. Match by
		// basename: if the same .tmpl basename exists under any
		// dsl/<domain>/prompts/ folder it's considered migrated.
		if strings.HasSuffix(f, ".tmpl") {
			base := filepath.Base(f)
			if tmplBasenames[base] {
				migratedFiles = append(migratedFiles, f)
				continue
			}
		}
		unmigratedFiles = append(unmigratedFiles, f)
	}

	fmt.Printf("\nMIGRATED legacy files (safe to delete): %d\n", len(migratedFiles))
	fmt.Printf("UNMIGRATED legacy files: %d\n", len(unmigratedFiles))

	if len(unmigratedFiles) > 0 {
		fmt.Println("\nUnmigrated legacy files (need attention before deletion):")
		for _, f := range unmigratedFiles {
			fmt.Println("  -", f)
		}
	}

	if !*deleteMigrated {
		fmt.Println("\n(dry-run; pass --delete-migrated to remove migrated files)")
		return
	}

	if len(unmigratedFiles) > 0 {
		fmt.Println("\nABORT: unmigrated files exist; refusing to delete the migrated set until they are addressed.")
		os.Exit(1)
	}

	fmt.Println("\nDeleting migrated legacy files...")
	for _, f := range migratedFiles {
		if err := os.Remove(f); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: %s: %v\n", f, err)
		}
	}
	fmt.Println("Done.")
}

// buildMigratedSet scans every .memql in the new tree for the
// "// Sources:" header emitted by scripts/restructure-dsl. Each
// listed legacy path (relative to dsl/v1/) is added to the set.
func buildMigratedSet() (map[string]bool, error) {
	set := make(map[string]bool)
	err := filepath.Walk(newRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip the legacy subtree.
			if p == legacyRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".memql") && !strings.HasSuffix(p, ".tmpl") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// Only inspect the leading comment block (up to first non-//
		// line) to keep this fast and avoid false positives from
		// content inside bodies.
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				if m := sourceLineRe.FindStringSubmatch(line); m != nil {
					set[strings.TrimSpace(m[1])] = true
				}
				continue
			}
			break
		}
		return nil
	})
	return set, err
}

// buildTmplBasenameSet collects every .tmpl file under the new
// tree's prompts/ folders and returns the basenames. Used to
// verify that legacy .tmpl files are present at a new path.
func buildTmplBasenameSet() (map[string]bool, error) {
	set := make(map[string]bool)
	err := filepath.Walk(newRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if p == legacyRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".tmpl") {
			set[filepath.Base(p)] = true
		}
		return nil
	})
	return set, err
}

// findLegacyFiles returns every .memql + .tmpl under dsl/v1/,
// excluding schema reference files (`_*.memql`, `_*.jsonc`).
func findLegacyFiles() ([]string, error) {
	var out []string
	err := filepath.Walk(legacyRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".memql") && !strings.HasSuffix(name, ".tmpl") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// isSchemaReference returns true for `_*` files (schema reference
// docs that aren't loaded as runtime DSL content). These don't need
// to appear in the new tree.
func isSchemaReference(relPath string) bool {
	base := filepath.Base(relPath)
	return strings.HasPrefix(base, "_")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(2)
}
