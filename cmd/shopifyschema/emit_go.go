// Package main -- emit_go.go: the Go side of the generated model.
//
// The connector holds no per-type code. It reads this table: which topic
// routes to which concept, which document fetches it, which mutation writes
// it, which fields map from which GraphQL paths. That is what makes the
// quarterly bump a regeneration rather than a rewrite -- a new field on
// Shopify's Order becomes a line in a table and a line in a concept, and no
// hand-written apply function has to learn about it.
package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// EmitTopicsFile renders integrations/shopify/generated/topics.go.
func (ps *PlanSet) EmitTopicsFile(version string, list *Allowlist) string {
	type route struct {
		topic, concept, action, gql string
	}
	var routes []route
	for _, p := range ps.All() {
		for topic, action := range p.Entry.Topics {
			routes = append(routes, route{topic: topic, concept: p.Concept, action: action, gql: p.GraphQLType})
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].topic < routes[j].topic })

	var b strings.Builder
	b.WriteString(goHeader(version))
	b.WriteString(`package generated

import "strings"

// TopicRoute says what a delivered webhook topic MEANS to the mirror.
//
// A webhook is a trigger, never a payload (design D2). The topic decides
// which concept is affected and whether the object is to be fetched or
// tombstoned; the object itself is then read from the Admin API with the
// generated selection set. Nothing in the apply path reads a payload field
// other than the object's own id, which is what makes the mirror faithful to
// the API rather than to the webhook's older, lossier shape.
type TopicRoute struct {
	// Topic is the WebhookSubscriptionTopic enum value.
	Topic string
	// Concept is the MemQL concept name (v1:shopify:<Concept>).
	Concept string
	// GraphQLType is the Admin type the concept mirrors.
	GraphQLType string
	// Action is ActionUpsert or ActionDelete.
	Action string
}

// The two actions a mirrored topic can carry.
const (
	ActionUpsert = "upsert"
	ActionDelete = "delete"
)

// TopicBulkOperationsFinish is not a mirrored topic: it is how Shopify tells
// the connector a bulk operation it started has finished, and it routes to
// the backfill runner instead of to a concept.
const TopicBulkOperationsFinish = "BULK_OPERATIONS_FINISH"

// Compliance topics. Declared in the app configuration rather than created
// through webhookSubscriptionCreate -- they are not members of the
// WebhookSubscriptionTopic enum -- and they route to the compliance jobs.
const (
	TopicCustomersDataRequest = "customers/data_request"
	TopicCustomersRedact      = "customers/redact"
	TopicShopRedact           = "shop/redact"
)

// ComplianceTopics is the set delivered as HTTP topic headers rather than
// enum values. A bad HMAC on any of them must answer 401.
var ComplianceTopics = []string{TopicCustomersDataRequest, TopicCustomersRedact, TopicShopRedact}

// Topics routes every mirrored WebhookSubscriptionTopic.
var Topics = map[string]TopicRoute{
`)
	for _, r := range routes {
		fmt.Fprintf(&b, "\t%q: {Topic: %q, Concept: %q, GraphQLType: %q, Action: Action%s},\n",
			r.topic, r.topic, r.concept, r.gql, title(r.action))
	}
	b.WriteString("}\n\n")

	b.WriteString(`// SubscribedTopics lists every topic EnsureSubscriptions registers, sorted
// so the reconcile diff is stable. BULK_OPERATIONS_FINISH is included: the
// connector needs its own bulk completions delivered.
var SubscribedTopics = []string{
`)
	names := make([]string, 0, len(routes)+1)
	for _, r := range routes {
		names = append(names, r.topic)
	}
	names = append(names, "BULK_OPERATIONS_FINISH")
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "\t%q,\n", n)
	}
	b.WriteString("}\n\n")

	b.WriteString(`// TopicHeaderToEnum maps the delivered X-Shopify-Topic header spelling
// ("orders/updated") onto the enum spelling ("ORDERS_UPDATED").
//
// The two spellings exist because subscriptions are created through GraphQL
// and deliveries arrive over HTTP, and Shopify does not translate between
// them for you. A connector that compares the header against the enum matches
// nothing and silently mirrors an empty store.
func TopicHeaderToEnum(header string) string {
	s := strings.ToUpper(strings.TrimSpace(header))
	s = strings.ReplaceAll(s, "/", "_")
	return strings.ReplaceAll(s, "-", "_")
}

// EnumToTopicHeader is the inverse, for tests and diagnostics. It is not
// round-trip exact for a topic whose resource name itself contains an
// underscore, which is why the header form is never used as a key.
func EnumToTopicHeader(enum string) string {
	if i := strings.Index(enum, "_"); i > 0 {
		return strings.ToLower(enum[:i]) + "/" + strings.ToLower(enum[i+1:])
	}
	return strings.ToLower(enum)
}

// RouteForHeader resolves a delivered topic header to its route.
func RouteForHeader(header string) (TopicRoute, bool) {
	r, ok := Topics[TopicHeaderToEnum(header)]
	return r, ok
}
`)
	return b.String()
}

// EmitModelFile renders integrations/shopify/generated/model.go.
func (ps *PlanSet) EmitModelFile(version string, list *Allowlist, docs map[string]string, bulkDocs map[string]string, bulkOps map[string][]string) string {
	var b strings.Builder
	b.WriteString(goHeader(version))
	b.WriteString(`package generated

import "strings"

// APIVersion is the Admin GraphQL version this tree was generated from, and
// the version every subscription, fetch and bulk operation pins. Changing it
// is a regeneration and a reviewed diff, never an env override -- a store
// answering a different version than the mirror was built for returns fields
// the concepts do not declare and omits fields they require.
const APIVersion = ` + strconv.Quote(version) + `

// Reconciliation modes, mirroring cmd/shopifyschema's allowlist.
const (
	ReconcileUpdatedAt  = "updated_at"
	ReconcileFullRelist = "full_relist"
	ReconcileNone       = "none"
)

// FieldSpec maps one mirrored field: the MemQL name the mutation takes, the
// Admin GraphQL field it came from, and enough of its shape for the apply
// path to render a literal of the right type.
type FieldSpec struct {
	// Name is the mutation argument and concept field name.
	Name string
	// GraphQL is the field name in the fetched JSON.
	GraphQL string
	// Kind is one of the Kind* constants.
	Kind string
	// DSLType is the concept's declared type, e.g. "string", "[]string".
	DSLType string
	// Extract is where the value lives in the fetched JSON, in the small
	// path language ExtractField understands: "field", "field.id",
	// "field[].id", "field.nodes[].id", "field.nodes[]".
	Extract string
}

// Field kinds.
const (
	KindScalar     = "scalar"
	KindEnum       = "enum"
	KindObject     = "object"
	KindObjectList = "objectList"
	KindScalarList = "scalarList"
	KindRefs       = "refs"
	KindMetafields = "metafields"
	KindGid        = "gid"
)

// ChildSpec is a data-carrying connection materialised as its own concept.
type ChildSpec struct {
	// Connection is the field name on the parent's fetched JSON.
	Connection string
	// Concept is the child's MemQL concept.
	Concept string
	// GraphQLType is the child's Admin type.
	GraphQLType string
	// List is set when the parent's field is a plain array rather than a
	// Relay connection: the apply path reads it directly instead of
	// through "nodes" / "edges".
	List bool
}

// TypeSpec is everything the connector knows about one mirrored domain.
type TypeSpec struct {
	Concept     string
	GraphQLType string

	// ByGidFn / ForStoreFn are the generated reads. There are no
	// generated WRITES: the runtime performs mirror writes from the
	// MirrorWrites this connector returns. See emit_dsl.go.
	ByGidFn    string
	ForStoreFn string
	// Shape is the concept's default projection, which both reads use.
	Shape string

	// ListQuery is the QueryRoot connection the domain is listed through,
	// empty for a singleton or a child-only type. Singleton names the
	// QueryRoot field that returns the one object.
	ListQuery string
	Singleton string
	// Fetchable says the type implements Node, so node(id:) resolves it.
	// A type that does not is only ever reached through its parent or a
	// bulk stream -- and a webhook naming it has nothing to fetch.
	Fetchable bool

	Bulk      bool
	Reconcile string
	Cadence   string
	Scopes    []string

	// Parent / ParentConnection are set on a materialised child.
	Parent           string
	ParentConnection string
	// ParentGidPath is where a DIRECTLY fetched child finds its parent's
	// GID in the fetched JSON (e.g. "order.id"). Empty when the child is
	// only ever materialised through its parent.
	ParentGidPath string
	Children      []ChildSpec

	// VersionField is the Admin field the apply guard compares -- the
	// origin's own version. Empty means the type carries neither updatedAt
	// nor createdAt, so the guard falls back to the fetch time and the
	// domain is effectively last-write-wins.
	VersionField string

	Fields []FieldSpec

	// FetchDocument holds both generated operations: ShopifyFetch<Type>
	// (by GID, or the singleton) and ShopifyList<Type> (paged, filterable).
	FetchDocument string
	FetchOp       string
	ListOp        string

	// BulkDocument holds every bulk operation for the domain, and BulkOps
	// names them in the order the runner must run them.
	BulkDocument string
	BulkOps      []string
}

// FieldByGraphQL resolves a field spec by its Admin GraphQL name.
func (t *TypeSpec) FieldByGraphQL(name string) (FieldSpec, bool) {
	for _, f := range t.Fields {
		if f.GraphQL == name {
			return f, true
		}
	}
	return FieldSpec{}, false
}

// Types is every mirrored domain, keyed by concept.
var Types = map[string]*TypeSpec{}

// ApplyOrder lists concepts parents-first. A backfill or a full re-list runs
// in this order so a child never lands before the parent its parentGid names.
var ApplyOrder = []string{
`)
	for _, p := range ps.ParentsFirst() {
		fmt.Fprintf(&b, "\t%q,\n", p.Concept)
	}
	b.WriteString("}\n\n")

	b.WriteString("// Scopes is the union of every domain's Admin access scopes: the scope list\n")
	b.WriteString("// the app must be granted, which the runbook prints and the portal checks a\n")
	b.WriteString("// store's grant against.\nvar Scopes = []string{\n")
	for _, s := range list.AllScopes() {
		fmt.Fprintf(&b, "\t%q,\n", s)
	}
	b.WriteString("}\n\n")

	b.WriteString("// PollingOnlyConcepts are the domains Shopify publishes no webhook topic\n")
	b.WriteString("// for. They are reconciled by a scheduled full re-list, and the cadence on\n")
	b.WriteString("// each TypeSpec is what bounds their drift window.\nvar PollingOnlyConcepts = []string{\n")
	for _, p := range ps.ParentsFirst() {
		if p.Entry.Reconcile == ReconcileFullRelist {
			fmt.Fprintf(&b, "\t%q,\n", p.Concept)
		}
	}
	b.WriteString("}\n\n")

	b.WriteString("func init() {\n")
	for _, p := range ps.All() {
		b.WriteString(emitTypeSpec(p, docs[p.Concept], bulkDocs[p.Concept], bulkOps[p.Concept]))
	}
	b.WriteString("}\n\n")

	b.WriteString(`// ConceptID is the canonical id of a mirrored concept.
func ConceptID(concept string) string { return "v1:shopify:" + concept }

// ConceptFromID is the inverse, for a caller holding a canonical id.
func ConceptFromID(id string) string { return strings.TrimPrefix(id, "v1:shopify:") }
`)
	return b.String()
}

func emitTypeSpec(p *TypePlan, doc, bulkDoc string, bulkOps []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\tTypes[%q] = &TypeSpec{\n", p.Concept)
	fmt.Fprintf(&b, "\t\tConcept:     %q,\n", p.Concept)
	fmt.Fprintf(&b, "\t\tGraphQLType: %q,\n", p.GraphQLType)
	fmt.Fprintf(&b, "\t\tByGidFn:     %q,\n", byGidName(p))
	fmt.Fprintf(&b, "\t\tForStoreFn:  %q,\n", forStoreName(p))
	fmt.Fprintf(&b, "\t\tShape:       %q,\n", shapeName(p))
	fmt.Fprintf(&b, "\t\tListQuery:   %q,\n", p.Entry.Query)
	fmt.Fprintf(&b, "\t\tSingleton:   %q,\n", p.Entry.Singleton)
	fmt.Fprintf(&b, "\t\tFetchable:   %t,\n", p.Fetchable())
	fmt.Fprintf(&b, "\t\tBulk:        %t,\n", p.Entry.Bulk)
	fmt.Fprintf(&b, "\t\tReconcile:   %q,\n", p.Entry.Reconcile)
	fmt.Fprintf(&b, "\t\tCadence:     %q,\n", p.Entry.Cadence)
	if len(p.Entry.Scopes) > 0 {
		fmt.Fprintf(&b, "\t\tScopes:      []string{%s},\n", quoteList(p.Entry.Scopes))
	}
	if p.ParentConcept != "" {
		fmt.Fprintf(&b, "\t\tParent:           %q,\n", p.ParentConcept)
		fmt.Fprintf(&b, "\t\tParentConnection: %q,\n", p.ParentConnection)
		fmt.Fprintf(&b, "\t\tParentGidPath:    %q,\n", p.ParentGidPath)
	}
	fmt.Fprintf(&b, "\t\tVersionField: %q,\n", p.VersionField)
	if len(p.Children) > 0 {
		b.WriteString("\t\tChildren: []ChildSpec{\n")
		for _, ch := range p.Children {
			fmt.Fprintf(&b, "\t\t\t{Connection: %q, Concept: %q, GraphQLType: %q, List: %t},\n", ch.Connection, ch.Concept, ch.Type, ch.List)
		}
		b.WriteString("\t\t},\n")
	}
	b.WriteString("\t\tFields: []FieldSpec{\n")
	for _, f := range p.Fields {
		fmt.Fprintf(&b, "\t\t\t{Name: %q, GraphQL: %q, Kind: %q, DSLType: %q, Extract: %q},\n", f.Name, f.GraphQL, goKindConst(f.Kind), f.DSLType, f.Extract)
	}
	b.WriteString("\t\t},\n")
	fmt.Fprintf(&b, "\t\tFetchOp:       %q,\n", "ShopifyFetch"+title(p.Concept))
	if p.Entry.Query != "" {
		fmt.Fprintf(&b, "\t\tListOp:        %q,\n", "ShopifyList"+title(p.Concept))
	}
	fmt.Fprintf(&b, "\t\tFetchDocument: %s,\n", backquote(doc))
	if bulkDoc != "" {
		fmt.Fprintf(&b, "\t\tBulkDocument:  %s,\n", backquote(bulkDoc))
		fmt.Fprintf(&b, "\t\tBulkOps:       []string{%s},\n", quoteList(bulkOps))
	}
	b.WriteString("\t}\n")
	return b.String()
}

// goKindConst renders a kind as its literal value; the constants exist for
// the reader, and the emitted table uses the string so the file stays
// diffable without resolving identifiers.
func goKindConst(kind string) string { return kind }

func quoteList(in []string) string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strconv.Quote(s))
	}
	return strings.Join(out, ", ")
}

// backquote renders a GraphQL document as a Go raw string. A document can
// never contain a backquote -- GraphQL has no such token outside a string,
// and no generated selection contains a string -- so the escape hatch is an
// assertion rather than a code path.
func backquote(s string) string {
	if strings.Contains(s, "`") {
		return strconv.Quote(s)
	}
	return "`" + s + "`"
}

func goHeader(version string) string {
	return `// Code generated by cmd/shopifyschema from Admin GraphQL ` + version + `. DO NOT EDIT.
//
// Regenerate with:  go run ./cmd/shopifyschema
// The allowlist that decides what is here: cmd/shopifyschema/allowlist.yaml

`
}
