// audit-pagination enumerates every list-returning query in the DSL
// tree and reports how each is bounded -- the pagination authoring rule
// audit (epic 5, issue 5.1 / memql#1965).
//
// It walks the on-disk tree (dsl/<domain>/queries.memql) and classifies
// every struct-form query via the shared, deterministic classifier in
// component/language/pagination -- the SAME rule the conformance gate
// uses, so the audit and the gate can never drift. For each query it
// reports one of:
//
//	single-row       filtered by `id ==` -- reads at most one row (exempt)
//	aggregate        carries `count` -- returns a number, not rows (exempt)
//	bounded-list     declares `paginate` / `sort` (compliant)
//	unbounded-marked carries `@unbounded("reason")` (compliant, auditable)
//	unmarked-list    none of the above -- the pagination-rule VIOLATION
//
// The UNMARKED-LIST set is the work item for issue 5.3 (the backfill):
// every entry there needs `paginate`/`sort` or `@unbounded("reason")`.
// The UNBOUNDED-MARKED set is the legitimate full-set reads, listed with
// their reasons so they stay auditable.
//
// Read-only by design -- it mutates nothing.
//
// Usage:
//
//	go run ./scripts/audit-pagination                 # full text report
//	go run ./scripts/audit-pagination --json          # JSON output
//	go run ./scripts/audit-pagination --unmarked      # only unmarked-list rows
//	go run ./scripts/audit-pagination --unbounded     # only @unbounded rows + reasons
//	go run ./scripts/audit-pagination --strict        # exit 1 if any unmarked-list
//	go run ./scripts/audit-pagination --domain=cognition
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/pagination"
)

const dslRoot = "dsl"

func main() {
	var (
		jsonOut       = flag.Bool("json", false, "emit JSON instead of the text report")
		onlyUnmarked  = flag.Bool("unmarked", false, "list only unmarked-list queries (the 5.3 backfill set)")
		onlyUnbounded = flag.Bool("unbounded", false, "list only @unbounded queries + their reasons")
		strict        = flag.Bool("strict", false, "exit 1 when any unmarked-list query is found")
		domain        = flag.String("domain", "", "restrict to a single domain (e.g. cognition)")
	)
	flag.Parse()

	findings, err := scanTree(*domain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit-pagination: %v\n", err)
		os.Exit(2)
	}
	pagination.SortFindings(findings)

	if *jsonOut {
		emitJSON(findings)
	} else {
		emitText(findings, *onlyUnmarked, *onlyUnbounded)
	}

	if *strict && countClass(findings, pagination.UnmarkedList) > 0 {
		os.Exit(1)
	}
}

// scanTree walks dsl/<domain>/queries.memql files and classifies every
// query. Skips the _reference/ skeletons (docs, not loaded).
func scanTree(domainFilter string) ([]pagination.QueryFinding, error) {
	var findings []pagination.QueryFinding
	err := filepath.WalkDir(dslRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "_reference" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Base(path) != "queries.memql" {
			return nil
		}
		rel, _ := filepath.Rel(dslRoot, path)
		if domainFilter != "" {
			dom := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
			if dom != domainFilter {
				return nil
			}
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		findings = append(findings, pagination.ScanSource(filepath.ToSlash(rel), string(raw))...)
		return nil
	})
	return findings, err
}

func countClass(findings []pagination.QueryFinding, c pagination.Classification) int {
	n := 0
	for _, f := range findings {
		if f.Class == c {
			n++
		}
	}
	return n
}

func emitText(findings []pagination.QueryFinding, onlyUnmarked, onlyUnbounded bool) {
	total := len(findings)
	byClass := map[pagination.Classification]int{}
	for _, f := range findings {
		byClass[f.Class]++
	}

	if !onlyUnmarked && !onlyUnbounded {
		fmt.Println("=== Pagination audit (memql#1965) ===")
		fmt.Printf("%d queries scanned\n", total)
		order := []pagination.Classification{
			pagination.SingleRow, pagination.Aggregate, pagination.BoundedList,
			pagination.UnboundedMarked, pagination.UnmarkedList,
		}
		for _, c := range order {
			fmt.Printf("  %-16s %4d\n", c.String(), byClass[c])
		}
		fmt.Println()
	}

	if !onlyUnbounded {
		fmt.Printf("--- UNMARKED LIST queries (%d) -- the issue 5.3 backfill set ---\n", byClass[pagination.UnmarkedList])
		for _, f := range findings {
			if f.Class == pagination.UnmarkedList {
				fmt.Printf("  %s:%d  %s (concept %s)\n", f.File, f.Line, f.Name, f.Concept)
			}
		}
		fmt.Println()
	}

	if !onlyUnmarked {
		fmt.Printf("--- @unbounded queries (%d) -- legitimate full-set reads ---\n", byClass[pagination.UnboundedMarked])
		for _, f := range findings {
			if f.Class == pagination.UnboundedMarked {
				fmt.Printf("  %s:%d  %s -- %q\n", f.File, f.Line, f.Name, f.UnboundedReason)
			}
		}
	}
}

func emitJSON(findings []pagination.QueryFinding) {
	type row struct {
		File            string `json:"file"`
		Line            int    `json:"line"`
		Name            string `json:"name"`
		Concept         string `json:"concept"`
		Class           string `json:"class"`
		UnboundedReason string `json:"unboundedReason,omitempty"`
	}
	out := struct {
		Total   int            `json:"total"`
		Summary map[string]int `json:"summary"`
		Queries []row          `json:"queries"`
	}{
		Total:   len(findings),
		Summary: map[string]int{},
	}
	for _, f := range findings {
		out.Summary[f.Class.String()]++
		out.Queries = append(out.Queries, row{
			File: f.File, Line: f.Line, Name: f.Name, Concept: f.Concept,
			Class: f.Class.String(), UnboundedReason: f.UnboundedReason,
		})
	}
	sort.Slice(out.Queries, func(i, j int) bool {
		if out.Queries[i].File != out.Queries[j].File {
			return out.Queries[i].File < out.Queries[j].File
		}
		return out.Queries[i].Line < out.Queries[j].Line
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
