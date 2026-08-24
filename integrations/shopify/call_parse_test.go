package shopify

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// call_parse_test.go -- every MemQL call this connector builds, through the
// REAL front end (the memql#4256 class).
//
// # Why this file exists at all
//
// Every other test in this package runs against a fake engine that RECORDS
// the call string and parses nothing. That is exactly the coverage shape
// which let five guest mutations ship rendering a form the parser had
// rejected since memql#2335: their unit tests were green, and every write
// failed at parse in production. component/grpc's
// render_query_args_parse_test.go is the same guard for that defect, and this
// is its counterpart here.
//
// # Three levels, and the third is the one people skip
//
//  1. SYNTAX -- the text is a well-formed expression.
//  2. RESOLUTION -- the construct exists in the tree. A call naming a
//     mutation nobody declared parses fine and resolves to nothing.
//  3. DECLARED ARGUMENTS -- every name passed is a field the construct
//     declares. Resolution does NOT cover this: validateFunctionArgs iterates
//     the DECLARED fields, so an argument the caller invented is silently
//     discarded and the row never receives it.
//
// The generated tree makes level 3 the interesting one here: the connector
// renders a mutation call from a table, so a mapping-rule change that renamed
// a field would produce a call that parses, resolves, and quietly writes
// nothing.

func newRealEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := memorynodes.DefaultRegistry()
	if registry == nil {
		t.Fatal("no concept registry")
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	return eng
}

// connectorCallSites is every function this package calls, with the argument
// map it builds. Keep it in step with the code: a call site that is not here
// is not covered, and the whole point of the file is that a call site nobody
// parsed is a call site that fails in production.
func connectorCallSites() []struct {
	fn   string
	args map[string]any
} {
	type site = struct {
		fn   string
		args map[string]any
	}
	sites := []site{
		// store.go / config.go
		{"stores", map[string]any{}},
		{"storeById", map[string]any{"storeId": "acme"}},
		{"storeByDomain", map[string]any{"domain": "acme.myshopify.com"}},
		{"createStore", map[string]any{
			"storeId": "acme", "domain": "acme.myshopify.com", "name": "Acme",
			"appClientId": "abc", "adminTokenRef": "A", "storefrontTokenRef": "S",
			"webhookSecretRef": "W", "apiVersion": "2026-07", "protectedDataLevel": "level1",
		}},
		{"setGlobalSecret", map[string]any{
			"id": "sec-x", "name": "SHOPIFY_ACME_ADMIN_TOKEN", "encryptedValue": "ZW5j",
			"fingerprint": "...abcd", "kind": "vendor_api_key", "description": "seeded", "addedBy": "system:connector:shopify",
		}},
		// subscriptions.go
		{"recordStoreHealth", map[string]any{
			"storeId": "acme", "health": map[string]any{"subscriptions": map[string]any{"desired": 3}},
			"subscriptionsCheckedAt": "2026-08-23T12:00:00Z",
		}},
		// compliance.go -- the connector's OWN queue. Not the outbox:
		// that one's drain hands every entry to Propagate, and a
		// privacy job handed to Propagate would try to write a
		// customer's export into Shopify.
		{"queueComplianceJob", map[string]any{
			"jobId": "shpcj1", "storeId": "acme", "topic": "customers/redact",
			"customerGid": "gid://shopify/Customer/1", "requestId": "dr-1",
			"shopDomain": "acme.myshopify.com", "dueAt": "2026-08-24T12:00:00Z",
		}},
		{"complianceJobsDue", map[string]any{"asOf": "2026-08-23T12:00:00Z"}},
		{"recordComplianceJob", map[string]any{
			"jobId": "shpcj1", "status": "done", "outcome": "scrubbed 3 row version(s)", "lastError": "",
		}},
		// analytics.go -- the reads the four commerce tools walk.
		{"ordersInWindow", map[string]any{"storeId": "acme", "from": "2026-07-24T12:00:00Z", "to": "2026-08-23T12:00:00Z"}},
		{"lineItemsForOrder", map[string]any{"storeId": "acme", "orderGid": "gid://shopify/Order/1"}},
		{"repeatCustomers", map[string]any{"storeId": "acme", "from": "2026-07-24T12:00:00Z", "to": "2026-08-23T12:00:00Z"}},
		{"refundRate", map[string]any{"storeId": "acme", "from": "2026-07-24T12:00:00Z", "to": "2026-08-23T12:00:00Z"}},
		{"stockBelow", map[string]any{"storeId": "acme", "locationGid": "gid://shopify/Location/1"}},
		{"ordersByCompany", map[string]any{"storeId": "acme", "companyGid": "gid://shopify/Company/1", "from": "2026-07-24T12:00:00Z", "to": "2026-08-23T12:00:00Z"}},
		{"paymentTermsOutstanding", map[string]any{"storeId": "acme"}},
		{"creditLimitsForCompany", map[string]any{"storeId": "acme", "companyGid": "gid://shopify/Company/1"}},
		// capabilities.go -- the runtime's health read, which the Stores
		// page renders through this connector.
		{"syncStatesAll", map[string]any{"connector": "shopify"}},
		{"setProductContentStatus", map[string]any{"contentId": "pc1", "status": "live"}},
		{"createAuditEvent", map[string]any{
			"eventId": "aud1", "occurredAt": "2026-08-23T12:00:00Z", "category": "data",
			"action": "shopify_customer_redacted", "actorUserId": "system:connector:shopify",
			"targetType": "shopifyStore", "targetId": "acme",
			"detail": map[string]any{"topic": "customers/redact"}, "outcome": "success",
		}},
		{"createGeneratedOutput", map[string]any{
			"outputId": "shpdr1", "title": "export.json", "summary": "s",
			"body": `{"rows":{}}`, "format": "text", "mimeType": "application/json", "source": "derived",
		}},
		{"markStoreRedacted", map[string]any{"storeId": "acme", "redactedAt": "2026-08-23T12:00:00Z"}},
	}
	// Every generated READ, for one representative root type and one
	// materialised child -- the two shapes the emitter produces. There are
	// no generated writes: the runtime performs mirror writes from the
	// MirrorWrites this connector returns, and the one statement the
	// connector renders itself is covered by TestTheMirrorInsertParses.
	for _, conceptName := range []string{"product", "orderLineItem"} {
		spec := generated.Types[conceptName]
		sites = append(sites,
			site{spec.ByGidFn, map[string]any{"storeId": "acme", "gid": "gid://shopify/X/1"}},
			site{spec.ForStoreFn, map[string]any{"storeId": "acme"}},
		)
	}
	return sites
}

// sampleValue produces a value of the declared type, so the rendered call
// exercises the literal form the connector really emits for that field kind.
func sampleValue(f generated.FieldSpec) any {
	switch f.DSLType {
	case "bool":
		return true
	case "int":
		return 3
	case "float":
		return 1.5
	case "[]string":
		return []string{"gid://shopify/X/2"}
	case "[]object":
		return []any{map[string]any{"k": "v"}}
	case "object", "any":
		return map[string]any{"ns.key": map[string]any{"value": "v"}}
	default:
		return "sample"
	}
}

func TestGeneratedCallsAreSyntacticallyValid(t *testing.T) {
	for _, site := range connectorCallSites() {
		stmt := renderCall(site.fn, site.args)
		if _, err := parseExpression(stmt); err != nil {
			t.Errorf("%s: the parser refused the rendered call:\n  %s\n  --> %v", site.fn, truncate(stmt, 400), err)
		}
	}
}

func TestGeneratedCallsResolveAgainstTheRealTree(t *testing.T) {
	eng := newRealEngine(t)
	checked := 0
	for _, site := range connectorCallSites() {
		stmt := renderCall(site.fn, site.args)
		checked++
		if _, err := eng.Parse(stmt); err != nil {
			t.Errorf("%s: the engine refused the rendered call:\n  %s\n  --> %v", site.fn, truncate(stmt, 400), err)
		}
	}
	if want := len(connectorCallSites()); checked != want {
		t.Fatalf("resolved %d call sites, want %d", checked, want)
	}
}

func TestGeneratedCallArgumentsAreDeclared(t *testing.T) {
	eng := newRealEngine(t)
	checked := 0
	for _, site := range connectorCallSites() {
		fn, err := eng.Functions().Get(site.fn)
		if err != nil || fn == nil {
			t.Errorf("%s: not in the function registry: %v", site.fn, err)
			continue
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, f := range fn.ArgsSchema.Fields {
				declared[f.Name] = true
			}
		}
		if len(declared) == 0 && len(site.args) > 0 {
			t.Errorf("%s declares no args block, but the call site passes %d", site.fn, len(site.args))
			continue
		}
		checked++
		for name := range site.args {
			if declared[name] {
				continue
			}
			t.Errorf("%s: the connector passes %q, which the construct does not declare. It is not "+
				"refused -- rejectUnknownArgs is gated behind the MCP boundary -- so the value is "+
				"silently discarded and the row never receives it (memql#3626).", site.fn, name)
		}
	}
	if want := len(connectorCallSites()); checked != want {
		t.Fatalf("checked %d call sites, want %d", checked, want)
	}
}

// EVERY generated READ, not just the two sampled above. The emitter
// produces the same shape for all 65 types, so a change that broke one
// breaks them all -- but a change that broke only the types with, say, a
// parent relationship would slip past a two-type sample.
func TestEveryGeneratedReadResolves(t *testing.T) {
	eng := newRealEngine(t)
	for name, spec := range generated.Types {
		if _, err := eng.Parse(renderCall(spec.ByGidFn, map[string]any{"storeId": "acme", "gid": "gid://shopify/X/1"})); err != nil {
			t.Errorf("%s byGid: %v", name, err)
		}
		if _, err := eng.Parse(renderCall(spec.ForStoreFn, map[string]any{"storeId": "acme"})); err != nil {
			t.Errorf("%s list: %v", name, err)
		}
	}
}

// The one statement this connector renders for itself -- the raw concept
// insert a reconciliation sweep heals with -- through the real front end.
//
// It carries a mirrored PAYLOAD, so it is the statement most likely to
// contain something the lexer objects to: a metafield key with a dot in
// it, an address with an apostrophe, a note with a newline. All three are
// in the fixture below on purpose.
func TestTheMirrorInsertParses(t *testing.T) {
	eng := newRealEngine(t)
	for _, w := range []memqlsync.MirrorWrite{
		{
			Concept: generated.ConceptID("order"),
			RowId:   "shp0123456789abcdef012345",
			Payload: map[string]any{
				"storeId":   "acme",
				"gid":       "gid://shopify/Order/1",
				"updatedAt": "2026-08-23T12:00:00Z",
				"syncedAt":  "2026-08-23T12:00:00Z",
				"deleted":   false,
				"name":      "O'Brien \"the\" <admin> & co \\ line\nbreak é",
				"metafields": map[string]any{
					"custom.care": map[string]any{"type": "single_line_text_field", "value": "cold wash"},
				},
				"totalPriceSet": map[string]any{"shopMoney": map[string]any{"amount": "42.00"}},
				"tags":          []any{"a", "b"},
			},
			Version: "2026-08-23T12:00:00Z",
		},
		tombstone("order", "acme", "gid://shopify/Order/1", timeFixture()),
	} {
		stmt, err := mirrorInsert(w)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if _, err := eng.Parse(stmt); err != nil {
			t.Errorf("the engine refused the mirror insert:\n  %s\n  --> %v", truncate(stmt, 400), err)
		}
	}
}

// The reconcile heal and the runtime's own writer must render the SAME
// statement, or a mirror is written one way by a live delivery and
// another by a sweep. mirror.go says so; this is what keeps it true.
func TestReconcileHealWritesTheRuntimesStatementShape(t *testing.T) {
	stmt, err := mirrorInsert(memqlsync.MirrorWrite{
		Concept: "v1:shopify:order",
		RowId:   "shp1",
		Payload: map[string]any{"gid": "gid://shopify/Order/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `insert("v1:shopify:order", id="shp1", payload={"gid":"gid://shopify/Order/1"})`
	if stmt != want {
		t.Errorf("rendered\n  %s\nwant\n  %s\n\nThis is the form component/datasync's EngineMirrorWriter renders. "+
			"If that one changed, change this one with it -- two mirrors written two ways is the failure.", stmt, want)
	}
	// A retirement adds the marker and nothing else.
	retired, err := mirrorInsert(memqlsync.MirrorWrite{Concept: "v1:shopify:order", RowId: "shp1", Retire: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(retired, `"deleted":true`) {
		t.Errorf("a retirement did not mark the row gone: %s", retired)
	}
}

// timeFixture is a fixed instant, so a rendered statement is stable.
func timeFixture() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) }
