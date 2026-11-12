package datasync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
	"google.golang.org/protobuf/types/known/structpb"
)

// shopify_mirror_rig_test.go -- the real Shopify connector and the runtime's
// own writers over one mirror, with no database.
//
// The races the version guard settles -- a delivery against a bulk export
// started before it, a sweep's page against a delivery that lands while it is
// out, a delete notice against a read made a second earlier -- are decided by
// two halves: the stamps the connector puts on a write, and the rule the
// runtime's applier holds them to. Neither half proves anything alone, so
// these tests run the connector's own Apply, BackfillStore and
// ReconcilePartition, and the runtime's own applier, dispatcher and runner,
// against a mirror both write into.
//
// They live here, in the root module, because the integrations module cannot
// import the runtime. The connector is built as production builds it
// (shopify.NewConnector over shopify.NewAdminClient), Shopify is scripted at
// the transport (shopify_origin_test.go), and the one thing a test says that
// production does not is what time it is (Connector.WithClock).

// shopRigStore is the rig's one store.
const shopRigStore = "acme"

// shopMirrorEngine is an engine with a mirror in it and nothing else: it
// applies the raw insert both writers render (read-merge, as the engine does:
// a field a write omits keeps its stored value), answers the raw concept read
// both guards make, lists a domain's live rows for a re-list, and answers the
// connector's read of its stores. Every other call is recorded and answered
// with no rows.
type shopMirrorEngine struct {
	// Only Execute is reached; the rest of the interface is never called.
	memql.IntegrationEngineAccess

	mu     stdsync.Mutex
	rows   map[string]map[string]any // concept|row id -> payload
	stores []map[string]any
	// storesAway makes the connector's read of its stores fail: a store list
	// that cannot be read.
	storesAway bool
	// named answers a read of a platform row by its name: the managed app's
	// client id and sealed secret, which the connector reads that way.
	named map[string]map[string]any
	calls []string
	// inserts counts the rows written, whoever wrote them.
	inserts int
}

var (
	shopInsertRE = regexp.MustCompile(`^insert\(("(?:[^"\\]|\\.)*"), id=("(?:[^"\\]|\\.)*"), payload=(.*)\)$`)
	shopReadRE   = regexp.MustCompile(`^concept==(\S+) && id==("(?:[^"\\]|\\.)*")$`)
	shopNamedRE  = regexp.MustCompile(`^concept==\S+&&payload\.name==("(?:[^"\\]|\\.)*")$`)
)

func (m *shopMirrorEngine) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	q = strings.TrimSpace(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, q)
	if g := shopInsertRE.FindStringSubmatch(q); g != nil {
		concept, _ := strconv.Unquote(g[1])
		id, _ := strconv.Unquote(g[2])
		var payload map[string]any
		if err := json.Unmarshal([]byte(g[3]), &payload); err != nil {
			return nil, err
		}
		m.inserts++
		row := m.rows[concept+"|"+id]
		if row == nil {
			row = map[string]any{}
			m.rows[concept+"|"+id] = row
		}
		for k, v := range payload {
			row[k] = v
		}
		return &memql.ExecuteResult{}, nil
	}
	if g := shopReadRE.FindStringSubmatch(q); g != nil {
		id, _ := strconv.Unquote(g[2])
		row := m.rows[g[1]+"|"+id]
		if row == nil {
			return shopRows(nil), nil
		}
		out := map[string]any{"id": id}
		for k, v := range row {
			out[k] = v
		}
		return shopRows([]map[string]any{out}), nil
	}
	if g := shopNamedRE.FindStringSubmatch(q); g != nil {
		name, _ := strconv.Unquote(g[1])
		if row := m.named[name]; row != nil {
			return shopRows([]map[string]any{row}), nil
		}
		return shopRows(nil), nil
	}
	name := shopCallName(q)
	if name == "storesForConnector" {
		if m.storesAway {
			return nil, errors.New("the store list could not be read")
		}
		return shopRows(m.stores), nil
	}
	for _, spec := range generated.Types {
		if spec.ForStoreFn == "" || name != spec.ForStoreFn {
			continue
		}
		var live []map[string]any
		prefix := generated.ConceptID(spec.Concept) + "|"
		for key, row := range m.rows {
			if strings.HasPrefix(key, prefix) && row["deleted"] != true {
				live = append(live, map[string]any{"id": strings.TrimPrefix(key, prefix), "gid": row["gid"]})
			}
		}
		return shopRows(live), nil
	}
	return shopRows(nil), nil
}

// sent is every call the engine was sent, in order.
func (m *shopMirrorEngine) sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// callsTo is every call of one name the engine was sent, in order.
func (m *shopMirrorEngine) callsTo(name string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, q := range m.calls {
		if shopCallName(q) == name {
			out = append(out, q)
		}
	}
	return out
}

func shopCallName(q string) string {
	q = strings.TrimSpace(q)
	for _, prefix := range []string{"query ", "mutation ", "builtin "} {
		q = strings.TrimPrefix(q, prefix)
	}
	if i := strings.IndexByte(q, '('); i > 0 {
		return q[:i]
	}
	return q
}

// shopRows is a result a caller can materialise rows from: through the bundle,
// the shape a read with no `shape` clause produces.
func shopRows(rows []map[string]any) *memql.ExecuteResult {
	if rows == nil {
		return &memql.ExecuteResult{}
	}
	nodes := make([]*memqlv1.MemoryNode, 0, len(rows))
	for _, r := range rows {
		payload := map[string]any{}
		for k, v := range r {
			if k != "id" {
				payload[k] = v
			}
		}
		pb, err := structpb.NewStruct(payload)
		if err != nil {
			panic("shop rig: a row is not representable as a struct: " + err.Error())
		}
		id, _ := r["id"].(string)
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: pb})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

// shopConnectorSide is the mirror as the CONNECTOR reaches it: the same
// engine, except that it refuses the connector's own write of the rows it
// names, as an engine that will not take one record does. The runtime's side
// is not refused anything.
type shopConnectorSide struct {
	*shopMirrorEngine
	refused map[string]bool
}

func (c *shopConnectorSide) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "insert(") {
		c.mu.Lock()
		for row := range c.refused {
			if strings.Contains(q, `"`+row+`"`) {
				c.mu.Unlock()
				return nil, errors.New("the mirror refused this row")
			}
		}
		c.mu.Unlock()
	}
	return c.shopMirrorEngine.Execute(ctx, q)
}

// shopSeam is the runtime's engine seam over the rig's.
type shopSeam struct{ e *shopMirrorEngine }

func (s shopSeam) Execute(ctx context.Context, q string) (any, error) { return s.e.Execute(ctx, q) }

// shopRig is the connector and the runtime's applier over one mirror.
type shopRig struct {
	t       *testing.T
	mirror  *shopMirrorEngine
	side    *shopConnectorSide
	origin  *scriptedOrigin
	stores  *shopify.StoreRegistry
	conn    *shopify.Connector
	applier *Applier
	specs   map[string]memqlsync.DomainSpec
	// now is what time it is: the connector's clock reads a millisecond past
	// it each time, so two reads are never the same instant, as they are not
	// in production.
	now time.Time
}

func newShopRig(t *testing.T) *shopRig {
	t.Helper()
	m := &shopMirrorEngine{rows: map[string]map[string]any{}, named: map[string]map[string]any{}}
	r := &shopRig{t: t, mirror: m, side: &shopConnectorSide{shopMirrorEngine: m, refused: map[string]bool{}},
		now: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
	r.origin = newScriptedOrigin(t)
	r.stores = shopify.NewStoreRegistry(r.side, func(_ context.Context, name string) (string, error) {
		switch name {
		case "ACME_ADMIN":
			return "shpat_test", nil
		case "ACME_WEBHOOK":
			return "webhook-secret", nil
		case shopManagedSecretName:
			return "managed-secret", nil
		}
		return "", errors.New("no such secret " + name)
	})
	r.granted()

	var mu stdsync.Mutex
	var base time.Time
	ticks := 0
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if !r.now.Equal(base) {
			base, ticks = r.now, 0
		}
		ticks++
		return base.Add(time.Duration(ticks) * time.Millisecond)
	}
	r.conn = shopify.NewConnector(r.side, slog.New(slog.DiscardHandler), r.stores, shopify.NewAdminClient()).WithClock(clock)
	r.specs = map[string]memqlsync.DomainSpec{}
	for _, spec := range r.conn.Domains() {
		r.specs[spec.Concept] = spec
	}
	r.applier = NewApplier(NewStore(shopSeam{m}), NewEngineMirrorWriter(shopSeam{m}))
	return r
}

// granted writes the rig's store with the scopes it was granted; with none,
// a store whose grant was never recorded.
func (r *shopRig) granted(scopes ...string) {
	row := map[string]any{
		"id": shopRigStore, "domain": "acme-widgets.myshopify.com", "name": "Acme Widgets",
		"adminTokenRef": "ACME_ADMIN", "webhookSecretRef": "ACME_WEBHOOK", "apiVersion": generated.APIVersion,
		"protectedDataLevel": shopify.ProtectedLevel1, "status": shopify.StatusLive, "ownerUserId": "user-1",
	}
	if len(scopes) > 0 {
		list := make([]any, 0, len(scopes))
		for _, s := range scopes {
			list = append(list, s)
		}
		row["scopesGranted"] = list
	}
	r.mirror.mu.Lock()
	r.mirror.stores = []map[string]any{row}
	r.mirror.mu.Unlock()
	r.stores.Invalidate()
}

// The names the shared app's client id and sealed secret are stored under:
// what an installation's owner sets to connect stores through it.
const (
	shopManagedClientName = "SHOPIFY_CONNECT_CLIENT_ID"
	shopManagedSecretName = "SHOPIFY_CONNECT_CLIENT_SECRET"
)

// managedApp configures the shared app on the rig's engine, so a delivery on
// the app-level source is one the connector trusts as app-signed.
func (r *shopRig) managedApp() {
	r.mirror.mu.Lock()
	defer r.mirror.mu.Unlock()
	r.mirror.named[shopManagedClientName] = map[string]any{"id": "managed-client", "name": shopManagedClientName, "value": "managed-app"}
	r.mirror.named[shopManagedSecretName] = map[string]any{"id": "managed-secret", "name": shopManagedSecretName, "encryptedValue": "sealed-test"}
}

// storeListAway makes the connector's store list unreadable, or readable
// again.
func (r *shopRig) storeListAway(away bool) {
	r.mirror.mu.Lock()
	r.mirror.storesAway = away
	r.mirror.mu.Unlock()
	r.stores.Invalidate()
}

// readsAllOrders grants the rig's store read_all_orders: the origin then hides
// no order from it, so an answer of nothing for one can be a deletion. The
// tests that play that rule through an order use it; without it an order is
// never retired by an answer of nothing.
func (r *shopRig) readsAllOrders() { r.granted("read_orders", "read_all_orders") }

func (r *shopRig) store() shopify.Store {
	r.t.Helper()
	s, ok := r.stores.ByID(context.Background(), shopRigStore)
	if !ok {
		r.t.Fatal("the rig's store did not resolve")
	}
	return s
}

// refuses makes the mirror refuse the connector's write of these gids.
func (r *shopRig) refuses(gids ...string) {
	r.mirror.mu.Lock()
	defer r.mirror.mu.Unlock()
	for _, gid := range gids {
		r.side.refused[shopify.MirrorRowID(shopRigStore, gid)] = true
	}
}

func (r *shopRig) connectorCtx() context.Context {
	return auth.ContextWithConnectorActor(context.Background(), shopify.ConnectorName)
}

// apply writes as the runtime does, behind its guard.
func (r *shopRig) apply(writes []memqlsync.MirrorWrite) ApplyResult {
	r.t.Helper()
	res, err := r.applier.Apply(r.connectorCtx(), shopify.ConnectorName, r.specs, writes)
	if err != nil {
		r.t.Fatalf("apply: %v", err)
	}
	return res
}

// deliver works one delivery the way the dispatcher does: the connector's
// Apply, then the runtime's applier.
func (r *shopRig) deliver(req memqlsync.InboundRequest) ApplyResult {
	r.t.Helper()
	writes, err := r.conn.Apply(context.Background(), req)
	if err != nil {
		r.t.Fatalf("Apply %s: %v", req.Topic, err)
	}
	return r.apply(writes)
}

// deliverMeanwhile is deliver from inside a scripted origin answer, which runs
// while another call is out and so reports rather than stops.
func (r *shopRig) deliverMeanwhile(req memqlsync.InboundRequest) {
	writes, err := r.conn.Apply(context.Background(), req)
	if err != nil {
		r.t.Errorf("Apply %s: %v", req.Topic, err)
		return
	}
	if _, err := r.applier.Apply(r.connectorCtx(), shopify.ConnectorName, r.specs, writes); err != nil {
		r.t.Errorf("apply %s: %v", req.Topic, err)
	}
}

// sweep is one sweep of a domain of the rig's store, as the runtime asks it.
func (r *shopRig) sweep(concept string, since time.Time) (memqlsync.ReconcileReport, error) {
	return r.conn.ReconcilePartition(context.Background(), shopRigStore, generated.ConceptID(concept), since)
}

// row is the mirror's row for a gid of a concept, or nil.
func (r *shopRig) row(concept, gid string) map[string]any {
	r.mirror.mu.Lock()
	defer r.mirror.mu.Unlock()
	return r.mirror.rows[generated.ConceptID(concept)+"|"+shopify.MirrorRowID(shopRigStore, gid)]
}

// mirrorWrites counts the rows written into the mirror so far.
func (r *shopRig) mirrorWrites() int {
	r.mirror.mu.Lock()
	defer r.mirror.mu.Unlock()
	return r.mirror.inserts
}

// held is a live record the mirror holds, written under the rule: its version
// the origin's, read at `read`.
func (r *shopRig) held(concept, gid, updatedAt string, read time.Time) {
	r.put(concept, gid, map[string]any{"name": "held", "updatedAt": updatedAt, "versionSource": memqlsync.VersionFromOrigin,
		"syncedAt": memqlsync.ReadTime(read), "versionReadAt": memqlsync.ReadTime(read), "deleted": false})
}

// put writes a row straight into the mirror, as an earlier writer left it.
func (r *shopRig) put(concept, gid string, payload map[string]any) {
	r.mirror.mu.Lock()
	defer r.mirror.mu.Unlock()
	row := map[string]any{"storeId": shopRigStore, "gid": gid}
	for k, v := range payload {
		row[k] = v
	}
	r.mirror.rows[generated.ConceptID(concept)+"|"+shopify.MirrorRowID(shopRigStore, gid)] = row
}

// exportStarted starts a domain's bulk export: the origin takes its snapshot
// from here.
func (r *shopRig) exportStarted(concept string) string {
	r.t.Helper()
	r.origin.reply("ShopifyBulkRun", originStartedOperation("gid://shopify/BulkOperation/1"))
	page, err := r.conn.BackfillStore(context.Background(), r.store(), generated.ConceptID(concept), "")
	if err != nil {
		r.t.Fatalf("start the export: %v", err)
	}
	return page.NextCursor
}

// exportRead finishes the export with these lines and returns its page, not
// yet applied.
func (r *shopRig) exportRead(concept, cursor string, lines ...map[string]any) memqlsync.BackfillPage {
	r.t.Helper()
	link := r.origin.serveJSONL("/download/"+concept+".jsonl", originBulkLines(lines...))
	r.origin.reply("ShopifyBulkStatus", originOperationStatus("gid://shopify/BulkOperation/1", "COMPLETED", link))
	page, err := r.conn.BackfillStore(context.Background(), r.store(), generated.ConceptID(concept), cursor)
	if err != nil {
		r.t.Fatalf("stream the export: %v", err)
	}
	return page
}

// exportStreamed finishes the export with these lines and applies its page.
func (r *shopRig) exportStreamed(concept, cursor string, lines ...map[string]any) (ApplyResult, memqlsync.BackfillPage) {
	r.t.Helper()
	page := r.exportRead(concept, cursor, lines...)
	return r.apply(page.Writes), page
}

func (r *shopRig) orderFetches(node map[string]any) {
	if node == nil {
		// A clean answer of nothing: the node is null, and there is no error.
		r.origin.reply("ShopifyFetchOrder", map[string]any{"node": nil})
		return
	}
	r.origin.reply("ShopifyFetchOrder", map[string]any{"node": node})
}

// ---- what a test hands the rig ----

// shopNotice is one staged delivery of a topic for a gid, triggered at the
// origin at `triggered`.
func shopNotice(topic, gid, webhookID string, triggered time.Time) memqlsync.InboundRequest {
	body, _ := json.Marshal(map[string]any{"admin_graphql_api_id": gid})
	return memqlsync.InboundRequest{
		RequestId: webhookID,
		Source:    shopify.ConnectorName + "-" + shopRigStore,
		Topic:     topic,
		Body:      body,
		// Staged two seconds after the origin's event; a retry keeps it.
		ReceivedAt: triggered.UTC().Add(2 * time.Second),
		Headers: map[string]string{
			"x-shopify-topic":        topic,
			"x-shopify-shop-domain":  "acme-widgets.myshopify.com",
			"x-shopify-webhook-id":   webhookID,
			"x-shopify-triggered-at": triggered.UTC().Format(time.RFC3339),
		},
	}
}

// shopRetried is a delivery on its retry after a try that ended in a clean
// empty answer, as the dispatcher hands it (AnsweredNothingBefore).
func shopRetried(req memqlsync.InboundRequest) memqlsync.InboundRequest {
	req.Attempts = 1
	req.AnsweredNothingBefore = true
	return req
}

// clockAt is a time of the rig's day, 2026-08-23.
func clockAt(clock string) time.Time {
	t, err := time.Parse(time.RFC3339, "2026-08-23T"+clock+"Z")
	if err != nil {
		panic(err)
	}
	return t
}

// shopOrderNode is an order as the origin answers it, with one line.
func shopOrderNode(updatedAt string, quantity int) map[string]any {
	return map[string]any{
		"__typename": "Order",
		"id":         "gid://shopify/Order/1",
		"name":       "#1001",
		"updatedAt":  updatedAt,
		"lineItems": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":    []any{map[string]any{"id": "gid://shopify/LineItem/1", "name": "Tee", "quantity": quantity}},
		},
	}
}

// shopOrderLines are an order and its line as a bulk export streams them.
func shopOrderLines(updatedAt string, quantity int) []map[string]any {
	return []map[string]any{
		{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": updatedAt},
		{"id": "gid://shopify/LineItem/1", "name": "Tee", "quantity": quantity, "__parentId": "gid://shopify/Order/1"},
	}
}

func shopStringAt(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}
