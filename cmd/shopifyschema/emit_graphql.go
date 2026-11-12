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
	// The row's identity -- when the type has one. A type without an id
	// field (a price list's prices) is selected without it; asking for it
	// is what makes the Admin API refuse the parent's whole document.
	if p.HasID {
		lines = append(lines, indent+"id")
	}
	// The ORIGIN's version, which the apply guard compares (originVersion).
	// Selection only: it is the row's updatedAt, never a payload field
	// (planType keeps updatedAt out of Fields), so nothing else puts it in the
	// document. Without it the connector stamped the time it read the object,
	// or streamed a bulk line, and an older snapshot could replace a newer
	// delivery. A type whose version is a field it also mirrors (createdAt)
	// already selects it.
	if p.VersionField != "" && !p.selectsField(p.VersionField, bulk) {
		lines = append(lines, indent+p.VersionField)
	}
	for _, f := range p.Fields {
		sel := f.Selection
		if sel == "" {
			sel = f.GraphQL
		}
		if bulk {
			switch {
			case f.Kind == KindRefs || f.OmitFromBulk:
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
		// A fetch reads the connection one page deep and says whether more
		// follow: the page operation (pageOperation) reads the rest, so an
		// order with more lines than one page holds keeps them all.
		lines = append(lines, fmt.Sprintf("%s%s(first: %d) {", indent, ch.Connection, ch.Page))
		lines = append(lines, indent+"  pageInfo { hasNextPage endCursor }")
		lines = append(lines, indent+"  nodes {")
		lines = append(lines, inner...)
		lines = append(lines, indent+"  }")
		lines = append(lines, indent+"}")
	}
	return lines
}

// selectsField reports whether the type's own field selections already ask
// for `name` bare, in the document form given (bulk omits some fields).
func (p *TypePlan) selectsField(name string, bulk bool) bool {
	for _, f := range p.Fields {
		if bulk && (f.Kind == KindRefs || f.OmitFromBulk) {
			continue
		}
		sel := f.Selection
		if bulk && f.SelectionBulk != "" {
			sel = f.SelectionBulk
		}
		if sel == "" {
			sel = f.GraphQL
		}
		if strings.TrimSpace(sel) == name {
			return true
		}
	}
	return false
}

// pageOpName names the operation that reads one further page of a paged
// child connection: ShopifyPage<Concept><Connection>. Empty when the child
// has none -- a plain list is never paged, and a parent node(id:) cannot
// resolve has no id to read the page by.
func pageOpName(p *TypePlan, ch ChildPlan) string {
	if ch.List || !p.Fetchable() {
		return ""
	}
	return "ShopifyPage" + title(p.Concept) + title(ch.Connection)
}

// pageOperation renders the operation pageOpName names: the parent by its
// id (or the singleton), and that one connection from the `after` cursor
// on, with the same page size and selection the fetch uses.
func (ps *PlanSet) pageOperation(p *TypePlan, ch ChildPlan) string {
	name := pageOpName(p, ch)
	child := ps.byType[ch.Type]
	if name == "" || child == nil {
		return ""
	}
	var b strings.Builder
	indent := "      "
	if p.Entry.Singleton != "" {
		fmt.Fprintf(&b, "query %s($after: String!) {\n", name)
		fmt.Fprintf(&b, "  %s {\n", p.Entry.Singleton)
		indent = "    "
	} else {
		fmt.Fprintf(&b, "query %s($id: ID!, $after: String!) {\n", name)
		b.WriteString("  node(id: $id) {\n")
		b.WriteString("    __typename\n")
		fmt.Fprintf(&b, "    ... on %s {\n", p.GraphQLType)
	}
	fmt.Fprintf(&b, "%s%s(first: %d, after: $after) {\n", indent, ch.Connection, ch.Page)
	fmt.Fprintf(&b, "%s  pageInfo { hasNextPage endCursor }\n", indent)
	fmt.Fprintf(&b, "%s  nodes {\n", indent)
	for _, l := range ps.selectionLines(child, indent+"    ", false, false) {
		b.WriteString(l + "\n")
	}
	fmt.Fprintf(&b, "%s  }\n", indent)
	fmt.Fprintf(&b, "%s}\n", indent)
	if p.Entry.Singleton != "" {
		b.WriteString("  }\n}\n")
	} else {
		b.WriteString("    }\n  }\n}\n")
	}
	return b.String()
}

// pageOperations renders every page operation of a type, each preceded by
// a blank line, in child order.
func (ps *PlanSet) pageOperations(p *TypePlan) string {
	var b strings.Builder
	for _, ch := range p.Children {
		if op := ps.pageOperation(p, ch); op != "" {
			b.WriteString("\n")
			b.WriteString(op)
		}
	}
	return b.String()
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
		b.WriteString(ps.pageOperations(p))
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
		// $query only where the connection takes it. Declaring it against
		// a connection that does not is refused outright (argumentNotAccepted
		// plus variableNotUsed), with or without a value bound to it.
		if p.ListFilterable {
			fmt.Fprintf(&b, "query ShopifyList%s($first: Int!, $after: String, $query: String) {\n", title(p.Concept))
			fmt.Fprintf(&b, "  %s(first: $first, after: $after, query: $query) {\n", p.Entry.Query)
		} else {
			fmt.Fprintf(&b, "query ShopifyList%s($first: Int!, $after: String) {\n", title(p.Concept))
			fmt.Fprintf(&b, "  %s(first: $first, after: $after) {\n", p.Entry.Query)
		}
		b.WriteString("    pageInfo { hasNextPage endCursor }\n")
		b.WriteString("    nodes {\n")
		for _, l := range body {
			b.WriteString(l + "\n")
		}
		b.WriteString("    }\n  }\n}\n")
	}
	b.WriteString(ps.pageOperations(p))
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
		// The same rule as the list document: $query only where accepted.
		if p.ListFilterable {
			fmt.Fprintf(&file, "query %s($query: String) {\n", name)
			fmt.Fprintf(&file, "  %s(query: $query) {\n", p.Entry.Query)
		} else {
			fmt.Fprintf(&file, "query %s {\n", name)
			fmt.Fprintf(&file, "  %s {\n", p.Entry.Query)
		}
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
