package dsl

import (
	"io"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// TestFilterSyntaxCanonical asserts that every filter clause in the
// tree references payload fields via `payload.X` (or `?.payload.X`
// for arg-conditional predicates), never via `<conceptName>.X` or
// `?.<conceptName>.X`.
//
// Background: prior to the 2026-05 cleanup, filter clauses mixed
// five syntactic forms for the same operation -- payload.X,
// <conceptName>.X, ?.<conceptName>.X, ?.X, and trait/spec calls.
// The decision recorded in feature/dsl-improvements: payload.X is
// the only legal prefix for payload fields; intrinsics (id, concept,
// createdAt, createdBy, partition, type, schema) stay bare; the ?.
// prefix is preserved wherever it carries arg-conditional semantics
// but only over payload.X or a bare intrinsic, never over a
// concept-name alias.
//
// This test parses each .memql file line-structure-only (no full
// parser), extracts filter clauses, and rejects any predicate whose
// LHS starts with `<conceptName>.` or `?.<conceptName>.` where
// <conceptName> is not "payload" / "args" / "actor" / "caller" or
// one of the row intrinsics.
func TestFilterSyntaxCanonical(t *testing.T) {
	intrinsics := map[string]bool{
		"id": true, "concept": true, "createdAt": true,
		"createdBy": true, "partition": true, "type": true,
		"schema": true, "payload": true,
		// reserved engine-side names that may appear bare on the LHS
		"args": true, "actor": true, "caller": true, "now": true,
		"config": true, "trace": true,
	}

	type violation struct {
		file string
		line int
		text string
	}
	var violations []violation

	visitFilterPredicates(t, func(file string, lineno int, pred string) {
		// ?.<head>(.<rest>)? or <head>(.<rest>)?
		head, _ := splitFilterRef(pred)
		if head == "" {
			return
		}
		if intrinsics[head] {
			return
		}
		// Heads like "traitIsActiveRecord" (no `.` after) are spec
		// calls, not field refs. Only flag if the predicate has a
		// `.` after the head (so it's <head>.<field>) or an operator
		// that proves it's a comparison.
		if !strings.Contains(pred, ".") && !hasFilterOperator(pred) {
			return
		}
		violations = append(violations, violation{file, lineno, pred})
	})

	if len(violations) > 0 {
		t.Errorf("found %d filter predicates using non-canonical prefix (must be payload.X or bare intrinsic):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s", v.file, v.line, v.text)
		}
	}
}

// TestNoInlineTraitablePredicates asserts that no filter clause
// inlines a payload comparison that an existing trait spec covers.
// Today's traits in dsl/common/traits.memql:
//
//	traitIsActiveRecord  ⇔ payload.active == true
//	traitIsNotDeleted    ⇔ payload.deleted != true / payload.deleted == false
//	traitIsArchived      ⇔ payload.archived == true / payload.archivedAt != null
//	traitIsSaved         ⇔ payload.saved == true
//
// Authors must call the trait, not inline the comparison, so the
// definition of "active" / "deleted" / etc. lives in one place.
// Concept-specific predicates (payload.ownerUserId==args.userId)
// remain legal inline -- only the traitable predicates are
// rejected here.
func TestNoInlineTraitablePredicates(t *testing.T) {
	// Each pattern: matcher regex + suggested trait
	type rule struct {
		re   *regexp.Regexp
		hint string
	}
	rules := []rule{
		{regexp.MustCompile(`payload\.active\s*==\s*true\b`), "traitIsActiveRecord"},
		{regexp.MustCompile(`payload\.active\s*!=\s*false\b`), "traitIsActiveRecord"},
		{regexp.MustCompile(`payload\.deleted\s*==\s*false\b`), "traitIsNotDeleted"},
		{regexp.MustCompile(`payload\.deleted\s*!=\s*true\b`), "traitIsNotDeleted"},
	}

	type violation struct {
		file string
		line int
		text string
		hint string
	}
	var violations []violation

	visitFilterPredicates(t, func(file string, lineno int, pred string) {
		for _, r := range rules {
			if r.re.MatchString(pred) {
				violations = append(violations, violation{file, lineno, pred, r.hint})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("found %d filter predicates that should use a trait spec:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s   → use %s", v.file, v.line, v.text, v.hint)
		}
	}
}

// visitFilterPredicates walks every .memql file in the unified tree,
// extracts filter-clause lines, splits on `;`, and invokes f for
// each predicate. Files under _reference/ are skipped -- they are
// documentation, not loaded.
func visitFilterPredicates(t *testing.T, f func(file string, lineno int, pred string)) {
	t.Helper()
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		walkFilterPredicates(p, string(raw), f)
	}
}

// walkFilterPredicates scans src line-by-line. When it sees a line
// beginning with `filter ` it enters "in filter clause" mode and
// emits each `;`-separated predicate on the line. Subsequent
// indented continuation lines are treated as more predicates. The
// clause ends when it hits `shape`, `insert`, `update`, `return`,
// `concept`, `args`, `use`, `@<annotation>`, a closing brace, or a
// blank line.
func walkFilterPredicates(path, src string, emit func(file string, lineno int, pred string)) {
	inFilter := false
	for lineno, raw := range strings.Split(src, "\n") {
		line := raw
		// strip // comments (best-effort; don't try to handle strings)
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		trim := strings.TrimSpace(line)
		if trim == "" {
			inFilter = false
			continue
		}
		if strings.HasPrefix(trim, "filter ") || strings.HasPrefix(trim, "filter\t") {
			inFilter = true
			rest := strings.TrimSpace(strings.TrimPrefix(trim, "filter"))
			for _, p := range splitPredicates(rest) {
				if p != "" {
					emit(path, lineno+1, p)
				}
			}
			continue
		}
		if !inFilter {
			continue
		}
		// End markers
		end := false
		for _, kw := range []string{"shape", "insert", "update", "return", "concept", "args", "use", "}", ")"} {
			if strings.HasPrefix(trim, kw+" ") || trim == kw || strings.HasPrefix(trim, kw+"\t") {
				end = true
				break
			}
		}
		if strings.HasPrefix(trim, "@") {
			end = true
		}
		if end {
			inFilter = false
			continue
		}
		for _, p := range splitPredicates(trim) {
			if p != "" {
				emit(path, lineno+1, p)
			}
		}
	}
}

func splitPredicates(s string) []string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// splitFilterRef peels the leading identifier (and optional `?.`
// prefix) off a predicate. Returns (head, rest). For
// `?.user.role==args.role` returns ("user", ".role==args.role").
// For `traitIsActiveRecord` returns ("traitIsActiveRecord", "").
// For `id==args.userId` returns ("id", "==args.userId").
func splitFilterRef(pred string) (string, string) {
	s := pred
	if strings.HasPrefix(s, "?.") {
		s = s[2:]
	}
	end := 0
	for end < len(s) {
		c := s[end]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9' && end > 0)) {
			break
		}
		end++
	}
	if end == 0 {
		return "", ""
	}
	return s[:end], s[end:]
}

func hasFilterOperator(pred string) bool {
	for _, op := range []string{"==", "!=", "<=", ">=", " has ", " in ", " not "} {
		if strings.Contains(pred, op) {
			return true
		}
	}
	for _, c := range pred {
		if c == '<' || c == '>' {
			return true
		}
	}
	return false
}

// Compile-time guarantee that fs is referenced.
var _ fs.FS = (fs.FS)(nil)
