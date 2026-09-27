// Package main -- mapping.go: the rules of spec 4.2, one function each.
//
// Every rule here is a decision about FIDELITY versus CHURN. Shopify ships a
// new API version every quarter and adds fields inside a version's lifetime;
// a mirror that over-constrains refuses to write the day a value it has never
// seen arrives, and a mirror that under-constrains cannot be queried. The
// split this file draws: the SELECTION SET carries the fidelity (it names
// every field we fetch, at full depth), and the CONCEPT carries only as much
// structure as a read needs.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// Field kinds, as the emitters distinguish them.
const (
	KindScalar     = "scalar"
	KindEnum       = "enum"
	KindObject     = "object"
	KindObjectList = "objectList"
	KindScalarList = "scalarList"
	KindRefs       = "refs"       // a connection kept as a []string of GIDs
	KindMetafields = "metafields" // the namespace.key map
	KindGid        = "gid"        // a single mirrored object, kept as its GID
)

// maxInlineEnumValues bounds a closed enum in a generated concept.
//
// The rule exists because two of Shopify's enums are catalogues rather than
// states: CurrencyCode carries about 170 members and CountryCode about 260.
// Inlining those would put a thousand string literals in the tree for no
// read that wants them, and would make the quarterly bump a diff nobody can
// review. Below the bound the enum is CLOSED, which is what makes a status
// field worth filtering on; above it the field is a string and the doc
// comment names the schema enum so the constraint is still discoverable.
const maxInlineEnumValues = 40

// refsPageSize and childPageSize bound the nested selections of a
// webhook-triggered fetch. Bulk operations page for themselves, so these
// only apply on the fetch path, where the whole query has to stay under the
// 1,000-point single-query cost ceiling.
const (
	refsPageSize      = 25
	childPageSize     = 100
	metafieldPageSize = 50
)

// mirrorOwnFields are the columns every mirror row carries. A Shopify field
// of the same name is renamed rather than dropped -- losing a field silently
// is the one outcome the generator must not have.
var mirrorOwnFields = map[string]bool{
	"storeId": true, "gid": true, "updatedAt": true, "syncedAt": true,
	"deleted": true, "parentGid": true,
}

// engineReservedFields mirrors component/database/memory-nodes'
// reservedPayloadFields. It is duplicated here rather than imported because
// cmd/ tools in this module may not depend on the database package, and
// because the generator has to know the rule at EMIT time; the drift between
// the two lists is caught the moment the generated tree fails to load, which
// the generator's own load test does on every run.
// memqlKeywords cannot be a field or argument name: the parser reads them as
// language tokens wherever an identifier is expected, so a Shopify field
// called `default` or `return` does not merely warn, it fails the file.
var memqlKeywords = map[string]bool{
	"func": true, "for": true, "range": true, "if": true, "else": true,
	"switch": true, "case": true, "default": true, "continue": true,
	"break": true, "return": true, "nil": true, "retry": true, "when": true,
	"as": true, "where": true, "use": true, "import": true, "in": true,
	"has": true, "not": true, "startswith": true, "true": true, "false": true,
	"null": true,
}

var engineReservedFields = map[string]bool{
	"id": true, "createdat": true, "createdby": true, "concept": true,
	"partition": true, "payload": true, "schema": true, "type": true,
	"provenance": true, "row": true, "actor": true, "args": true,
	"now": true, "config": true, "trace": true, "meta": true,
}

// FieldPlan is one mirrored field: what it is called in MemQL, where it came
// from in GraphQL, how it is declared, and how it is selected.
type FieldPlan struct {
	Name      string // the MemQL concept field and mutation argument name
	GraphQL   string // the Admin GraphQL field name
	DSLType   string // as rendered into the concept
	Kind      string
	Required  bool
	Doc       string
	Selection string // the GraphQL selection text, "" for a plain leaf
	// SelectionBulk is the same field inside a BULK operation, where a
	// connection takes no pagination argument and is spelled through
	// edges/node. Empty means the fetch selection is reused as is.
	SelectionBulk string
	// OmitFromBulk keeps the field out of the bulk document altogether.
	// Set on every reference connection: each nested connection counts
	// against a bulk query's budget, and what it carries is refilled by
	// the paged list operation on the first reconcile after a backfill.
	OmitFromBulk bool
	// Extract is where the value lives in the fetched JSON, in the small
	// path language the connector's applier understands: "field",
	// "field.id", "field[].id", "field.nodes[].id".
	Extract string
}

// ChildPlan is a data-carrying connection materialised as its own concept.
type ChildPlan struct {
	Connection string
	Type       string
	Concept    string
	Page       int
	// List is set when the parent's field is a plain LIST rather than a
	// Relay connection. Shopify models three of an order's most important
	// children that way -- transactions, refunds and fulfilments -- so the
	// generator has to render both spellings or lose them.
	List bool
}

// TypePlan is everything the emitters need about one allowlisted type.
type TypePlan struct {
	Entry            *Entry
	GraphQLType      string
	Concept          string
	Fields           []FieldPlan
	Children         []ChildPlan
	ParentType       string
	ParentConcept    string
	ParentConnection string
	IsInterface      bool
	// ImplementsNode says the type is reachable through `node(id:)`. A
	// mirrored type that is not is only ever materialised with its parent
	// or streamed by a bulk operation -- so a webhook naming it has nothing
	// to fetch, and the apply path has to re-fetch the PARENT instead.
	ImplementsNode bool
	// HasID says the type declares an `id` field, which is where a mirror
	// row's gid comes from. Not every mirrored type does: a price list's
	// prices and quantity rules, and ShopLocale, are keyed by their parent
	// and a variant or locale instead. Selecting `id` on one of those is
	// a field the schema does not have, and the whole document is refused.
	HasID bool
	// ListFilterable says the root connection accepts `query:`, so the
	// list document can declare $query and an updated_at sweep can pass
	// `updated_at:>`. Seven root connections do not (channels, publications,
	// themes, ...), and for those a declared-but-unaccepted $query is a
	// validation error on every page, not an ignored variable.
	ListFilterable bool
	// VersionField is the Admin field the apply guard compares. Most types
	// carry updatedAt; some carry only createdAt; a handful carry neither,
	// and for those the guard degrades to the fetch time -- which is
	// last-write-wins, and is why the field is recorded rather than assumed.
	VersionField string
	// ParentGidPath is where a DIRECTLY fetched child finds its parent's
	// GID (e.g. "order.id"). Empty when the child is only ever materialised
	// through its parent's selection, where the parent GID is already known.
	ParentGidPath string
}

// Fetchable reports whether one row of this type can be read by GID.
func (p *TypePlan) Fetchable() bool { return p.ImplementsNode || p.Entry.Singleton != "" }

// Planner turns the schema plus the allowlist into TypePlans.
type Planner struct {
	schema *Schema
	list   *Allowlist
}

// NewPlanner builds a planner.
func NewPlanner(s *Schema, a *Allowlist) *Planner { return &Planner{schema: s, list: a} }

// Plan resolves every allowlisted type, in concept order.
func (p *Planner) Plan() ([]*TypePlan, error) {
	var out []*TypePlan
	for _, e := range p.list.Sorted() {
		tp, err := p.planType(e)
		if err != nil {
			return nil, err
		}
		out = append(out, tp)
	}
	return out, nil
}

func (p *Planner) planType(e *Entry) (*TypePlan, error) {
	t, err := p.schema.MustLookup(e.Type)
	if err != nil {
		return nil, err
	}
	if t.Kind != "OBJECT" && t.Kind != "INTERFACE" {
		return nil, fmt.Errorf("%s is a %s, and only an object or interface can be mirrored", e.Type, t.Kind)
	}
	plan := &TypePlan{
		Entry:       e,
		GraphQLType: e.Type,
		Concept:     e.Concept,
		IsInterface: t.Kind == "INTERFACE",
	}
	for _, i := range t.Interfaces {
		if i.Named() == "Node" {
			plan.ImplementsNode = true
		}
	}
	plan.HasID = t.Field("id") != nil
	if e.Query != "" {
		if err := p.planListing(plan, e); err != nil {
			return nil, err
		}
	}
	if parentType, conn := e.Parent(); parentType != "" {
		plan.ParentType = parentType
		plan.ParentConnection = conn
		if pe := p.list.Entry(parentType); pe != nil {
			plan.ParentConcept = pe.Concept
		}
	}

	childByConnection := map[string]Child{}
	for _, ch := range e.Children {
		childByConnection[ch.Connection] = ch
		page := ch.Page
		if page == 0 {
			page = childPageSize
		}
		childEntry := p.list.Entry(ch.Type)
		field := t.Field(ch.Connection)
		if field == nil {
			return nil, fmt.Errorf("%s has no field %q to materialise %s from", e.Type, ch.Connection, ch.Type)
		}
		named := field.Type.Named()
		isList := !strings.HasSuffix(named, "Connection")
		if isList && named != ch.Type {
			return nil, fmt.Errorf("%s.%s is a %s, not a connection of %s nor a list of it", e.Type, ch.Connection, named, ch.Type)
		}
		if !isList && named != ch.Type+"Connection" {
			return nil, fmt.Errorf("%s.%s is a %s, which is not the connection of %s", e.Type, ch.Connection, named, ch.Type)
		}
		plan.Children = append(plan.Children, ChildPlan{
			Connection: ch.Connection, Type: ch.Type,
			Concept: childEntry.Concept, Page: page, List: isList,
		})
	}
	refs := map[string]bool{}
	for _, r := range e.References {
		refs[r] = true
	}
	skip := map[string]bool{}
	for _, s := range e.Skip {
		skip[s] = true
	}

	taken := map[string]bool{}
	for _, f := range t.Fields {
		if f.Name == "id" || skip[f.Name] {
			continue // the GID is the row's identity, carried as `gid`
		}
		// The origin's updatedAt IS the mirror's updatedAt column -- the
		// value the apply guard compares. Carrying it a second time as a
		// payload field would be the same fact in two places, and the two
		// would disagree the moment a write took one path and not the
		// other.
		if f.Name == "updatedAt" {
			continue
		}
		// A DEPRECATED field is omitted in favour of its replacement
		// (spec 4.2's last row). The replacement is a separate, live field
		// of the same type, so nothing is lost by dropping the old name --
		// and keeping it would mirror a value Shopify has already stopped
		// maintaining.
		if f.IsDeprecated {
			continue
		}
		if requiresArgument(&f) {
			continue // cannot be selected without a caller-supplied value
		}
		if childByConnection[f.Name].Connection != "" {
			continue // materialised as its own concept
		}
		fp, ok, err := p.planField(&f, refs[f.Name])
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", e.Type, f.Name, err)
		}
		if !ok {
			continue
		}
		if fp.Extract == "" {
			fp.Extract = f.Name
		}
		fp.Name = mirrorFieldName(f.Name, taken)
		if fp.Kind == KindGid && !strings.HasSuffix(fp.Name, "Gid") {
			fp.Name += "Gid"
		}
		if fp.Kind == KindRefs && strings.HasSuffix(fp.Extract, "[].id") && !strings.HasSuffix(fp.Name, "Gids") {
			fp.Name += "Gids"
		}
		taken[strings.ToLower(fp.Name)] = true
		plan.Fields = append(plan.Fields, fp)
	}
	sort.SliceStable(plan.Fields, func(i, j int) bool { return plan.Fields[i].Name < plan.Fields[j].Name })

	// The version the apply guard compares. Preference order is a fact
	// about the schema, not a choice: updatedAt is the origin's own
	// version; createdAt is the best available for an immutable record;
	// neither means the guard has nothing to compare and the fetch time
	// stands in.
	switch {
	case t.Field("updatedAt") != nil && !t.Field("updatedAt").IsDeprecated:
		plan.VersionField = "updatedAt"
	case t.Field("createdAt") != nil && !t.Field("createdAt").IsDeprecated:
		plan.VersionField = "createdAt"
	}
	if e.Reconcile == ReconcileUpdatedAt && plan.VersionField != "updatedAt" {
		return nil, fmt.Errorf("%s is set to reconcile by updated_at, but the type has no updatedAt field -- use %s with a cadence",
			e.Type, ReconcileFullRelist)
	}

	// Where a directly-fetched child finds its parent. Derived rather than
	// authored: the back-reference is a field of the child's own type, so
	// the schema already says whether one exists.
	if plan.ParentType != "" {
		for _, f := range t.Fields {
			if f.IsDeprecated || requiresArgument(&f) {
				continue
			}
			if f.Type.Named() == plan.ParentType && !f.Type.IsList() {
				plan.ParentGidPath = f.Name + ".id"
				break
			}
		}
	}
	return plan, nil
}

// listArguments are the arguments the generated list document supplies
// (first, after, query) or that Shopify always defaults (reverse, sortKey).
// A root connection requiring anything else -- metafieldDefinitions
// (ownerType:), metaobjects(type:) -- cannot be listed generically, and
// refusing it here is what stops the runtime finding that out from a
// missingRequiredArguments error on every sweep.
var listArguments = map[string]bool{
	"first": true, "after": true, "query": true, "reverse": true, "sortKey": true,
}

// planListing checks the QueryRoot connection an entry is listed through and
// records whether it can be filtered. Both facts come from the schema, not
// the allowlist: which arguments a connection takes is Shopify's decision.
func (p *Planner) planListing(plan *TypePlan, e *Entry) error {
	rootName := p.schema.QueryType
	if rootName == "" {
		rootName = "QueryRoot"
	}
	root, err := p.schema.MustLookup(rootName)
	if err != nil {
		return err
	}
	field := root.Field(e.Query)
	if field == nil {
		return fmt.Errorf("%s: %s has no field %q to list it through", e.Type, rootName, e.Query)
	}
	for _, a := range field.Args {
		if a.Type.NonNull() && a.DefaultValue == "" && !listArguments[a.Name] {
			return fmt.Errorf("%s: %s.%s requires the argument %q, which a generic listing cannot supply -- drop `query` and set reconcile: %s (listing per %s is future work)",
				e.Type, rootName, e.Query, a.Name, ReconcileNone, a.Name)
		}
	}
	plan.ListFilterable = field.HasArg("query")
	if e.Reconcile == ReconcileUpdatedAt && !plan.ListFilterable {
		return fmt.Errorf("%s is set to reconcile by updated_at, but %s.%s accepts no `query` argument to carry the filter -- use %s with a cadence",
			e.Type, rootName, e.Query, ReconcileFullRelist)
	}
	return nil
}

// referencedByGid reports whether a nested type is kept as a GID reference:
// it is mirrored in its own concept AND declares the `id` the reference
// would carry. A mirrored type without an id has no GID to keep, so it is
// inlined like any other nested shape -- `{ id }` on it is a field the
// schema does not have, and the whole document is refused.
func (p *Planner) referencedByGid(t *Type) bool {
	return t != nil && p.list.Entry(t.Name) != nil && t.Field("id") != nil
}

// connectionNodeType resolves what a Relay connection is a connection OF,
// through its `nodes` field. The naming convention is the fallback for a
// connection type the fixture happened to prune.
func (p *Planner) connectionNodeType(connName string) *Type {
	if conn := p.schema.Lookup(connName); conn != nil {
		if nodes := conn.Field("nodes"); nodes != nil {
			if t := p.schema.Lookup(nodes.Type.Named()); t != nil {
				return t
			}
		}
	}
	return p.schema.Lookup(strings.TrimSuffix(connName, "Connection"))
}

// requiresArgument reports whether a field cannot be selected without the
// caller supplying something -- `metafield(namespace:, key:)` and the like.
func requiresArgument(f *Field) bool {
	for _, a := range f.Args {
		if a.Type.NonNull() && a.DefaultValue == "" {
			return true
		}
	}
	return false
}

// mirrorFieldName renames a Shopify field that would collide with a row
// intrinsic or with a mirror column. `createdAt` becomes `sourceCreatedAt`:
// the row has its own createdAt (when MemQL first saw it) and Shopify's is a
// different fact, so the rename is a clarification rather than a workaround.
func mirrorFieldName(name string, taken map[string]bool) string {
	out := name
	if engineReservedFields[strings.ToLower(name)] || mirrorOwnFields[name] || memqlKeywords[strings.ToLower(name)] {
		out = "source" + strings.ToUpper(name[:1]) + name[1:]
	}
	for taken[strings.ToLower(out)] {
		out += "Field"
	}
	return out
}

// planField applies the mapping table to one field.
func (p *Planner) planField(f *Field, isRef bool) (FieldPlan, bool, error) {
	named := f.Type.Named()
	target := p.schema.Lookup(named)
	fp := FieldPlan{GraphQL: f.Name, Required: f.Type.NonNull()}

	// A connection is the one shape whose MemQL form depends on the
	// allowlist rather than on the schema: data-carrying connections became
	// child concepts before we got here, so what is left is either a
	// declared reference list or nothing at all.
	// A connection whose node type is itself mirrored, and a plain object
	// field whose type is mirrored, are both REFERENCES: the row lives in
	// its own concept, so carrying a second copy inline would be two
	// versions of the same fact, drifting apart between fetches. Keep the
	// GID and let the reads join.
	if strings.HasSuffix(named, "Connection") {
		if named == "MetafieldConnection" {
			fp.Kind = KindMetafields
			fp.DSLType = "object"
			fp.Required = false
			fp.Doc = "Merchant-owned and own-app metafields, keyed \"namespace.key\". App-owned metafields of OTHER apps are not visible to anyone (2025-05-19)."
			fp.Selection = fmt.Sprintf("%s(first: %d) { nodes { namespace key type value jsonValue } }", f.Name, metafieldPageSize)
			fp.SelectionBulk = f.Name + " { edges { node { namespace key type value jsonValue } } }"
			fp.Extract = f.Name + ".nodes[]"
			return fp, true, nil
		}
		if !isRef {
			return fp, false, nil
		}
		// Deliberately absent from bulk either way (see OmitFromBulk).
		fp.OmitFromBulk = true
		fp.Required = false
		node := p.connectionNodeType(named)
		if node != nil && node.Field("id") == nil {
			// The node type has no id to keep -- ResourcePublication,
			// DiscountApplication -- so the rows come inline instead.
			sel := p.objectSelection(node, 0)
			if sel == "" {
				return fp, false, nil
			}
			fp.Kind = KindObjectList
			fp.DSLType = "[]object"
			fp.Doc = fmt.Sprintf("Inline %s rows (reference connection, first %d): the type has no id to keep a GID of.", node.Name, refsPageSize)
			fp.Selection = fmt.Sprintf("%s(first: %d) { nodes %s }", f.Name, refsPageSize, sel)
			fp.Extract = f.Name + ".nodes[]"
			return fp, true, nil
		}
		fp.Kind = KindRefs
		fp.DSLType = "[]string"
		fp.Doc = fmt.Sprintf("GIDs only (reference connection, first %d).", refsPageSize)
		fp.Selection = fmt.Sprintf("%s(first: %d) { nodes { id } }", f.Name, refsPageSize)
		fp.Extract = f.Name + ".nodes[].id"
		return fp, true, nil
	}

	// A mirrored type referenced as a plain object (Order.customer) or as a
	// list of them: keep the GID only. A mirrored type WITHOUT an id
	// (ShopLocale) falls through to the nested-object rule below and is
	// inlined -- there is no GID to keep.
	if target != nil && (target.Kind == "OBJECT" || target.Kind == "INTERFACE") && p.referencedByGid(target) {
		if f.Type.IsList() {
			fp.Kind = KindRefs
			fp.DSLType = "[]string"
			fp.Required = false
			fp.Doc = fmt.Sprintf("GIDs of the mirrored %s rows.", named)
			fp.Selection = f.Name + " { id }"
			fp.SelectionBulk = fp.Selection
			fp.Extract = f.Name + "[].id"
			return fp, true, nil
		}
		fp.Kind = KindGid
		fp.DSLType = "string"
		fp.Required = false
		fp.Doc = fmt.Sprintf("GID of the mirrored %s row.", named)
		fp.Selection = f.Name + " { id }"
		fp.SelectionBulk = fp.Selection
		fp.Extract = f.Name + ".id"
		return fp, true, nil
	}

	switch {
	case target == nil:
		return fp, false, nil

	case target.Kind == "SCALAR":
		base := scalarToDSL(named)
		if f.Type.IsList() {
			fp.Kind = KindScalarList
			fp.DSLType = "[]" + base
			fp.Required = false
		} else {
			fp.Kind = KindScalar
			fp.DSLType = base
		}
		fp.Selection = f.Name
		return fp, true, nil

	case target.Kind == "ENUM":
		values := liveEnumValues(target)
		if f.Type.IsList() {
			fp.Kind = KindScalarList
			fp.DSLType = "[]string"
			fp.Required = false
			fp.Doc = fmt.Sprintf("Admin GraphQL enum %s.", named)
		} else if len(values) > 0 && len(values) <= maxInlineEnumValues {
			fp.Kind = KindEnum
			fp.DSLType = renderEnum(values)
			// A closed enum with a NON_NULL schema type still has to
			// tolerate absence: the mirror writes what the fetch returned,
			// and an unapproved protected-data scope makes Shopify answer
			// null for a field its own schema calls non-null.
			fp.Required = false
		} else {
			fp.Kind = KindScalar
			fp.DSLType = "string"
			fp.Doc = fmt.Sprintf("Admin GraphQL enum %s (%d values -- carried as a string, see maxInlineEnumValues).", named, len(values))
		}
		fp.Selection = f.Name
		return fp, true, nil

	case target.Kind == "OBJECT" || target.Kind == "INTERFACE" || target.Kind == "UNION":
		sel := p.objectSelection(target, 0)
		if sel == "" {
			return fp, false, nil // nothing selectable: omit rather than emit an invalid query
		}
		if f.Type.IsList() {
			fp.Kind = KindObjectList
			fp.DSLType = "[]object"
			fp.Required = false
		} else {
			fp.Kind = KindObject
			fp.DSLType = "object"
			fp.Required = false
		}
		if moneyShape(named) != "" {
			fp.Doc = "Money: " + moneyShape(named) + "."
		}
		fp.Selection = f.Name + " " + sel
		return fp, true, nil
	}
	return fp, false, nil
}

// objectSelection renders `{ ... }` for a nested object, interface or union.
//
// Depth is bounded at two levels below the root field. That is not an
// arbitrary cap: it is the depth at which Shopify's own object graph stops
// being a shape and starts being a traversal -- MailingAddress and MoneyBag
// are shapes, `Order.customer.lastOrder.customer` is a traversal, and a
// mirror that follows one would fetch the store through a single field.
func (p *Planner) objectSelection(t *Type, depth int) string {
	if t == nil {
		return ""
	}
	// Money is rendered whole at any depth. It is the one nested shape a
	// read actually needs -- a line item's price two levels down is the
	// number every analytics query sums -- and the depth bound below would
	// otherwise drop the amount and leave an empty selection behind.
	switch t.Name {
	case "MoneyV2":
		return "{ amount currencyCode }"
	case "MoneyBag":
		return "{ shopMoney { amount currencyCode } presentmentMoney { amount currencyCode } }"
	}
	if depth > 1 {
		return ""
	}
	var parts []string
	if t.Kind == "INTERFACE" || t.Kind == "UNION" {
		parts = append(parts, "__typename")
	}
	// A UNION has no fields of its own, so `{ __typename }` is all a naive
	// walk produces -- and that mirrors the discriminator while losing
	// everything it discriminates. Spec 4.2 asks for the per-member
	// selection, which in GraphQL is an inline fragment per possible type.
	if t.Kind == "UNION" {
		for _, pt := range t.PossibleTypes {
			member := p.schema.Lookup(pt.Named())
			if member == nil {
				continue
			}
			// A member that is itself MIRRORED is a reference, exactly as
			// a mirrored object field is: inlining it would put a whole
			// second row inside this one, and the two copies would drift
			// apart between fetches. (Only when it has an id to refer by.)
			if p.referencedByGid(member) {
				parts = append(parts, "... on "+member.Name+" { id }")
				continue
			}
			sub := p.objectSelection(member, depth+1)
			if sub == "" {
				// A member reached at the depth bound still keeps its own
				// scalars: a PricingValue that is a percentage is
				// `{ percentage }`, and a `{ __typename }` alone would name
				// the kind of value while dropping the value.
				sub = p.leafSelection(member)
			}
			if sub != "" {
				parts = append(parts, "... on "+member.Name+" "+sub)
			}
		}
		if len(parts) == 1 {
			return "{ __typename }"
		}
		return "{ " + strings.Join(parts, " ") + " }"
	}
	for _, f := range t.Fields {
		if f.IsDeprecated || requiresArgument(&f) {
			continue
		}
		named := f.Type.Named()
		if strings.HasSuffix(named, "Connection") {
			continue // a nested connection is a traversal, never a shape
		}
		target := p.schema.Lookup(named)
		if target == nil {
			continue
		}
		switch target.Kind {
		case "SCALAR", "ENUM":
			parts = append(parts, f.Name)
		case "OBJECT", "INTERFACE", "UNION":
			// A nested object whose type is MIRRORED is a reference, and
			// following it here would inline a whole second row -- an
			// order's customer, that customer's last order, and so on
			// down. Keep its id; the row is mirrored in its own concept.
			// A mirrored type with no id is inlined like anything else.
			if p.referencedByGid(target) {
				parts = append(parts, f.Name+" { id }")
				continue
			}
			if sub := p.objectSelection(target, depth+1); sub != "" {
				parts = append(parts, f.Name+" "+sub)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "{ " + strings.Join(parts, " ") + " }"
}

// leafSelection is a type's scalar and enum fields and nothing nested: the
// selection a union member gets at the depth bound.
func (p *Planner) leafSelection(t *Type) string {
	var parts []string
	for _, f := range t.Fields {
		if f.IsDeprecated || requiresArgument(&f) {
			continue
		}
		target := p.schema.Lookup(f.Type.Named())
		if target == nil {
			continue
		}
		if target.Kind == "SCALAR" || target.Kind == "ENUM" {
			parts = append(parts, f.Name)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "{ " + strings.Join(parts, " ") + " }"
}

// moneyShape names the money shape a type carries, for the doc comment.
func moneyShape(named string) string {
	switch named {
	case "MoneyV2":
		return "{amount, currencyCode}"
	case "MoneyBag":
		return "{shopMoney, presentmentMoney}, each {amount, currencyCode}"
	}
	return ""
}

// liveEnumValues lists the non-deprecated members of an enum.
func liveEnumValues(t *Type) []string {
	out := make([]string, 0, len(t.EnumValues))
	for _, v := range t.EnumValues {
		if v.IsDeprecated {
			continue
		}
		out = append(out, v.Name)
	}
	return out
}

func renderEnum(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, `"`+v+`"`)
	}
	return "enum(" + strings.Join(quoted, ", ") + ")"
}

// scalarToDSL maps a GraphQL scalar onto a MemQL field type.
//
// Everything Shopify calls a custom scalar -- Money, Decimal, HTML, URL,
// UnsignedInt64, StorefrontID -- serialises as a JSON string, so string is
// the faithful mapping and not a fallback. DateTime is the one that earns a
// distinct type, because the engine validates RFC3339 and the reads window
// on it.
func scalarToDSL(named string) string {
	switch named {
	case "Boolean":
		return "bool"
	case "Int":
		return "int"
	case "Float":
		return "float"
	case "DateTime":
		// A MIRRORED timestamp is a string, and this is the one mapping
		// that looks like a downgrade and is not.
		//
		// Every generated write stamps EVERY field, so that a value the
		// origin CLEARED is cleared here too rather than merged forward
		// from the previous fetch (see emitUpsert). Stamping needs a
		// literal the field's type accepts for "the origin returned
		// null", and there is no empty datetime -- `datetime` means
		// RFC3339 and "" is not one. So a nullable Shopify timestamp
		// would force a choice between losing the clear and failing the
		// write.
		//
		// Nothing is lost by carrying it as a string: Shopify returns
		// RFC3339 in UTC, which sorts chronologically as text, so the
		// windowed reads compare exactly as they would on a datetime.
		// The mirror's OWN updatedAt and syncedAt stay `datetime` --
		// they are always written, so they never need the empty case.
		return "string"
	case "JSON":
		return "any"
	default:
		return "string"
	}
}
