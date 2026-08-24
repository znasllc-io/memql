package main

import (
	"strings"
	"testing"
)

// testSchema is a hand-built miniature of the Admin schema carrying one
// example of every mapping rule. Written by hand rather than sliced out of
// the recorded fixture so a rule's test says what the rule IS, not what
// Shopify happened to ship this quarter.
func testSchema() *Schema {
	named := func(kind, name string) *TypeRef { return &TypeRef{Kind: kind, Name: name} }
	nn := func(t *TypeRef) *TypeRef { return &TypeRef{Kind: "NON_NULL", OfType: t} }
	list := func(t *TypeRef) *TypeRef { return &TypeRef{Kind: "LIST", OfType: t} }

	s := &Schema{Version: "2026-07", QueryType: "QueryRoot"}
	add := func(t *Type) { s.Types = append(s.Types, t) }

	for _, name := range []string{"String", "ID", "Boolean", "Int", "Float", "DateTime", "Money", "Decimal", "URL", "JSON", "UnsignedInt64"} {
		add(&Type{Kind: "SCALAR", Name: name})
	}
	add(&Type{Kind: "OBJECT", Name: "Node"})
	add(&Type{Kind: "INTERFACE", Name: "Node", Fields: []Field{{Name: "id", Type: nn(named("SCALAR", "ID"))}}})
	add(&Type{Kind: "ENUM", Name: "SmallEnum", EnumValues: []EnumValue{{Name: "OPEN"}, {Name: "CLOSED"}, {Name: "GONE", IsDeprecated: true}}})

	big := &Type{Kind: "ENUM", Name: "BigEnum"}
	for i := 0; i < maxInlineEnumValues+1; i++ {
		big.EnumValues = append(big.EnumValues, EnumValue{Name: "V" + string(rune('A'+i%26)) + string(rune('0'+i/26))})
	}
	add(big)

	add(&Type{Kind: "OBJECT", Name: "MoneyV2", Fields: []Field{
		{Name: "amount", Type: nn(named("SCALAR", "Decimal"))},
		{Name: "currencyCode", Type: nn(named("ENUM", "BigEnum"))},
	}})
	add(&Type{Kind: "OBJECT", Name: "MoneyBag", Fields: []Field{
		{Name: "shopMoney", Type: nn(named("OBJECT", "MoneyV2"))},
		{Name: "presentmentMoney", Type: nn(named("OBJECT", "MoneyV2"))},
	}})
	add(&Type{Kind: "OBJECT", Name: "Address", Fields: []Field{
		{Name: "city", Type: named("SCALAR", "String")},
		{Name: "zip", Type: named("SCALAR", "String")},
	}})
	add(&Type{Kind: "INTERFACE", Name: "Instrument", Fields: []Field{
		{Name: "id", Type: nn(named("SCALAR", "ID"))},
	}})
	add(&Type{Kind: "OBJECT", Name: "Metafield", Fields: []Field{
		{Name: "namespace", Type: nn(named("SCALAR", "String"))},
	}})
	add(&Type{Kind: "OBJECT", Name: "MetafieldConnection", Fields: []Field{
		{Name: "nodes", Type: nn(list(nn(named("OBJECT", "Metafield"))))},
	}})
	add(&Type{Kind: "OBJECT", Name: "TagConnection", Fields: []Field{
		{Name: "nodes", Type: nn(list(nn(named("OBJECT", "Tag"))))},
	}})
	add(&Type{Kind: "OBJECT", Name: "Tag", Fields: []Field{{Name: "id", Type: nn(named("SCALAR", "ID"))}}})
	add(&Type{Kind: "OBJECT", Name: "LineConnection", Fields: []Field{
		{Name: "nodes", Type: nn(list(nn(named("OBJECT", "Line"))))},
	}})
	add(&Type{Kind: "OBJECT", Name: "Line", Interfaces: []TypeRef{*named("INTERFACE", "Node")}, Fields: []Field{
		{Name: "id", Type: nn(named("SCALAR", "ID"))},
		{Name: "updatedAt", Type: nn(named("SCALAR", "DateTime"))},
		{Name: "title", Type: named("SCALAR", "String")},
		{Name: "widget", Type: named("OBJECT", "Widget")},
	}})
	add(&Type{Kind: "OBJECT", Name: "Widget", Interfaces: []TypeRef{*named("INTERFACE", "Node")}, Fields: []Field{
		{Name: "id", Type: nn(named("SCALAR", "ID"))},
		{Name: "updatedAt", Type: nn(named("SCALAR", "DateTime"))},
		{Name: "name", Type: named("SCALAR", "String")},
	}})
	add(&Type{Kind: "OBJECT", Name: "WidgetConnection", Fields: []Field{
		{Name: "nodes", Type: nn(list(nn(named("OBJECT", "Widget"))))},
	}})

	add(&Type{Kind: "OBJECT", Name: "Thing", Interfaces: []TypeRef{*named("INTERFACE", "Node")}, Fields: []Field{
		{Name: "id", Type: nn(named("SCALAR", "ID"))},
		{Name: "updatedAt", Type: nn(named("SCALAR", "DateTime"))},
		{Name: "createdAt", Type: nn(named("SCALAR", "DateTime"))},
		{Name: "type", Type: named("SCALAR", "String")},
		{Name: "default", Type: named("SCALAR", "String")},
		{Name: "title", Type: nn(named("SCALAR", "String"))},
		{Name: "count", Type: named("SCALAR", "Int")},
		{Name: "ratio", Type: named("SCALAR", "Float")},
		{Name: "live", Type: nn(named("SCALAR", "Boolean"))},
		{Name: "blob", Type: named("SCALAR", "JSON")},
		{Name: "state", Type: named("ENUM", "SmallEnum")},
		{Name: "currency", Type: named("ENUM", "BigEnum")},
		{Name: "price", Type: named("OBJECT", "MoneyV2")},
		{Name: "totalSet", Type: named("OBJECT", "MoneyBag")},
		{Name: "address", Type: named("OBJECT", "Address")},
		{Name: "instrument", Type: named("INTERFACE", "Instrument")},
		{Name: "tagList", Type: nn(list(nn(named("SCALAR", "String"))))},
		{Name: "addresses", Type: nn(list(nn(named("OBJECT", "Address"))))},
		{Name: "widget", Type: named("OBJECT", "Widget")},
		{Name: "widgets", Type: nn(named("OBJECT", "WidgetConnection"))},
		{Name: "metafields", Type: nn(named("OBJECT", "MetafieldConnection"))},
		{Name: "tags", Type: nn(named("OBJECT", "TagConnection"))},
		{Name: "lines", Type: nn(named("OBJECT", "LineConnection"))},
		{Name: "oldPrice", Type: named("SCALAR", "Money"), IsDeprecated: true, DeprecationReason: "use price"},
		{Name: "metafield", Args: []InputValue{{Name: "key", Type: nn(named("SCALAR", "String"))}}, Type: named("OBJECT", "Metafield")},
	}})
	add(&Type{Kind: "OBJECT", Name: "ThingConnection", Fields: []Field{
		{Name: "nodes", Type: nn(list(nn(named("OBJECT", "Thing"))))},
	}})
	add(&Type{Kind: "OBJECT", Name: "QueryRoot", Fields: []Field{
		{Name: "things", Args: []InputValue{{Name: "first", Type: named("SCALAR", "Int")}, {Name: "query", Type: named("SCALAR", "String")}}, Type: nn(named("OBJECT", "ThingConnection"))},
	}})
	return s
}

func testAllowlist(t *testing.T) *Allowlist {
	t.Helper()
	al := &Allowlist{
		APIVersion: "2026-07",
		Types: []Entry{
			{
				Type: "Thing", Concept: "thing", Query: "things", Reconcile: ReconcileUpdatedAt, Bulk: true,
				Children:   []Child{{Connection: "lines", Type: "Line"}},
				References: []string{"tags", "widgets"},
				Topics:     map[string]string{"THINGS_UPDATE": ActionUpsert, "THINGS_DELETE": ActionDelete},
			},
			{Type: "Line", Concept: "thingLine", Reconcile: ReconcileNone},
			{Type: "Widget", Concept: "widget", Reconcile: ReconcileNone},
		},
	}
	if err := al.validate(); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	return al
}

func planFor(t *testing.T, concept string) *TypePlan {
	t.Helper()
	plans, err := NewPlanner(testSchema(), testAllowlist(t)).Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, p := range plans {
		if p.Concept == concept {
			return p
		}
	}
	t.Fatalf("no plan for %q", concept)
	return nil
}

func fieldByName(t *testing.T, p *TypePlan, name string) FieldPlan {
	t.Helper()
	for _, f := range p.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q on %s (have %v)", name, p.Concept, fieldNames(p))
	return FieldPlan{}
}

func fieldNames(p *TypePlan) []string {
	out := make([]string, 0, len(p.Fields))
	for _, f := range p.Fields {
		out = append(out, f.Name)
	}
	return out
}

func hasField(p *TypePlan, name string) bool {
	for _, f := range p.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// --- spec 4.2, one rule per test ---------------------------------------

func TestScalarsMapToTheMatchingType(t *testing.T) {
	p := planFor(t, "thing")
	for name, want := range map[string]string{
		"title": "string", "count": "int", "ratio": "float", "live": "bool", "blob": "any",
	} {
		if got := fieldByName(t, p, name).DSLType; got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

// A mirrored timestamp is a string, not a datetime, and the reason is the
// write path rather than the type system -- see scalarToDSL.
func TestMirroredTimestampsAreStrings(t *testing.T) {
	p := planFor(t, "thing")
	if got := fieldByName(t, p, "sourceCreatedAt").DSLType; got != "string" {
		t.Errorf("createdAt: got %q, want string", got)
	}
}

func TestSmallEnumsAreClosedAndDropDeprecatedValues(t *testing.T) {
	f := fieldByName(t, planFor(t, "thing"), "state")
	if f.Kind != KindEnum {
		t.Fatalf("kind %q, want %q", f.Kind, KindEnum)
	}
	if !strings.Contains(f.DSLType, `"OPEN"`) || !strings.Contains(f.DSLType, `"CLOSED"`) {
		t.Errorf("enum %q lost a value", f.DSLType)
	}
	if strings.Contains(f.DSLType, "GONE") {
		t.Errorf("enum %q kept a deprecated value", f.DSLType)
	}
}

func TestLargeEnumsBecomeStringsAndSayWhy(t *testing.T) {
	f := fieldByName(t, planFor(t, "thing"), "currency")
	if f.DSLType != "string" {
		t.Errorf("got %q, want string -- an enum above maxInlineEnumValues is a catalogue, not a state", f.DSLType)
	}
	if !strings.Contains(f.Doc, "BigEnum") {
		t.Errorf("doc %q does not name the schema enum, so the constraint is undiscoverable", f.Doc)
	}
}

func TestMoneyIsRenderedWholeAtAnyDepth(t *testing.T) {
	p := planFor(t, "thing")
	if got := fieldByName(t, p, "price").Selection; got != "price { amount currencyCode }" {
		t.Errorf("MoneyV2: got %q", got)
	}
	bag := fieldByName(t, p, "totalSet").Selection
	if !strings.Contains(bag, "shopMoney { amount currencyCode }") || !strings.Contains(bag, "presentmentMoney { amount currencyCode }") {
		t.Errorf("MoneyBag: got %q", bag)
	}
}

func TestNestedObjectsBecomeAnObjectFieldWithASelection(t *testing.T) {
	f := fieldByName(t, planFor(t, "thing"), "address")
	if f.DSLType != "object" {
		t.Errorf("got %q, want object", f.DSLType)
	}
	if !strings.Contains(f.Selection, "city") || !strings.Contains(f.Selection, "zip") {
		t.Errorf("selection %q does not carry the nested fields", f.Selection)
	}
}

func TestListsOfObjectsBecomeObjectArrays(t *testing.T) {
	if got := fieldByName(t, planFor(t, "thing"), "addresses").DSLType; got != "[]object" {
		t.Errorf("got %q, want []object", got)
	}
}

func TestScalarListsKeepTheirElementType(t *testing.T) {
	if got := fieldByName(t, planFor(t, "thing"), "tagList").DSLType; got != "[]string" {
		t.Errorf("got %q, want []string", got)
	}
}

func TestInterfacesCarryTypename(t *testing.T) {
	f := fieldByName(t, planFor(t, "thing"), "instrument")
	if !strings.Contains(f.Selection, "__typename") {
		t.Errorf("selection %q lacks __typename, so the member is unidentifiable", f.Selection)
	}
}

func TestDataCarryingConnectionsBecomeChildConcepts(t *testing.T) {
	p := planFor(t, "thing")
	if hasField(p, "lines") {
		t.Error("a child connection must not also be a field on the parent")
	}
	if len(p.Children) != 1 || p.Children[0].Concept != "thingLine" {
		t.Fatalf("children = %+v", p.Children)
	}
	child := planFor(t, "thingLine")
	if child.ParentConcept != "thing" || child.ParentConnection != "lines" {
		t.Errorf("child lineage = %q/%q", child.ParentConcept, child.ParentConnection)
	}
}

func TestReferenceOnlyConnectionsBecomeGidLists(t *testing.T) {
	f := fieldByName(t, planFor(t, "thing"), "tagsGids")
	if f.Kind != KindRefs || f.DSLType != "[]string" {
		t.Fatalf("kind %q type %q", f.Kind, f.DSLType)
	}
	if !strings.Contains(f.Selection, "{ nodes { id } }") {
		t.Errorf("selection %q should read ids only", f.Selection)
	}
	if f.SelectionBulk != "" {
		t.Error("a reference connection must stay out of the bulk document")
	}
}

// A connection or object whose type is itself mirrored is a REFERENCE: two
// inline copies of one row drift apart between fetches.
func TestMirroredTypesAreReferencedByGid(t *testing.T) {
	p := planFor(t, "thing")
	single := fieldByName(t, p, "widgetGid")
	if single.Kind != KindGid || single.Extract != "widget.id" {
		t.Errorf("single: kind %q extract %q", single.Kind, single.Extract)
	}
	many := fieldByName(t, p, "widgetsGids")
	if many.Kind != KindRefs || many.Extract != "widgets.nodes[].id" {
		t.Errorf("many: kind %q extract %q", many.Kind, many.Extract)
	}
}

func TestMetafieldsAreKeyedByNamespaceAndKey(t *testing.T) {
	f := fieldByName(t, planFor(t, "thing"), "metafields")
	if f.Kind != KindMetafields || f.DSLType != "object" {
		t.Fatalf("kind %q type %q", f.Kind, f.DSLType)
	}
	for _, want := range []string{"namespace", "key", "type", "value", "jsonValue"} {
		if !strings.Contains(f.Selection, want) {
			t.Errorf("selection %q lacks %q", f.Selection, want)
		}
	}
}

func TestDeprecatedFieldsAreOmitted(t *testing.T) {
	if hasField(planFor(t, "thing"), "oldPrice") {
		t.Error("a deprecated field must be dropped in favour of its replacement")
	}
}

func TestFieldsNeedingAnArgumentAreOmitted(t *testing.T) {
	if hasField(planFor(t, "thing"), "metafield") {
		t.Error("a field with a required argument cannot be selected and must be omitted")
	}
}

// Two rename rules, both of which are silent data loss if they are wrong: a
// reserved payload name is refused by the concept loader, and a language
// keyword fails the parse of the whole file.
func TestReservedAndKeywordNamesAreRenamedNotDropped(t *testing.T) {
	p := planFor(t, "thing")
	for graphql, want := range map[string]string{"type": "sourceType", "default": "sourceDefault", "createdAt": "sourceCreatedAt"} {
		f := fieldByName(t, p, want)
		if f.GraphQL != graphql {
			t.Errorf("%s: renamed field points at %q", want, f.GraphQL)
		}
		if f.Extract != graphql {
			t.Errorf("%s: extract path %q must still name the Admin field", want, f.Extract)
		}
	}
	if hasField(p, "id") {
		t.Error("the object's own id is carried as gid, never as a payload field")
	}
}

func TestVersionFieldPrefersUpdatedAt(t *testing.T) {
	p := planFor(t, "thing")
	if got := p.VersionField; got != "updatedAt" {
		t.Errorf("got %q", got)
	}
	// ...and it is NOT also a payload field: the mirror column is it.
	if hasField(p, "sourceUpdatedAt") || hasField(p, "updatedAt") {
		t.Error("the origin's updatedAt is the mirror's updatedAt column, not a second payload field")
	}
}

func TestChildFindsItsParentThroughABackReference(t *testing.T) {
	// Line has no field of type Thing, so a directly-fetched line cannot
	// resolve its parent -- and the plan says so rather than guessing.
	if got := planFor(t, "thingLine").ParentGidPath; got != "" {
		t.Errorf("got %q, want empty: Line carries no back-reference to Thing", got)
	}
}
