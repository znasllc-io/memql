package compose

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	pure "github.com/znasllc-io/memql/component/compose"
)

// sources.go -- resolving what a composition is made FROM.
//
// EVERY READ HERE RUNS UNDER THE CALLER'S OWN ACTOR, unstamped, so this
// can never return a row the caller could not have read themselves. That
// is what makes `composeResolveSources` safe to expose to a browser as a
// live count: the count is of rows they can see, and an id somebody
// pastes buys them nothing.
//
// The three kinds are NOT interchangeable and the design record says why
// (section C): "made from the 4 invoices matching status=open" and "made
// from these 4 invoice ids" are different claims about repeatability, and
// only the first survives next quarter.

// SourceKind discriminates the three things a composition can be made
// from.
type SourceKind string

const (
	// KindConceptRow names ONE row by its bare id.
	KindConceptRow SourceKind = "concept_row"
	// KindLibraryFile names ONE v1:library:file by its bare id.
	KindLibraryFile SourceKind = "library_file"
	// KindQuery names a SELECTION -- a rendered call string resolved at
	// run time. This is the kind that makes a recipe worth having.
	KindQuery SourceKind = "query"
)

// SourceRef is one entry of the caller's `sources` list.
type SourceRef struct {
	Kind  SourceKind `json:"kind"`
	Ref   string     `json:"ref"`
	Label string     `json:"label"`
}

// Resolved is what a SourceRef found.
type Resolved struct {
	Ref SourceRef
	// Rows is what the source yielded. A concept_row and a library_file
	// yield at most one; a query yields whatever the caller can read.
	Rows []map[string]any
	// Count is len(Rows), carried separately because the app renders it
	// without needing the rows.
	Count int
	// Problem is why this source found nothing, in words a person can
	// act on. EMPTY AND ZERO ROWS ARE DIFFERENT ANSWERS from a problem
	// and zero rows: "the filter matched nothing" and "you cannot read
	// that" are different situations and the app draws them differently.
	Problem string
	// CapturedAt is when it was resolved.
	CapturedAt time.Time
}

// parseSourceRefs decodes the caller's `sources` argument.
//
// AN UNKNOWN KIND IS AN ERROR, not a skip. A silently-dropped source
// produces a composition made from less than the person asked for, and
// nothing on the page would say so -- the output would simply be thinner
// than expected, which is the hardest kind of wrong to notice.
func parseSourceRefs(raw any) ([]SourceRef, error) {
	items, ok := raw.([]any)
	if !ok {
		if items2, ok2 := raw.([]map[string]any); ok2 {
			items = make([]any, 0, len(items2))
			for _, m := range items2 {
				items = append(items, m)
			}
		} else if raw == nil {
			return nil, nil
		} else {
			return nil, fmt.Errorf("compose: sources must be a list of {kind, ref, label}")
		}
	}
	out := make([]SourceRef, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("compose: sources[%d] is not an object", idx)
		}
		ref := SourceRef{
			Kind:  SourceKind(strings.TrimSpace(stringOf(m["kind"]))),
			Ref:   strings.TrimSpace(stringOf(m["ref"])),
			Label: strings.TrimSpace(stringOf(m["label"])),
		}
		switch ref.Kind {
		case KindConceptRow, KindLibraryFile, KindQuery:
		case "":
			return nil, fmt.Errorf("compose: sources[%d] declares no kind (expected concept_row, library_file or query)", idx)
		default:
			return nil, fmt.Errorf("compose: sources[%d] declares kind %q (expected concept_row, library_file or query)", idx, ref.Kind)
		}
		if ref.Ref == "" {
			return nil, fmt.Errorf("compose: sources[%d] (%s) names nothing", idx, ref.Kind)
		}
		out = append(out, ref)
	}
	return out, nil
}

// resolve reads every source under the CALLER's actor.
//
// It never returns an error for a source that found nothing: a source
// the caller cannot read and a filter that matched nothing are both
// answers, and stopping the whole materialization because one of five
// sources came back empty would make a composition impossible to
// assemble incrementally. The PROBLEM is recorded per source and the
// caller decides.
func (i *Integration) resolve(ctx context.Context, refs []SourceRef) ([]Resolved, error) {
	st := i.store()
	now := i.clock().UTC()
	out := make([]Resolved, 0, len(refs))
	for _, ref := range refs {
		res := Resolved{Ref: ref, CapturedAt: now}
		switch ref.Kind {
		case KindLibraryFile:
			row, err := st.libraryFileById(ctx, ref.Ref)
			if err != nil {
				res.Problem = err.Error()
			} else if row == nil {
				res.Problem = "no file with that id is readable by you"
			} else {
				res.Rows = []map[string]any{row}
			}
		case KindConceptRow:
			// A concept_row's ref is "<conceptId>#<bareRowId>". The
			// CONCEPT half names the read: MemQL has no generic row
			// read, so the concept's own @composable(list=...) supplies
			// it, and a concept that declared none is one this source
			// kind cannot reach -- which the error SAYS, rather than
			// reporting an empty result that reads as "the row is gone".
			conceptId, rowId, ok := splitConceptRef(ref.Ref)
			if !ok {
				res.Problem = "a concept row is named as <concept>#<id>"
				break
			}
			list := i.listQueryFor(conceptId)
			if list == "" {
				res.Problem = fmt.Sprintf("%s declares no @composable(list=...), so this cluster has no read for a row of it -- name a query source instead", conceptId)
				break
			}
			rows, err := st.query(ctx, "query "+list+"()")
			if err != nil {
				res.Problem = err.Error()
				break
			}
			match := rowWithId(rows, rowId)
			switch {
			case match != nil:
				res.Rows = []map[string]any{match}
			case len(rows) == 0:
				res.Problem = "no rows of that concept are readable by you"
			default:
				// THE PAGE BOUND IS NAMED, because the honest answer
				// here is ambiguous: the row may be gone, or it may
				// simply be past the end of one page. Saying "not
				// found" would assert the first, and somebody would go
				// looking for a deleted row that is fine.
				res.Problem = fmt.Sprintf("that row is not in the first page of %s -- it may have aged out of the page rather than been removed; name a query source to reach it precisely", list)
			}
		case KindQuery:
			rows, err := st.query(ctx, ensureQueryPrefix(ref.Ref))
			if err != nil {
				res.Problem = err.Error()
			} else {
				res.Rows = rows
				if len(rows) == 0 {
					// A query that matched nothing is NOT a problem --
					// it is an answer, and one somebody wants to see
					// before they spend a model call finding it out.
					res.Problem = ""
				}
			}
		}
		res.Count = len(res.Rows)
		out = append(out, res)
	}
	return out, nil
}

// ensureQueryPrefix lets a caller pass either `query foo(...)` or
// `foo(...)`. THE PREFIX IS ADDED, NEVER THE VERB CHANGED: a `mutate`
// arriving here is passed through as written and refused by the engine
// as a construct the caller may not reach, rather than being silently
// rewritten into a read that succeeds. A source that quietly became a
// different statement is the one failure this helper must not have.
func ensureQueryPrefix(s string) string {
	trimmed := strings.TrimSpace(s)
	lower := strings.ToLower(trimmed)
	for _, verb := range []string{"query ", "mutate ", "logic ", "shape "} {
		if strings.HasPrefix(lower, verb) {
			return trimmed
		}
	}
	return "query " + trimmed
}

// pureSources projects the resolved set onto the provenance record's own
// shape. CapturedAt travels, because it is what makes the difference
// between two runs of one recipe explainable.
func pureSources(resolved []Resolved) []pure.Source {
	out := make([]pure.Source, 0, len(resolved))
	for _, r := range resolved {
		out = append(out, pure.Source{
			Kind:       string(r.Ref.Kind),
			Ref:        r.Ref.Ref,
			Label:      r.Ref.Label,
			CapturedAt: r.CapturedAt,
		})
	}
	return out
}

// rowSources projects the resolved set onto the stored `sources` field.
func rowSources(resolved []Resolved) []map[string]any {
	out := make([]map[string]any, 0, len(resolved))
	for _, r := range resolved {
		out = append(out, map[string]any{
			"kind":       string(r.Ref.Kind),
			"ref":        r.Ref.Ref,
			"label":      r.Ref.Label,
			"capturedAt": r.CapturedAt.Format(time.RFC3339),
		})
	}
	return out
}

// tabularRows flattens every resolved source into the row list the csv
// and json writers take.
//
// THE COLUMN ORDER IS DERIVED AND SORTED, for the reason
// component/compose's own header fallback is: Go randomises map
// iteration, and a CSV whose columns move between two runs of one recipe
// is one no downstream tool can read.
func tabularRows(resolved []Resolved) ([]string, []map[string]any) {
	var rows []map[string]any
	seen := map[string]bool{}
	var header []string
	for _, r := range resolved {
		for _, row := range r.Rows {
			rows = append(rows, row)
			for k := range row {
				if !seen[k] {
					seen[k] = true
					header = append(header, k)
				}
			}
		}
	}
	sort.Strings(header)
	return header, rows
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// splitConceptRef parses "<conceptId>#<bareRowId>".
//
// THE SEPARATOR IS '#' RATHER THAN ':' because a concept id is itself
// colon-delimited ("v1:library:file"), so a colon-joined pair has no
// unambiguous split point -- and the identifiers contract already warns
// against clients composing or parsing canonical ids. '#' appears in
// neither half.
func splitConceptRef(ref string) (conceptId, rowId string, ok bool) {
	conceptId, rowId, found := strings.Cut(strings.TrimSpace(ref), "#")
	conceptId = strings.TrimSpace(conceptId)
	rowId = strings.TrimSpace(rowId)
	if !found || conceptId == "" || rowId == "" {
		return "", "", false
	}
	return conceptId, rowId, true
}

// rowWithId finds a row by its BARE id.
//
// It compares the tail after the last colon on BOTH sides, because a
// query's rows carry canonical ids on the wire while a client sends
// bare ones -- the engine bare-ifies on egress and resolves bare args
// inbound, so a source stored from a browser and a row read here can
// legitimately differ in spelling for the same row.
func rowWithId(rows []map[string]any, wantId string) map[string]any {
	want := bareId(wantId)
	if want == "" {
		return nil
	}
	for _, row := range rows {
		if bareId(stringOf(row["id"])) == want {
			return row
		}
	}
	return nil
}

func bareId(id string) string {
	trimmed := strings.TrimSpace(id)
	if i := strings.LastIndex(trimmed, ":"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}
