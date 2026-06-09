// docs-gen emits the machine-generated reference for the documentation
// bundle: a concept catalog derived from the live DSL. It loads the
// embedded DSL tree (DB-free, via memql.LoadUnifiedConcepts) and walks the
// concept registry, so the catalog can never drift from the engine it was
// built against.
//
// Usage:
//
//	go run ./cmd/docs-gen [-out docs/public/reference/_generated]
//
// Output: <out>/concepts.md (front-matter: audience: public). The bundle
// builder (scripts/docs/build-docs-bundle.sh) runs this before selecting the
// public docs, so the generated catalog ships in every release snapshot.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// jsonSchema is the slice of a concept's "definition" JSON Schema we render.
type jsonSchema struct {
	Properties map[string]schemaProp `json:"properties"`
	Required   []string              `json:"required"`
}

type schemaProp struct {
	Type        any    `json:"type"` // JSON Schema type: string or []string
	Description string `json:"description"`
	Enum        []any  `json:"enum"`
}

func typeString(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " \\| ")
	}
	return ""
}

func cell(s string) string {
	// keep table cells single-line + pipe-safe
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// renderConceptCatalog renders the full concept catalog markdown for the
// given concepts (assumed already loaded). Separated from main for testing.
func renderConceptCatalog(concepts []*memoryNodes.Concept) string {
	sorted := make([]*memoryNodes.Concept, len(concepts))
	copy(sorted, concepts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("title: Concept Catalog\n")
	b.WriteString("audience: public\n")
	b.WriteString("status: stable\n")
	b.WriteString("area: language\n")
	b.WriteString("sinceVersion: 0.9.0\n")
	b.WriteString("owner: znas\n")
	b.WriteString("---\n\n")
	b.WriteString("# Concept Catalog\n\n")
	b.WriteString("Generated from the live DSL by `cmd/docs-gen` -- do not hand-edit.\n")
	b.WriteString("A memQL node is an instance of one of these concepts; each concept's\n")
	b.WriteString("fields below are its schema.\n\n")
	fmt.Fprintf(&b, "Total: **%d** concepts.\n\n", len(sorted))

	for _, c := range sorted {
		fmt.Fprintf(&b, "## `%s`\n\n", c.Name)
		if c.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(c.Description))
		}
		if def, ok := c.Schemas["definition"]; ok {
			var s jsonSchema
			if err := json.Unmarshal(def, &s); err == nil && len(s.Properties) > 0 {
				req := make(map[string]bool, len(s.Required))
				for _, r := range s.Required {
					req[r] = true
				}
				names := make([]string, 0, len(s.Properties))
				for k := range s.Properties {
					names = append(names, k)
				}
				sort.Strings(names)
				b.WriteString("| Field | Type | Required | Description |\n|---|---|---|---|\n")
				for _, name := range names {
					p := s.Properties[name]
					required := ""
					if req[name] {
						required = "yes"
					}
					desc := p.Description
					if len(p.Enum) > 0 {
						vals := make([]string, 0, len(p.Enum))
						for _, e := range p.Enum {
							vals = append(vals, fmt.Sprintf("%v", e))
						}
						if desc != "" {
							desc += " "
						}
						desc += "(enum: " + strings.Join(vals, ", ") + ")"
					}
					fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", name, typeString(p.Type), required, cell(desc))
				}
				b.WriteString("\n")
			}
		}
		if len(c.Relationships) > 0 {
			rels := make([]string, 0, len(c.Relationships))
			for _, r := range c.Relationships {
				if r.TargetConcept != "" {
					rels = append(rels, fmt.Sprintf("`%s` -> `%s`", r.Type, r.TargetConcept))
				}
			}
			if len(rels) > 0 {
				fmt.Fprintf(&b, "**Relationships:** %s\n\n", strings.Join(rels, ", "))
			}
		}
	}
	return b.String()
}

func main() {
	out := flag.String("out", filepath.Join("docs", "public", "reference", "_generated"), "output directory")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	n, err := memql.LoadUnifiedConcepts(logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docs-gen: load concepts:", err)
		os.Exit(1)
	}
	concepts := memoryNodes.List()
	if len(concepts) == 0 {
		fmt.Fprintln(os.Stderr, "docs-gen: no concepts loaded (registry empty)")
		os.Exit(1)
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "docs-gen:", err)
		os.Exit(1)
	}
	dest := filepath.Join(*out, "concepts.md")
	if err := os.WriteFile(dest, []byte(renderConceptCatalog(concepts)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "docs-gen:", err)
		os.Exit(1)
	}
	fmt.Printf("docs-gen: wrote %s (%d concepts; loader reported %d)\n", dest, len(concepts), n)
}
