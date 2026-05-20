// audit-import-model reports the import-model refactor's per-construct
// migration status (Commit 2 of dsl-import-model-refactor.md). It walks
// the new domain-first tree (dsl/<domain>/<kind>s.memql) and reports
// per-file how many constructs still use legacy `@useConcept(...)`
// annotations vs how many have been migrated to `import (...)` blocks.
//
// The script is read-only by design. The actual rewriting work lives
// in scripts/add-imports-to-legacy and the manual import-model
// migration. This tool answers "how far along is the migration?"
// without mutating any source.
//
// Output (per kind):
//
//	queries.memql:     migrated=12 legacy=3 mixed=0 (15 constructs total)
//	mutations.memql:   migrated=8  legacy=0 mixed=0 (8  constructs total)
//	specs.memql:       migrated=0  legacy=7 mixed=0 (7  constructs total)
//	...
//
// With --strict, exits non-zero when any legacy-only or mixed counts
// are non-zero. Useful as a CI gate once the migration completes.
//
// Usage:
//
//	go run ./scripts/audit-import-model              # text report
//	go run ./scripts/audit-import-model --json       # JSON output
//	go run ./scripts/audit-import-model --strict     # exit 1 on legacy
//	go run ./scripts/audit-import-model --domain=cognition  # one domain
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

const dslRoot = "dsl"

var (
	useConceptRe        = regexp.MustCompile(`(?m)^@useConcept\(`)
	importBlockRe       = regexp.MustCompile(`(?m)^import[ \t]*\(`)
	importLineRe        = regexp.MustCompile(`(?m)^import[ \t]+["]`)
	structuredTriggerRe = regexp.MustCompile(`@trigger\(\s*event\s*=\s*"node\.(created|updated|deleted)"`)
	legacyTriggerRe     = regexp.MustCompile(`@trigger\(\s*event\s*=\s*"graph\.node\.`)
)

// constructHeader matches every top-level construct declaration so
// we can count how many constructs live in each file.
var constructHeader = regexp.MustCompile(`(?m)^[ \t]*(concept|query|mutation|spec|trait|logic|automation|tool|prompt|provider|builtin|policy|shape|seed)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

type fileReport struct {
	Path               string `json:"path"`
	Domain             string `json:"domain"`
	Kind               string `json:"kind"`
	Constructs         int    `json:"constructs"`
	UseConceptCount    int    `json:"use_concept_count"`
	HasImportBlock     bool   `json:"has_import_block"`
	LegacyTriggers     int    `json:"legacy_triggers"`
	StructuredTriggers int    `json:"structured_triggers"`
	State              string `json:"state"` // "migrated" | "legacy" | "mixed" | "n/a"
}

type domainReport struct {
	Domain   string       `json:"domain"`
	Files    []fileReport `json:"files"`
	Migrated int          `json:"migrated"`
	Legacy   int          `json:"legacy"`
	Mixed    int          `json:"mixed"`
}

type report struct {
	Domains []domainReport `json:"domains"`
	Totals  struct {
		Migrated int `json:"migrated"`
		Legacy   int `json:"legacy"`
		Mixed    int `json:"mixed"`
	} `json:"totals"`
}

func main() {
	jsonOut := flag.Bool("json", false, "emit JSON instead of text")
	strict := flag.Bool("strict", false, "exit 1 when any file is legacy or mixed")
	domainFlag := flag.String("domain", "", "restrict to one domain (e.g. cognition)")
	flag.Parse()

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	rep, err := buildReport(filepath.Join(root, dslRoot), *domainFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printText(rep)
	}

	if *strict && (rep.Totals.Legacy > 0 || rep.Totals.Mixed > 0) {
		os.Exit(1)
	}
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
			return "", fmt.Errorf("could not find repo root (no go.mod above %s)", dir)
		}
		dir = parent
	}
}

func buildReport(dslPath string, onlyDomain string) (*report, error) {
	entries, err := os.ReadDir(dslPath)
	if err != nil {
		return nil, fmt.Errorf("read dsl/: %w", err)
	}

	rep := &report{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		domain := entry.Name()
		if strings.HasPrefix(domain, "_") || strings.HasPrefix(domain, ".") {
			continue
		}
		if domain == "v1" {
			// Legacy tree is reported separately by
			// audit-legacy-migration.
			continue
		}
		if onlyDomain != "" && domain != onlyDomain {
			continue
		}

		dr, err := buildDomainReport(filepath.Join(dslPath, domain), domain)
		if err != nil {
			return nil, fmt.Errorf("domain %s: %w", domain, err)
		}
		if dr == nil || len(dr.Files) == 0 {
			continue
		}
		rep.Domains = append(rep.Domains, *dr)
		rep.Totals.Migrated += dr.Migrated
		rep.Totals.Legacy += dr.Legacy
		rep.Totals.Mixed += dr.Mixed
	}

	sort.Slice(rep.Domains, func(i, j int) bool {
		return rep.Domains[i].Domain < rep.Domains[j].Domain
	})
	return rep, nil
}

func buildDomainReport(domainPath, domain string) (*domainReport, error) {
	entries, err := os.ReadDir(domainPath)
	if err != nil {
		return nil, err
	}
	dr := &domainReport{Domain: domain}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".memql") {
			continue
		}
		if strings.HasPrefix(name, "_") {
			continue
		}
		fr, err := analyzeFile(filepath.Join(domainPath, name), domain)
		if err != nil {
			return nil, err
		}
		dr.Files = append(dr.Files, fr)
		switch fr.State {
		case "migrated":
			dr.Migrated++
		case "legacy":
			dr.Legacy++
		case "mixed":
			dr.Mixed++
		}
	}
	sort.Slice(dr.Files, func(i, j int) bool {
		return dr.Files[i].Path < dr.Files[j].Path
	})
	return dr, nil
}

func analyzeFile(path, domain string) (fileReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fileReport{}, err
	}
	source := string(data)
	fr := fileReport{
		Path:               path,
		Domain:             domain,
		Kind:               kindFromFilename(filepath.Base(path)),
		Constructs:         countConstructs(source),
		UseConceptCount:    len(useConceptRe.FindAllStringIndex(source, -1)),
		HasImportBlock:     importBlockRe.MatchString(source) || importLineRe.MatchString(source),
		LegacyTriggers:     len(legacyTriggerRe.FindAllStringIndex(source, -1)),
		StructuredTriggers: len(structuredTriggerRe.FindAllStringIndex(source, -1)),
	}
	fr.State = classify(fr)
	return fr, nil
}

func kindFromFilename(name string) string {
	base := strings.TrimSuffix(name, ".memql")
	return strings.TrimSuffix(base, "s")
}

func countConstructs(source string) int {
	return len(constructHeader.FindAllStringIndex(source, -1))
}

func classify(fr fileReport) string {
	if fr.Constructs == 0 {
		return "n/a"
	}
	switch {
	case fr.HasImportBlock && fr.UseConceptCount == 0 && fr.LegacyTriggers == 0:
		return "migrated"
	case !fr.HasImportBlock && fr.UseConceptCount > 0:
		return "legacy"
	case fr.HasImportBlock && (fr.UseConceptCount > 0 || fr.LegacyTriggers > 0):
		return "mixed"
	default:
		// No bindings, no imports -- treat as migrated (e.g. logic /
		// automation files that don't need a concept binding).
		return "migrated"
	}
}

func printText(rep *report) {
	for _, dr := range rep.Domains {
		fmt.Printf("=== %s ===\n", dr.Domain)
		for _, fr := range dr.Files {
			fmt.Printf("  %-60s %-9s constructs=%-3d use_concept=%-3d import_block=%-5v legacy_trig=%-3d struct_trig=%-3d\n",
				fr.Path, "["+fr.State+"]", fr.Constructs,
				fr.UseConceptCount, fr.HasImportBlock,
				fr.LegacyTriggers, fr.StructuredTriggers)
		}
		fmt.Printf("  -> migrated=%d legacy=%d mixed=%d\n\n", dr.Migrated, dr.Legacy, dr.Mixed)
	}
	fmt.Printf("=== TOTAL ===\n")
	fmt.Printf("  migrated=%d  legacy=%d  mixed=%d\n",
		rep.Totals.Migrated, rep.Totals.Legacy, rep.Totals.Mixed)
}
