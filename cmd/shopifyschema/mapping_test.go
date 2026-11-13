package main

import (
	"fmt"
	"path/filepath"
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
	// Mirrored, but with NO id field: the shape of ShopLocale and of a
	// price list's prices. Nothing about it can be referenced by GID.
	add(&Type{Kind: "OBJECT", Name: "Sticker", Fields: []Field{
		{Name: "label", Type: named("SCALAR", "String")},
		{Name: "widget", Type: named("OBJECT", "Widget")},
	}})
	add(&Type{Kind: "OBJECT", Name: "StickerConnection", Fields: []Field{
		{Name: "nodes", Type: nn(list(nn(named("OBJECT", "Sticker"))))},
	}})
	add(&Type{Kind: "UNION", Name: "Sticky", PossibleTypes: []TypeRef{*named("OBJECT", "Sticker"), *named("OBJECT", "Widget")}})
	// Listed through a root connection that takes no query: argument.
	add(&Type{Kind: "OBJECT", Name: "Gadget", Interfaces: []TypeRef{*named("INTERFACE", "Node")}, Fields: []Field{
		{Name: "id", Type: nn(named("SCALAR", "ID"))},
		{Name: "updatedAt", Type: nn(named("SCALAR", "DateTime"))},
		{Name: "name", Type: named("SCALAR", "String")},
	}})
	add(&Type{Kind: "OBJECT", Name: "GadgetConnection", Fields: []Field{
		{Name: "nodes", Type: nn(list(nn(named("OBJECT", "Gadget"))))},
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
		{Name: "sticker", Type: named("OBJECT", "Sticker")},
		{Name: "stickers", Type: nn(list(nn(named("OBJECT", "Sticker"))))},
		{Name: "stickerFeed", Type: nn(named("OBJECT", "StickerConnection"))},
		{Name: "sticky", Type: named("UNION", "Sticky")},
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
		{Name: "gadgets", Args: []InputValue{{Name: "first", Type: named("SCALAR", "Int")}, {Name: "after", Type: named("SCALAR", "String")}}, Type: nn(named("OBJECT", "GadgetConnection"))},
		// metafieldDefinitions(ownerType:) and metaobjects(type:), in miniature.
		{Name: "gizmos", Args: []InputValue{{Name: "first", Type: named("SCALAR", "Int")}, {Name: "kind", Type: nn(named("SCALAR", "String"))}}, Type: nn(named("OBJECT", "GadgetConnection"))},
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
				References: []string{"tags", "widgets", "stickerFeed"},
				Topics:     map[string]string{"THINGS_UPDATE": ActionUpsert, "THINGS_DELETE": ActionDelete},
			},
			{Type: "Line", Concept: "thingLine", Reconcile: ReconcileNone},
			{Type: "Widget", Concept: "widget", Reconcile: ReconcileNone},
			{Type: "Sticker", Concept: "sticker", Reconcile: ReconcileNone},
			{Type: "Gadget", Concept: "gadget", Query: "gadgets", Reconcile: ReconcileFullRelist, Cadence: "1h", Bulk: true},
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

// The DSL type flattens what a value IS: a timestamp, an amount and a name
// are all `string`. The Admin type is carried beside it, for every kind of
// field, so a reader of the model can tell a date from a name.
func TestEveryFieldCarriesItsAdminType(t *testing.T) {
	p := planFor(t, "thing")
	for name, want := range map[string]string{
		"title":           "String",
		"count":           "Int",
		"ratio":           "Float",
		"live":            "Boolean",
		"blob":            "JSON",
		"sourceCreatedAt": "DateTime",
		"state":           "SmallEnum",
		"currency":        "BigEnum",
		"price":           "MoneyV2",
		"totalSet":        "MoneyBag",
		"address":         "Address",
		"addresses":       "Address",
		"tagList":         "String",
		"widgetGid":       "Widget",
		"widgetsGids":     "Widget",
		"tagsGids":        "Tag",
		"stickerFeed":     "Sticker",
		"sticky":          "Sticky",
		"metafields":      "Metafield",
	} {
		if got := fieldByName(t, p, name).AdminType; got != want {
			t.Errorf("%s: Admin type %q, want %q", name, got, want)
		}
	}
	for _, f := range p.Fields {
		if f.AdminType == "" {
			t.Errorf("%s carries no Admin type", f.Name)
		}
	}
}

// A nested object that keeps the GID of a mirrored row is about that row. The
// plan says which mirrored types an object's selection keeps the GID of, at
// any depth it selects, so a reader of the model can tell without reading the
// selection.
func TestAnObjectSaysWhichMirroredRowsItKeepsTheGidOf(t *testing.T) {
	p := planFor(t, "thing")
	for name, want := range map[string]string{
		"sticker":     "Widget", // a field of the nested type
		"stickers":    "Widget", // the same, in a list
		"stickerFeed": "Widget", // the same, through a connection of an id-less type
		"sticky":      "Widget", // a member of a union
		"address":     "",
		"price":       "",
		"widgetGid":   "", // a reference is not an object
		"title":       "",
	} {
		if got := strings.Join(fieldByName(t, p, name).GidsInside, ","); got != want {
			t.Errorf("%s: keeps the GIDs of %q, want %q", name, got, want)
		}
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

// A mirrored type that declares no id has no GID to reference. `{ id }` on
// it is a field the schema does not have -- ShopLocale under a market's web
// presence, ResourcePublication under a product -- and the Admin API refuses
// the PARENT's whole document, fetch and sweep alike. Such a type is inlined
// like any other nested shape, in every position it can appear.
func TestMirroredTypesWithoutAnIdAreInlinedNotReferenced(t *testing.T) {
	p := planFor(t, "thing")
	for _, name := range []string{"stickerGid", "stickersGids", "stickerFeedGids"} {
		if hasField(p, name) {
			t.Errorf("%s: a type without an id cannot be kept as a GID", name)
		}
	}
	single := fieldByName(t, p, "sticker")
	if single.Kind != KindObject || single.DSLType != "object" || single.Extract != "sticker" {
		t.Errorf("object: kind %q type %q extract %q", single.Kind, single.DSLType, single.Extract)
	}
	if want := "sticker { label widget { id } }"; single.Selection != want {
		t.Errorf("object: selection %q, want %q", single.Selection, want)
	}
	many := fieldByName(t, p, "stickers")
	if many.Kind != KindObjectList || many.DSLType != "[]object" || many.Extract != "stickers" {
		t.Errorf("list: kind %q type %q extract %q", many.Kind, many.DSLType, many.Extract)
	}
	feed := fieldByName(t, p, "stickerFeed")
	if feed.Kind != KindObjectList || feed.DSLType != "[]object" || feed.Extract != "stickerFeed.nodes[]" {
		t.Errorf("connection: kind %q type %q extract %q", feed.Kind, feed.DSLType, feed.Extract)
	}
	if want := "stickerFeed(first: 25) { nodes { label widget { id } } }"; feed.Selection != want {
		t.Errorf("connection: selection %q, want %q", feed.Selection, want)
	}
	if !feed.OmitFromBulk {
		t.Error("connection: an inline reference connection stays out of bulk exactly as a GID one does")
	}
	union := fieldByName(t, p, "sticky")
	if want := "sticky { __typename ... on Sticker { label widget { id } } ... on Widget { id } }"; union.Selection != want {
		t.Errorf("union: selection %q, want %q", union.Selection, want)
	}
	for _, f := range p.Fields {
		if strings.Contains(f.Selection, "Sticker { id }") || strings.Contains(f.Selection, "sticker { id }") {
			t.Errorf("%s: selection %q asks Sticker for an id it does not have", f.Name, f.Selection)
		}
	}
	// ...while a mirrored type WITH an id is still a reference.
	if got := fieldByName(t, p, "widgetGid"); got.Selection != "widget { id }" {
		t.Errorf("widget: selection %q -- the id rule must not un-reference a type that has one", got.Selection)
	}
}

// A root connection with a required argument the list document never
// supplies -- metafieldDefinitions(ownerType:), metaobjects(type:) -- cannot
// be listed generically. Saying so at generation time, naming the connection
// and the argument, beats a missingRequiredArguments error on every sweep.
func TestAConnectionRequiringAnExtraArgumentIsRefused(t *testing.T) {
	al := &Allowlist{APIVersion: "2026-07", Types: []Entry{
		{Type: "Gadget", Concept: "gadget", Query: "gizmos", Reconcile: ReconcileFullRelist, Cadence: "1h"},
	}}
	if err := al.validate(); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	_, err := NewPlanner(testSchema(), al).Plan()
	if err == nil {
		t.Fatal("a generic listing through gizmos(kind:) was accepted")
	}
	for _, want := range []string{"gizmos", `"kind"`, ReconcileNone} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// The updated_at mode rides the query: argument. A connection without one
// cannot carry the filter, whatever the allowlist says.
func TestUpdatedAtReconcileNeedsAFilterableConnection(t *testing.T) {
	al := &Allowlist{APIVersion: "2026-07", Types: []Entry{
		{Type: "Gadget", Concept: "gadget", Query: "gadgets", Reconcile: ReconcileUpdatedAt},
	}}
	if err := al.validate(); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	_, err := NewPlanner(testSchema(), al).Plan()
	if err == nil || !strings.Contains(err.Error(), "gadgets") || !strings.Contains(err.Error(), "query") {
		t.Fatalf("updated_at over an unfilterable connection was accepted, or the error does not say why: %v", err)
	}
}

// Both listing modes page a root connection; without one the runtime
// silently reconciles nothing, which looks like a healthy domain.
func TestAListedReconcileNeedsAQuery(t *testing.T) {
	al := &Allowlist{APIVersion: "2026-07", Types: []Entry{
		{Type: "Gadget", Concept: "gadget", Reconcile: ReconcileFullRelist, Cadence: "1h"},
	}}
	if err := al.validate(); err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("a full re-list with nothing to page was accepted: %v", err)
	}
}

// --- inlined connections ------------------------------------------------

// recordedPlanWith plans an allowlist of the test's own against the recorded
// fixture, so an inline rule is tested on the types Shopify really ships.
func recordedPlanWith(t *testing.T, types ...Entry) ([]*TypePlan, error) {
	t.Helper()
	schema, err := ReadSchemaFile(filepath.Join(repoRoot(), "cmd", "shopifyschema", "testdata", "schema-2026-07.json"))
	if err != nil {
		t.Fatalf("recorded schema: %v", err)
	}
	al := &Allowlist{APIVersion: "2026-07", Types: types}
	if err := al.validate(); err != nil {
		return nil, err
	}
	return NewPlanner(schema, al).Plan()
}

// recordedField is one field of the recorded plan of a type, by its GraphQL
// name.
func recordedField(t *testing.T, plans []*TypePlan, typeName, graphql string) FieldPlan {
	t.Helper()
	for _, p := range plans {
		if p.GraphQLType != typeName {
			continue
		}
		if f, ok := fieldByGraphQLName(p, graphql); ok {
			return f
		}
		t.Fatalf("%s has no field %s (have %v)", typeName, graphql, fieldNames(p))
	}
	t.Fatalf("no plan for %s", typeName)
	return FieldPlan{}
}

// A refund's shipping lines, selected whole, would carry the order's shipping
// line and the phone number at the buyer's delivery address into every refund
// row. Inlined, they carry the two amounts the allowlist names and nothing
// else.
func TestAnInlinedConnectionCarriesOnlyTheFieldsItNames(t *testing.T) {
	_, plans := recordedPlans(t)
	f := recordedField(t, plans, "Refund", "refundShippingLines")
	money := "{ shopMoney { amount currencyCode } presentmentMoney { amount currencyCode } }"
	want := FieldPlan{
		Kind: KindInlineList, DSLType: "[]object", OmitFromBulk: true, Page: 10,
		Extract:   "refundShippingLines.nodes[]",
		Selection: "refundShippingLines(first: 10) { nodes { subtotalAmountSet " + money + " taxAmountSet " + money + " } }",
	}
	if f.Kind != want.Kind || f.DSLType != want.DSLType || f.Required || !f.OmitFromBulk || f.Page != want.Page || f.Extract != want.Extract {
		t.Errorf("refundShippingLines planned as kind %q type %q required %t omitFromBulk %t page %d extract %q, want %q %q false true %d %q",
			f.Kind, f.DSLType, f.Required, f.OmitFromBulk, f.Page, f.Extract, want.Kind, want.DSLType, want.Page, want.Extract)
	}
	if f.Selection != want.Selection {
		t.Errorf("selection\n  %s\nwant\n  %s", f.Selection, want.Selection)
	}
	for _, never := range []string{"shippingLine", "phone"} {
		if strings.Contains(f.Selection, never+" ") || strings.Contains(f.Selection, never+"{") {
			t.Errorf("the inlined selection carries %s: %s", never, f.Selection)
		}
	}
}

// What an inlined connection holds is said in the plan, so the erasure can
// check each held field without reading the selection.
func TestAnInlinedConnectionSaysWhatItHolds(t *testing.T) {
	_, plans := recordedPlans(t)
	f := recordedField(t, plans, "Refund", "refundLineItems")
	if f.Kind != KindInlineList || f.AdminType != "RefundLineItem" || f.Page != 50 {
		t.Errorf("refundLineItems is kind %q of %q, page %d; want %q of RefundLineItem, page 50", f.Kind, f.AdminType, f.Page, KindInlineList)
	}
	if got := strings.Join(f.GidsInside, ","); got != "LineItem" {
		t.Errorf("refundLineItems keeps the GIDs of %q, want LineItem", got)
	}
	want := []HeldField{
		{Name: "lineItem", Kind: KindGid, AdminType: "LineItem"},
		{Name: "quantity", Kind: KindScalar, AdminType: "Int"},
		{Name: "subtotalSet", Kind: KindObject, AdminType: "MoneyBag"},
		{Name: "totalTaxSet", Kind: KindObject, AdminType: "MoneyBag"},
	}
	if fmt.Sprint(f.Holds) != fmt.Sprint(want) {
		t.Errorf("refundLineItems holds %+v, want %+v", f.Holds, want)
	}
	if !strings.Contains(f.Selection, "lineItem { id }") {
		t.Errorf("the line a refund line names is not kept as a reference: %s", f.Selection)
	}
	adj := recordedField(t, plans, "Refund", "orderAdjustments")
	if got := fmt.Sprint(adj.Holds); got != fmt.Sprint([]HeldField{
		{Name: "amountSet", Kind: KindObject, AdminType: "MoneyBag"},
		{Name: "taxAmountSet", Kind: KindObject, AdminType: "MoneyBag"},
		{Name: "reason", Kind: KindEnum, AdminType: "OrderAdjustmentDiscrepancyReason"},
	}) {
		t.Errorf("orderAdjustments holds %s", got)
	}
	if adj.GidsInside != nil {
		t.Errorf("orderAdjustments says it keeps the GIDs of %v", adj.GidsInside)
	}
}

// What a person typed is never inlined: a shipping line (an object that
// holds the phone at the delivery address) and a string are both refused,
// naming the entry, the connection and the field.
func TestAnInlinedFieldThatIsTextOrAnObjectIsRefused(t *testing.T) {
	for _, tc := range []struct{ connection, field string }{
		{"refundShippingLines", "shippingLine"},
		{"transactions", "gateway"},
	} {
		_, err := recordedPlanWith(t, Entry{Type: "Refund", Concept: "refund", Reconcile: ReconcileNone,
			Inline: []Inline{{Connection: tc.connection, Page: 10, Fields: []string{tc.field}}}})
		if err == nil {
			t.Errorf("inline %s with %s was accepted", tc.connection, tc.field)
			continue
		}
		for _, want := range []string{"Refund", tc.connection, tc.field, "nothing a person typed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
	}
}

func TestAnInlineWithNoFieldsIsRefused(t *testing.T) {
	_, err := recordedPlanWith(t, Entry{Type: "Refund", Concept: "refund", Reconcile: ReconcileNone,
		Inline: []Inline{{Connection: "refundShippingLines", Page: 10}}})
	if err == nil || !strings.Contains(err.Error(), "Refund.refundShippingLines") || !strings.Contains(err.Error(), "no fields") {
		t.Fatalf("an inline naming no fields was accepted, or the error does not say so: %v", err)
	}
}

// A mirrored node is a child or a reference, never a second copy inline.
func TestAnInlinedConnectionWhoseNodeIsMirroredIsRefused(t *testing.T) {
	_, err := recordedPlanWith(t,
		Entry{Type: "Order", Concept: "order", Reconcile: ReconcileNone,
			Inline: []Inline{{Connection: "lineItems", Page: 10, Fields: []string{"quantity"}}}},
		Entry{Type: "LineItem", Concept: "orderLineItem", Reconcile: ReconcileNone},
	)
	if err == nil {
		t.Fatal("an inlined connection of a mirrored node was accepted")
	}
	for _, want := range []string{"Order.lineItems", "its node LineItem is mirrored"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

// An inlined list is kept whole through a customer's erasure, and the erasure
// neither follows nor takes out a reference inside it. So an inline may not
// hold a reference to a row the erasure treats as a person: a company's
// contacts inlined with their customer would keep her gid after her erasure.
func TestAnInlinedReferenceToAPersonsRowIsRefused(t *testing.T) {
	_, err := recordedPlanWith(t,
		Entry{Type: "Company", Concept: "company", Reconcile: ReconcileNone,
			Inline: []Inline{{Connection: "contacts", Page: 10, Fields: []string{"customer"}}}},
		Entry{Type: "Customer", Concept: "customer", Reconcile: ReconcileNone},
	)
	if err == nil {
		t.Fatal("an inline holding a reference to a customer was accepted")
	}
	for _, want := range []string{"Company.contacts", "customer", "a reference to a Customer", "erasure"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	// A reference to a row that is not a person's is still held: the
	// refund lines' own line.
	if _, err := recordedPlanWith(t,
		Entry{Type: "Refund", Concept: "refund", Reconcile: ReconcileNone,
			Inline: []Inline{{Connection: "refundLineItems", Page: 10, Fields: []string{"lineItem"}}}},
		Entry{Type: "LineItem", Concept: "orderLineItem", Reconcile: ReconcileNone},
	); err != nil {
		t.Errorf("an inline holding a reference to a line item was refused: %v", err)
	}
}

func TestAConnectionNamedTwiceOnOneEntryIsRefused(t *testing.T) {
	_, err := recordedPlanWith(t, Entry{Type: "Refund", Concept: "refund", Reconcile: ReconcileNone,
		References: []string{"refundLineItems"},
		Inline:     []Inline{{Connection: "refundLineItems", Page: 10, Fields: []string{"quantity"}}}})
	if err == nil || !strings.Contains(err.Error(), "Refund.refundLineItems") || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a connection that is both a reference and inline was accepted, or the error does not say so: %v", err)
	}
}

func TestAnInlinePageOutsideOneToTwoHundredFiftyIsRefused(t *testing.T) {
	entry := func(page int) Entry {
		return Entry{Type: "Refund", Concept: "refund", Reconcile: ReconcileNone,
			Inline: []Inline{{Connection: "refundShippingLines", Page: page, Fields: []string{"subtotalAmountSet"}}}}
	}
	plans, err := recordedPlanWith(t, entry(0))
	if err != nil {
		t.Fatalf("an inline with no page was refused: %v", err)
	}
	if f := recordedField(t, plans, "Refund", "refundShippingLines"); f.Page != 25 || !strings.HasPrefix(f.Selection, "refundShippingLines(first: 25) ") {
		t.Errorf("an inline with no page reads %d (%s), want 25", f.Page, f.Selection)
	}
	for _, page := range []int{-1, 251} {
		if _, err := recordedPlanWith(t, entry(page)); err == nil || !strings.Contains(err.Error(), "Refund.refundShippingLines") {
			t.Errorf("an inline page of %d was accepted, or the error does not name the connection: %v", page, err)
		}
	}
	if _, err := recordedPlanWith(t, entry(250)); err != nil {
		t.Errorf("an inline page of 250 was refused: %v", err)
	}
}
