// migrate-concepts reads all JSON-based concept definitions and generates concept.memql files.
// It also scans .memql function/automation files to add `use` declarations and replace hardcoded concept strings.
//
// Usage:
//
//	go run scripts/migrate-concepts/main.go [--dry-run] [--concepts-only] [--memql-only]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	dryRun       = flag.Bool("dry-run", false, "Print what would be generated without writing files")
	conceptsOnly = flag.Bool("concepts-only", false, "Only migrate concept JSON to concept.memql")
	memqlOnly    = flag.Bool("memql-only", false, "Only add use declarations to .memql files")
)

func main() {
	flag.Parse()

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	if !*memqlOnly {
		if err := migrateConceptsToMemQL(root); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR migrating concepts: %v\n", err)
			os.Exit(1)
		}
	}

	if !*conceptsOnly {
		if err := addUseDeclarations(root); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR adding use declarations: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("\nSUCCESS: Migration complete.")
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
			return "", fmt.Errorf("could not find go.mod in any parent directory")
		}
		dir = parent
	}
}

// --- Part 1: Migrate JSON concepts to concept.memql ---

type conceptMetadata struct {
	Description     string                `json:"description"`
	Type            string                `json:"type"`
	CacheTTLSeconds *int64                `json:"cacheTTLSeconds"`
	SkipDeleted     *bool                 `json:"skipDeleted"`
	EnforceRequired *bool                 `json:"enforceRequired"`
	DefaultFilter   string                `json:"defaultFilter"`
	Relationships   []relationshipMeta    `json:"relationships"`
}

type relationshipMeta struct {
	Type          string `json:"type"`
	Field         string `json:"field"`
	FieldSource   string `json:"fieldSource"`
	TargetConcept string `json:"targetConcept"`
	Direction     string `json:"direction"`
}

type jsonSchema struct {
	ID                   string                    `json:"$id"`
	AdditionalProperties *bool                     `json:"additionalProperties"`
	Properties           map[string]jsonProperty   `json:"properties"`
	Required             []string                  `json:"required"`
}

type jsonProperty struct {
	Type        any               `json:"type"`
	Description string            `json:"description"`
	Default     any               `json:"default"`
	Enum        []any             `json:"enum"`
	Format      string            `json:"format"`
	Properties  map[string]jsonProperty `json:"properties"`
	Items       *jsonProperty     `json:"items"`
}

func migrateConceptsToMemQL(root string) error {
	conceptsDir := filepath.Join(root, "dsl", "v1", "concepts", "v1")

	var dirs []string
	err := filepath.Walk(conceptsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Name() == "definition.json" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("Found %d concepts to migrate\n", len(dirs))

	for _, dir := range dirs {
		if err := migrateOneConcept(dir); err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: skipping %s: %v\n", dir, err)
		}
	}

	return nil
}

func migrateOneConcept(dir string) error {
	// Skip if concept.memql already exists
	memqlPath := filepath.Join(dir, "concept.memql")
	if _, err := os.Stat(memqlPath); err == nil {
		fmt.Printf("  SKIP %s (concept.memql already exists)\n", dir)
		return nil
	}

	// Read definition.json
	defBytes, err := os.ReadFile(filepath.Join(dir, "definition.json"))
	if err != nil {
		return fmt.Errorf("read definition.json: %w", err)
	}

	var schema jsonSchema
	if err := json.Unmarshal(defBytes, &schema); err != nil {
		return fmt.Errorf("parse definition.json: %w", err)
	}

	// Read concept.json
	metaBytes, err := os.ReadFile(filepath.Join(dir, "concept.json"))
	if err != nil {
		return fmt.Errorf("read concept.json: %w", err)
	}

	var meta conceptMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return fmt.Errorf("parse concept.json: %w", err)
	}

	// Derive concept display name from directory
	displayName := deriveDisplayName(dir)

	// Generate concept.memql content
	content := generateConceptMemQL(displayName, &schema, &meta)

	if *dryRun {
		fmt.Printf("  DRY RUN: would write %s (%d bytes)\n", memqlPath, len(content))
		return nil
	}

	if err := os.WriteFile(memqlPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write concept.memql: %w", err)
	}

	fmt.Printf("  MIGRATED %s\n", memqlPath)
	return nil
}

func deriveDisplayName(dir string) string {
	base := filepath.Base(dir)
	if len(base) > 0 {
		return strings.ToUpper(base[:1]) + base[1:]
	}
	return base
}

func generateConceptMemQL(displayName string, schema *jsonSchema, meta *conceptMetadata) string {
	var sb strings.Builder

	// Concept-level annotations
	desc := strings.TrimSpace(meta.Description)
	if desc != "" {
		sb.WriteString(fmt.Sprintf("@description(%q)\n", desc))
	}

	if meta.Type != "" && meta.Type != "object" {
		sb.WriteString(fmt.Sprintf("@type(%q)\n", meta.Type))
	}

	if meta.CacheTTLSeconds != nil {
		sb.WriteString(fmt.Sprintf("@cache(ttl=%d)\n", *meta.CacheTTLSeconds))
	}

	if meta.SkipDeleted != nil && *meta.SkipDeleted {
		sb.WriteString("@skipDeleted\n")
	}

	if meta.EnforceRequired != nil && *meta.EnforceRequired {
		sb.WriteString("@enforceRequired\n")
	}

	if meta.DefaultFilter != "" {
		sb.WriteString(fmt.Sprintf("@defaultFilter(%q)\n", meta.DefaultFilter))
	}

	sb.WriteString(fmt.Sprintf("concept %s {\n", displayName))

	// Build required set
	requiredSet := make(map[string]bool)
	for _, r := range schema.Required {
		requiredSet[r] = true
	}

	// Sort properties for deterministic output
	propNames := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		propNames = append(propNames, name)
	}
	sort.Strings(propNames)

	for _, name := range propNames {
		prop := schema.Properties[name]
		writeProperty(&sb, name, prop, requiredSet[name], "  ")
	}

	// Relationships
	if len(meta.Relationships) > 0 {
		sb.WriteString("\n")
		for _, rel := range meta.Relationships {
			sb.WriteString("  @relationship(")
			parts := []string{}
			if rel.Type != "" {
				parts = append(parts, fmt.Sprintf("type=%q", rel.Type))
			}
			if rel.Field != "" {
				parts = append(parts, fmt.Sprintf("field=%q", rel.Field))
			}
			if rel.TargetConcept != "" {
				parts = append(parts, fmt.Sprintf("target=%q", rel.TargetConcept))
			}
			if rel.Direction != "" {
				parts = append(parts, fmt.Sprintf("direction=%q", rel.Direction))
			}
			sb.WriteString(strings.Join(parts, ", "))
			sb.WriteString(")\n")
		}
	}

	sb.WriteString("}\n")

	return sb.String()
}

func writeProperty(sb *strings.Builder, name string, prop jsonProperty, required bool, indent string) {
	typeStr := jsonTypeToMemQL(prop)

	if typeStr == "object" && len(prop.Properties) > 0 {
		// Nested block
		sb.WriteString(fmt.Sprintf("%s%s {\n", indent, name))
		propNames := make([]string, 0, len(prop.Properties))
		for n := range prop.Properties {
			propNames = append(propNames, n)
		}
		sort.Strings(propNames)
		for _, n := range propNames {
			writeProperty(sb, n, prop.Properties[n], false, indent+"  ")
		}
		sb.WriteString(fmt.Sprintf("%s}\n", indent))
		return
	}

	// Single-line property
	sb.WriteString(fmt.Sprintf("%s%s  %s", indent, name, typeStr))

	// Annotations on same line
	if required {
		sb.WriteString("  @required")
	}
	if prop.Default != nil {
		sb.WriteString(fmt.Sprintf("  @default(%q)", fmt.Sprintf("%v", prop.Default)))
	}
	if prop.Description != "" {
		sb.WriteString(fmt.Sprintf("  @description(%q)", prop.Description))
	}
	sb.WriteString("\n")
}

func jsonTypeToMemQL(prop jsonProperty) string {
	// Enum
	if len(prop.Enum) > 0 {
		vals := make([]string, len(prop.Enum))
		for i, v := range prop.Enum {
			vals[i] = fmt.Sprintf("%q", fmt.Sprintf("%v", v))
		}
		return fmt.Sprintf("enum(%s)", strings.Join(vals, ", "))
	}

	// Array
	typeStr, ok := prop.Type.(string)
	if ok && typeStr == "array" {
		itemType := "string"
		if prop.Items != nil {
			if it, ok := prop.Items.Type.(string); ok {
				itemType = jsonPrimitiveToMemQL(it, prop.Items.Format)
			}
		}
		return fmt.Sprintf("array(%s)", itemType)
	}

	if ok {
		return jsonPrimitiveToMemQL(typeStr, prop.Format)
	}

	return "string"
}

func jsonPrimitiveToMemQL(jsonType, format string) string {
	switch jsonType {
	case "string":
		if format == "date-time" {
			return "datetime"
		}
		return "string"
	case "boolean":
		return "bool"
	case "integer":
		return "int"
	case "number":
		return "float"
	case "object":
		return "object"
	default:
		return "string"
	}
}

// --- Part 2: Add use declarations to .memql files ---

// Concept ID pattern: v1:domain:entity[:sub:entity]
var conceptIdPattern = regexp.MustCompile(`v[0-9]+:[a-z]+(?::[a-z]+)+`)

func addUseDeclarations(root string) error {
	// Build concept ID -> use path mapping from the on-disk concepts/ tree.
	conceptMap, err := buildConceptMap(root)
	if err != nil {
		return fmt.Errorf("build concept map: %w", err)
	}

	// Scan .memql files in mutations/, queries/, automations/
	dirs := []string{
		filepath.Join(root, "dsl", "v1", "mutations"),
		filepath.Join(root, "dsl", "v1", "queries"),
		filepath.Join(root, "dsl", "v1", "automations"),
	}

	fileCount := 0
	for _, dir := range dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !strings.HasSuffix(info.Name(), ".memql") {
				return nil
			}
			modified, mErr := addUsesToFile(path, root, conceptMap)
			if mErr != nil {
				fmt.Fprintf(os.Stderr, "  WARNING: %s: %v\n", path, mErr)
				return nil
			}
			if modified {
				fileCount++
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	fmt.Printf("Added use declarations to %d .memql files\n", fileCount)
	return nil
}

// buildConceptMap walks concepts/**/*/concept.memql and returns a map of
// concept ID -> use path (dots notation without version prefix).
// e.g., concepts/v1/cognition/participant/concept.memql becomes
// "v1:cognition:participant" -> "cognition.participant".
//
// The filesystem scan replaced a hardcoded list so the script works on
// any branch and any concept layout without being edited each time a
// concept is added or moved.
func buildConceptMap(root string) (map[string]string, error) {
	conceptsDir := filepath.Join(root, "dsl", "v1", "concepts")
	m := make(map[string]string)

	err := filepath.WalkDir(conceptsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) != "concept.memql" {
			return nil
		}
		rel, err := filepath.Rel(conceptsDir, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/")
		if len(parts) < 2 {
			return nil
		}
		id := strings.Join(parts, ":")                  // v1:domain:entity(:sub)*
		useName := strings.Join(parts[1:], ".")         // domain.entity.sub
		m[id] = useName
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func addUsesToFile(filePath, root string, conceptMap map[string]string) (bool, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return false, err
	}

	text := string(content)

	// Find all concept IDs referenced in the file
	matches := conceptIdPattern.FindAllString(text, -1)
	if len(matches) == 0 {
		return false, nil
	}

	// Deduplicate
	seen := make(map[string]bool)
	var uniqueIds []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			if _, ok := conceptMap[m]; ok {
				uniqueIds = append(uniqueIds, m)
			}
		}
	}

	if len(uniqueIds) == 0 {
		return false, nil
	}

	sort.Strings(uniqueIds)

	// Check if file already has use declarations
	if strings.Contains(text, "\nuse ") || strings.HasPrefix(text, "use ") {
		fmt.Printf("  SKIP %s (already has use declarations)\n", filePath)
		return false, nil
	}

	// Build use declarations block
	var useBlock strings.Builder
	for _, id := range uniqueIds {
		usePath := conceptMap[id]
		useBlock.WriteString(fmt.Sprintf("use %s\n", usePath))
	}
	useBlock.WriteString("\n")

	// Find insertion point: after any leading comments/blank lines, before first non-use content
	newContent := useBlock.String() + text

	// Now replace concept IDs with leaf names in common positions.
	// We do NOT replace inside event strings like
	// "graph.node.created.*.v1:cognition:participant" because those need the
	// full ID at runtime. The resolver handles on= syntax and emits the
	// canonical 5-segment partition-aware topic form with a `*` partition
	// wildcard; see ConceptResolver.resolveAttribute.
	// For now, just add the use declarations without replacing hardcoded
	// strings. String replacement requires careful parsing context and is
	// better done manually or in a follow-up pass.

	if *dryRun {
		fmt.Printf("  DRY RUN: would add %d use declarations to %s\n", len(uniqueIds), filePath)
		return true, nil
	}

	if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
		return false, err
	}

	fmt.Printf("  UPDATED %s (+%d use declarations)\n", filePath, len(uniqueIds))
	return true, nil
}
