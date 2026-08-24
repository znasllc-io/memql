// Package main -- emit_graphql.go: the documents the connector sends.
//
// Two shapes, from one field list. The FETCH document reads one object by
// GID and is what a webhook triggers (design D2: the payload is a signal, the
// object is fetched). The BULK document reads a whole domain and is what a
// backfill runs (design D3). They agree on every field because they are
// rendered from the same plan -- which is the property that makes "one apply
// path for live, backfill and reconciliation" true rather than aspirational.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// maxBulkConnections is Shopify's ceiling on connections in one bulk query.
// A type with more child connections than this is split across several
// operations, each re-listing the root with identity fields plus its own
// batch -- which is also why the runner has to apply them in order.
const maxBulkConnections = 5

// PlanSet indexes plans so an emitter can resolve a child by type.
type PlanSet struct {
	byConcept map[string]*TypePlan
	byType    map[string]*TypePlan
	order     []*TypePlan
}

// NewPlanSet indexes the plans.
func NewPlanSet(plans []*TypePlan) *PlanSet {
	ps := &PlanSet{
		byConcept: map[string]*TypePlan{},
		byType:    map[string]*TypePlan{},
		order:     plans,
	}
	for _, p := range plans {
		ps.byConcept[p.Concept] = p
		ps.byType[p.GraphQLType] = p
	}
	return ps
}

// ByType resolves a plan by GraphQL type name.
func (ps *PlanSet) ByType(name string) *TypePlan { return ps.byType[name] }

// All returns the plans in concept order.
func (ps *PlanSet) All() []*TypePlan { return ps.order }

// ParentsFirst orders concepts so a parent is always applied before its
// children. A child row carries parentGid, and a backfill that wrote line
// items before their order would leave a window in which the mirror answers
// "this line belongs to nothing".
func (ps *PlanSet) ParentsFirst() []*TypePlan {
	var out []*TypePlan
	placed := map[string]bool{}
	var place func(p *TypePlan)
	place = func(p *TypePlan) {
		if placed[p.Concept] {
			return
		}
		if p.ParentType != "" {
			if parent := ps.byType[p.ParentType]; parent != nil && parent != p {
				place(parent)
			}
		}
		placed[p.Concept] = true
		out = append(out, p)
	}
	sorted := append([]*TypePlan(nil), ps.order...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Concept < sorted[j].Concept })
	for _, p := range sorted {
		place(p)
	}
	return out
}

// selectionLines renders the field selections for one type. `nested` renders
// the child connections too; a child rendered inside its parent does not
// recurse further, which is what keeps a fetch to two levels.
func (ps *PlanSet) selectionLines(p *TypePlan, indent string, nested bool, bulk bool) []string {
	var lines []string
	lines = append(lines, indent+"id")
	for _, f := range p.Fields {
		sel := f.Selection
		if sel == "" {
			sel = f.GraphQL
		}
		if bulk {
			switch {
			case f.Kind == KindRefs:
				// Omitted from bulk: see the note on the ref mapping.
				continue
			case f.SelectionBulk != "":
				// A bulk operation refuses pagination arguments on a
				// nested connection and streams it through edges/node.
				sel = f.SelectionBulk
			}
		}
		lines = append(lines, indent+sel)
	}
	if !nested {
		return lines
	}
	for _, ch := range p.Children {
		child := ps.byType[ch.Type]
		if child == nil {
			continue
		}
		if ch.List {
			// A plain LIST child needs no connection wrapper and takes no
			// pagination argument in either document form.
			inner := ps.selectionLines(child, indent+"  ", false, bulk)
			lines = append(lines, fmt.Sprintf("%s%s {", indent, ch.Connection))
			lines = append(lines, inner...)
			lines = append(lines, indent+"}")
			continue
		}
		inner := ps.selectionLines(child, indent+"    ", false, bulk)
		if bulk {
			lines = append(lines, fmt.Sprintf("%s%s {", indent, ch.Connection))
			lines = append(lines, indent+"  edges {")
			lines = append(lines, indent+"    node {")
			lines = append(lines, inner...)
			lines = append(lines, indent+"    }")
			lines = append(lines, indent+"  }")
			lines = append(lines, indent+"}")
			continue
		}
		lines = append(lines, fmt.Sprintf("%s%s(first: %d) {", indent, ch.Connection, ch.Page))
		lines = append(lines, indent+"  nodes {")
		lines = append(lines, inner...)
		lines = append(lines, indent+"  }")
		lines = append(lines, indent+"}")
	}
	return lines
}

// stripFirstArgument removes a `(first: N)` argument list from a selection.
func stripFirstArgument(sel string) string {
	open := strings.Index(sel, "(")
	if open < 0 {
		return sel
	}
	close := strings.Index(sel[open:], ")")
	if close < 0 {
		return sel
	}
	return sel[:open] + sel[open+close+1:]
}

// EmitSelectionDocument renders the fetch-by-GID document and the paged list
// document for one type, in one file. They share a selection set, and keeping
// them together is what stops the two paths drifting into different mirrors.
func (ps *PlanSet) EmitSelectionDocument(version string, p *TypePlan) string {
	var b strings.Builder
	b.WriteString(graphqlHeader(version, p))

	body := ps.selectionLines(p, "      ", true, false)

	if p.Entry.Singleton != "" {
		fmt.Fprintf(&b, "query ShopifyFetch%s {\n", title(p.Concept))
		fmt.Fprintf(&b, "  %s {\n", p.Entry.Singleton)
		for _, l := range body {
			b.WriteString(strings.TrimPrefix(l, "  ") + "\n")
		}
		b.WriteString("  }\n}\n")
		return b.String()
	}

	fmt.Fprintf(&b, "query ShopifyFetch%s($id: ID!) {\n", title(p.Concept))
	b.WriteString("  node(id: $id) {\n")
	b.WriteString("    __typename\n")
	fmt.Fprintf(&b, "    ... on %s {\n", p.GraphQLType)
	for _, l := range body {
		b.WriteString(l + "\n")
	}
	b.WriteString("    }\n  }\n}\n")

	if p.Entry.Query != "" {
		b.WriteString("\n")
		fmt.Fprintf(&b, "query ShopifyList%s($first: Int!, $after: String, $query: String) {\n", title(p.Concept))
		fmt.Fprintf(&b, "  %s(first: $first, after: $after, query: $query) {\n", p.Entry.Query)
		b.WriteString("    pageInfo { hasNextPage endCursor }\n")
		b.WriteString("    nodes {\n")
		for _, l := range body {
			b.WriteString(l + "\n")
		}
		b.WriteString("    }\n  }\n}\n")
	}
	return b.String()
}

// EmitBulkDocuments renders the bulk queries for one type: one operation
// normally, several when the child connections exceed what Shopify accepts in
// a single bulk query.
func (ps *PlanSet) EmitBulkDocuments(version string, p *TypePlan) (string, []string) {
	if !p.Entry.Bulk || p.Entry.Query == "" {
		return "", nil
	}
	// Only CONNECTION children count against the budget. A plain LIST
	// field is part of the parent's own JSON line and costs nothing.
	batches := [][]ChildPlan{nil}
	used := 0
	for _, ch := range p.Children {
		last := len(batches) - 1
		if !ch.List && used >= maxBulkConnections {
			batches = append(batches, nil)
			last++
			used = 0
		}
		if !ch.List {
			used++
		}
		batches[last] = append(batches[last], ch)
	}

	var file strings.Builder
	file.WriteString(graphqlHeader(version, p))
	var names []string
	for i, batch := range batches {
		// Only the FIRST operation writes the parent row. A later part
		// carries the same parent fields (a bulk query needs a root), but
		// applying it as a parent write would blank whatever the first
		// part's children set -- every generated write stamps every field.
		name := fmt.Sprintf("ShopifyBulk%s", title(p.Concept))
		if i > 0 {
			name = fmt.Sprintf("%sPart%d", name, i+1)
		}
		names = append(names, name)
		if i > 0 {
			file.WriteString("\n")
		}
		sub := *p
		sub.Children = batch
		body := ps.selectionLines(&sub, "        ", true, true)
		fmt.Fprintf(&file, "query %s($query: String) {\n", name)
		fmt.Fprintf(&file, "  %s(query: $query) {\n", p.Entry.Query)
		file.WriteString("    edges {\n      node {\n")
		for _, l := range body {
			file.WriteString(l + "\n")
		}
		file.WriteString("      }\n    }\n  }\n}\n")
	}
	return file.String(), names
}

func graphqlHeader(version string, p *TypePlan) string {
	return fmt.Sprintf(`# Code generated by cmd/shopifyschema from Admin GraphQL %s. DO NOT EDIT.
#
# The mirror selection for %s. Regenerate through cmd/shopifyschema; a hand
# edit here and the concept in dsl/shopify/generated/%s.memql would disagree,
# and the disagreement is a field the mirror fetches and never stores.

`, version, p.GraphQLType, p.Concept)
}
