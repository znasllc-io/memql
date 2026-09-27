package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func planSetFor(t *testing.T) *PlanSet {
	t.Helper()
	plans, err := NewPlanner(testSchema(), testAllowlist(t)).Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return NewPlanSet(plans)
}

// A list document declares $query only where the root connection accepts
// it. Seven of the recorded connections do not (channels, publications,
// themes, ...), and against those the declaration alone is refused --
// argumentNotAccepted plus variableNotUsed -- before any variable is bound.
func TestListDocumentDeclaresQueryOnlyWhereTheConnectionAcceptsIt(t *testing.T) {
	set := planSetFor(t)
	gadget := set.ByType("Gadget")
	if gadget.ListFilterable {
		t.Fatal("gadgets takes no query: argument, but the plan says it is filterable")
	}
	doc := set.EmitSelectionDocument("2026-07", gadget)
	if strings.Contains(doc, "$query") {
		t.Errorf("ShopifyListGadget declares $query against a connection that refuses it:\n%s", doc)
	}
	if !strings.Contains(doc, "query ShopifyListGadget($first: Int!, $after: String) {\n  gadgets(first: $first, after: $after) {") {
		t.Errorf("ShopifyListGadget is not the unfiltered form:\n%s", doc)
	}
	bulk, _ := set.EmitBulkDocuments("2026-07", gadget)
	if strings.Contains(bulk, "$query") || !strings.Contains(bulk, "query ShopifyBulkGadget {\n  gadgets {") {
		t.Errorf("ShopifyBulkGadget carries a query the connection refuses:\n%s", bulk)
	}

	thing := set.ByType("Thing")
	if !thing.ListFilterable {
		t.Fatal("things takes query:, but the plan says it is not filterable")
	}
	doc = set.EmitSelectionDocument("2026-07", thing)
	if !strings.Contains(doc, "query ShopifyListThing($first: Int!, $after: String, $query: String) {\n  things(first: $first, after: $after, query: $query) {") {
		t.Errorf("ShopifyListThing lost its filter:\n%s", doc)
	}
	bulk, _ = set.EmitBulkDocuments("2026-07", thing)
	if !strings.Contains(bulk, "query ShopifyBulkThing($query: String) {\n  things(query: $query) {") {
		t.Errorf("ShopifyBulkThing lost its filter:\n%s", bulk)
	}
}

// The runtime must know which form it got, so it never binds a $query the
// operation does not declare.
func TestTypeSpecRecordsWhetherTheListIsFilterable(t *testing.T) {
	set := planSetFor(t)
	if got := emitTypeSpec(set.ByType("Gadget"), "", "", nil); !strings.Contains(got, "ListFilterable: false,") {
		t.Errorf("gadget spec does not say ListFilterable: false:\n%s", got)
	}
	if got := emitTypeSpec(set.ByType("Thing"), "", "", nil); !strings.Contains(got, "ListFilterable: true,") {
		t.Errorf("thing spec does not say ListFilterable: true:\n%s", got)
	}
	if got := emitTypeSpec(set.ByType("Widget"), "", "", nil); strings.Contains(got, "ListFilterable") {
		t.Errorf("widget has no list operation, so the flag means nothing there:\n%s", got)
	}
}

// A type that declares no id is selected without one -- in its own document
// and nested inside a parent's -- and its spec says so, because its objects
// then arrive with no gid to key a row by.
func TestATypeWithoutAnIdIsSelectedWithoutOne(t *testing.T) {
	set := planSetFor(t)
	sticker := set.ByType("Sticker")
	if sticker.HasID {
		t.Fatal("Sticker declares no id, but the plan says it does")
	}
	for _, bulk := range []bool{false, true} {
		for _, l := range set.selectionLines(sticker, "", true, bulk) {
			if strings.TrimSpace(l) == "id" {
				t.Errorf("bulk=%t: Sticker's selection asks for an id it does not have", bulk)
			}
		}
	}
	if lines := set.selectionLines(set.ByType("Thing"), "", false, false); len(lines) == 0 || lines[0] != "id" {
		t.Errorf("Thing's selection must still lead with its id, got %v", lines)
	}
	if got := emitTypeSpec(sticker, "", "", nil); !strings.Contains(got, "HasID:       false,") {
		t.Errorf("sticker spec does not say HasID: false:\n%s", got)
	}
}

// The recorded fixture, end to end: every list and bulk document declares
// $query exactly when QueryRoot's connection accepts it. This is the check
// that would have caught the seven refused domains.
func TestRecordedListDocumentsDeclareQueryOnlyWhereAccepted(t *testing.T) {
	version, plans := recordedPlans(t)
	schema, err := ReadSchemaFile(filepath.Join(repoRoot(), "cmd", "shopifyschema", "testdata", "schema-"+version+".json"))
	if err != nil {
		t.Fatalf("recorded schema: %v", err)
	}
	root := schema.Lookup(schema.QueryType)
	if root == nil {
		t.Fatalf("the fixture has no %s type", schema.QueryType)
	}
	set := NewPlanSet(plans)
	unfiltered := 0
	for _, p := range plans {
		if p.Entry.Query == "" {
			continue
		}
		accepts := root.Field(p.Entry.Query).HasArg("query")
		if !accepts {
			unfiltered++
		}
		if doc := set.EmitSelectionDocument(version, p); strings.Contains(doc, "$query") != accepts {
			t.Errorf("%s: %s accepts query=%t but ShopifyList%s declares $query=%t", p.Concept, p.Entry.Query, accepts, title(p.Concept), !accepts)
		}
		if bulk, _ := set.EmitBulkDocuments(version, p); bulk != "" && strings.Contains(bulk, "$query") != accepts {
			t.Errorf("%s: %s accepts query=%t but the bulk document declares $query=%t", p.Concept, p.Entry.Query, accepts, !accepts)
		}
	}
	if unfiltered == 0 {
		t.Error("no recorded root connection refuses query:, so this test no longer exercises the rule")
	}
}

// The recorded fixture's id-less types, by name: the three the live sweep
// tripped over as children or references, plus the two reference
// connections whose node type has no id. Each must be inlined, and none may
// be asked for an id.
func TestRecordedIdlessTypesAreNeverAskedForAnId(t *testing.T) {
	_, plans := recordedPlans(t)
	set := NewPlanSet(plans)
	idless := 0
	for _, p := range plans {
		if p.HasID {
			continue
		}
		idless++
		for _, l := range set.selectionLines(p, "", true, false) {
			if strings.TrimSpace(l) == "id" {
				t.Errorf("%s declares no id, but its selection asks for one", p.GraphQLType)
			}
		}
	}
	if idless == 0 {
		t.Error("the fixture has no id-less mirrored type left, so this test no longer exercises the rule")
	}
	for _, want := range []struct{ typ, field, kind string }{
		{"Product", "resourcePublications", KindObjectList},
		{"Collection", "resourcePublications", KindObjectList},
		{"Order", "discountApplications", KindObjectList},
		{"MarketWebPresence", "defaultLocale", KindObject},
		{"MarketWebPresence", "alternateLocales", KindObjectList},
	} {
		p := set.ByType(want.typ)
		if p == nil {
			t.Errorf("%s is not planned", want.typ)
			continue
		}
		f, ok := fieldByGraphQLName(p, want.field)
		if !ok {
			t.Errorf("%s.%s is not mirrored", want.typ, want.field)
			continue
		}
		if f.Kind != want.kind {
			t.Errorf("%s.%s: kind %q selection %q -- the type has no id, so it must be inlined as %s", want.typ, want.field, f.Kind, f.Selection, want.kind)
		}
		if strings.HasSuffix(f.Name, "Gid") || strings.HasSuffix(f.Name, "Gids") {
			t.Errorf("%s.%s is named %q as if it carried GIDs", want.typ, want.field, f.Name)
		}
	}
}

func fieldByGraphQLName(p *TypePlan, graphql string) (FieldPlan, bool) {
	for _, f := range p.Fields {
		if f.GraphQL == graphql {
			return f, true
		}
	}
	return FieldPlan{}, false
}

// A union member that sits at the selection depth bound used to be dropped
// to `{ __typename }`: an order's percentage discount mirrored as a value
// with a kind and no number.
func TestRecordedUnionMembersAtTheDepthBoundKeepTheirLeaves(t *testing.T) {
	version, plans := recordedPlans(t)
	ps := NewPlanSet(plans)
	var sel string
	for _, plan := range plans {
		if plan.GraphQLType == "Order" {
			sel = ps.EmitSelectionDocument(version, plan)
		}
	}
	if sel == "" {
		t.Fatal("no Order plan in the recorded fixture")
	}
	if !strings.Contains(sel, "... on PricingPercentageValue { percentage }") {
		t.Fatalf("Order.discountApplications.value lost its percentage member:\n%s", sel)
	}
	if !strings.Contains(sel, "... on MoneyV2 { amount currencyCode }") {
		t.Fatalf("Order.discountApplications.value lost its money member:\n%s", sel)
	}
}
