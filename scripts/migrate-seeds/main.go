// migrate-seeds converts all seed.json files to seed.memql format.
//
// Usage:
//
//	go run scripts/migrate-seeds/main.go [--dry-run]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

var dryRun = flag.Bool("dry-run", false, "Print what would be generated without writing files")

func main() {
	flag.Parse()

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	conceptsDir := filepath.Join(root, "dsl", "v1", "concepts", "v1")
	var seedFiles []string

	err = filepath.Walk(conceptsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.EqualFold(info.Name(), "seed.json") {
			seedFiles = append(seedFiles, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Found %d seed.json files\n", len(seedFiles))

	for _, seedPath := range seedFiles {
		if err := migrateOneSeed(seedPath); err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: %s: %v\n", seedPath, err)
		}
	}

	fmt.Println("\nSUCCESS: Seed migration complete.")
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod")
		}
		dir = parent
	}
}

type seedFile struct {
	Actor   string           `json:"actor"`
	Records []seedFileRecord `json:"records"`
}

type seedFileRecord struct {
	ID      string         `json:"id"`
	Actor   string         `json:"actor"`
	Payload map[string]any `json:"payload"`
	Match   []seedMatch    `json:"match"`
	Note    string         `json:"_note"`
}

type seedMatch struct {
	Field string `json:"field"`
	Value any    `json:"value"`
}

func migrateOneSeed(jsonPath string) error {
	dir := filepath.Dir(jsonPath)
	memqlPath := filepath.Join(dir, "seed.memql")

	if _, err := os.Stat(memqlPath); err == nil {
		fmt.Printf("  SKIP %s (seed.memql already exists)\n", dir)
		return nil
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}

	var parsed seedFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	if len(parsed.Records) == 0 {
		// Empty seed file — write empty seed.memql
		content := "// No seed data\n"
		if *dryRun {
			fmt.Printf("  DRY RUN: %s (empty)\n", memqlPath)
			return nil
		}
		if err := os.WriteFile(memqlPath, []byte(content), 0644); err != nil {
			return err
		}
		fmt.Printf("  MIGRATED %s (empty)\n", memqlPath)
		return nil
	}

	content := generateSeedMemQL(&parsed)

	if *dryRun {
		fmt.Printf("  DRY RUN: %s (%d records, %d bytes)\n", memqlPath, len(parsed.Records), len(content))
		return nil
	}

	if err := os.WriteFile(memqlPath, []byte(content), 0644); err != nil {
		return err
	}

	fmt.Printf("  MIGRATED %s (%d records)\n", memqlPath, len(parsed.Records))
	return nil
}

func generateSeedMemQL(seed *seedFile) string {
	var sb strings.Builder

	if seed.Actor != "" {
		sb.WriteString(fmt.Sprintf("@actor(%q)\n\n", seed.Actor))
	}

	for i, rec := range seed.Records {
		if i > 0 {
			sb.WriteString("\n")
		}

		// Comment from _note
		if rec.Note != "" {
			sb.WriteString(fmt.Sprintf("// %s\n", rec.Note))
		}

		sb.WriteString(fmt.Sprintf("seed %q", rec.ID))

		// Per-record annotations
		if rec.Actor != "" {
			sb.WriteString(fmt.Sprintf(" @actor(%q)", rec.Actor))
		}
		for _, m := range rec.Match {
			sb.WriteString(fmt.Sprintf(" @match(field=%q, value=%q)", m.Field, fmt.Sprintf("%v", m.Value)))
		}

		sb.WriteString(" {\n")
		writePayload(&sb, rec.Payload, "  ")
		sb.WriteString("}\n")
	}

	return sb.String()
}

func writePayload(sb *strings.Builder, payload map[string]any, indent string) {
	// Sort keys for deterministic output
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := payload[key]
		writeValue(sb, key, value, indent)
	}
}

func writeValue(sb *strings.Builder, key string, value any, indent string) {
	if value == nil {
		sb.WriteString(fmt.Sprintf("%s%s  null\n", indent, key))
		return
	}

	switch v := value.(type) {
	case map[string]any:
		sb.WriteString(fmt.Sprintf("%s%s {\n", indent, key))
		writePayload(sb, v, indent+"  ")
		sb.WriteString(fmt.Sprintf("%s}\n", indent))
	case []any:
		sb.WriteString(fmt.Sprintf("%s%s  [", indent, key))
		for i, item := range v {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(formatScalar(item))
		}
		sb.WriteString("]\n")
	case string:
		sb.WriteString(fmt.Sprintf("%s%s  %q\n", indent, key, v))
	case bool:
		sb.WriteString(fmt.Sprintf("%s%s  %v\n", indent, key, v))
	case float64:
		// JSON numbers are float64
		if v == float64(int64(v)) {
			sb.WriteString(fmt.Sprintf("%s%s  %d\n", indent, key, int64(v)))
		} else {
			sb.WriteString(fmt.Sprintf("%s%s  %g\n", indent, key, v))
		}
	case json.Number:
		sb.WriteString(fmt.Sprintf("%s%s  %s\n", indent, key, v.String()))
	default:
		sb.WriteString(fmt.Sprintf("%s%s  %q\n", indent, key, fmt.Sprintf("%v", v)))
	}
}

// formatScalar renders a seed JSON value as a MemQL literal.
//
// Strings go through langparser.QuoteString, not fmt.Sprintf("%q"): the output
// is .memql SOURCE that the loader's lexer reads, and %q's escape set is not
// the one that lexer implements. A control byte in a seed value emitted
// `\x00` / `\a` / `\v`, none of which it knows, so the generated file would
// not load. memql#3192.
func formatScalar(value any) string {
	if value == nil {
		return "null"
	}
	switch v := value.(type) {
	case string:
		return langparser.QuoteString(v)
	case bool:
		return fmt.Sprintf("%v", v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	default:
		return langparser.QuoteString(fmt.Sprintf("%v", v))
	}
}
