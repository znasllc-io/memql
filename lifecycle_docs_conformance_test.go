package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLifecycleDocsMatchRuling is the enforcement gate for the lifecycle
// ruling's documentation (#2609): absent = enabled, @enabled = accepted
// no-op, @disabled = the only off-switch, with real gates on every kind
// (#2604-#2608). The phrases below are the drift shapes four review rounds
// kept finding -- each claims the pre-ruling world and must never reappear
// in docs, doc strings, or reference sheets.
func TestLifecycleDocsMatchRuling(t *testing.T) {
	banned := []string{
		"Functions are disabled by default",
		"Automations are **disabled by default**",
		"Automations are disabled by default",
		"Must use `@enabled`",
		"@enabled` to activate",
		"required to use it)",
		"all registered user-defined functions",
	}

	roots := []string{"docs", "component", "dsl"}
	skipDirs := map[string]bool{".git": true, "bin": true, "node_modules": true}
	self := "lifecycle_docs_conformance_test.go"

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".md", ".go", ".memql":
			default:
				return nil
			}
			if filepath.Base(path) == self {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			content := string(data)
			for _, phrase := range banned {
				if idx := strings.Index(content, phrase); idx >= 0 {
					line := 1 + strings.Count(content[:idx], "\n")
					t.Errorf("%s:%d claims the pre-ruling lifecycle (%q); rewrite to the #2609 semantics", path, line, phrase)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
}
