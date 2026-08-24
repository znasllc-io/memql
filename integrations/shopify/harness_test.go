package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// harness_test.go -- a fake Admin endpoint and a recording engine.
//
// Both are FAKES rather than mocks, and the distinction is the point. The
// engine records the MemQL text the connector builds and answers reads from a
// row table, so a test can assert what was written; the Admin endpoint serves
// canned GraphQL replies keyed by operation name, so a test can assert what
// was fetched. Neither pretends to be the real thing -- what they check is the
// connector's half of each conversation, which is the half this package owns.
//
// What they DO NOT check, stated so a reader does not over-trust a green run:
// the MemQL calls are recorded, not PARSED. That is the memql#4256 class, and
// it is covered separately by TestGeneratedCallsParse, which puts every call
// shape this package builds through the real front end.

// fakeEngine records executed MemQL and answers reads from a table.
type fakeEngine struct {
	memql.IntegrationEngineAccess

	mu       stdsync.Mutex
	executed []string
	// rows maps a function NAME to the rows a call of it returns. The
	// simplest thing that lets a test say "there are two stores".
	rows map[string][]map[string]any
	// fail maps a function name to an error it returns.
	fail map[string]error
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{rows: map[string][]map[string]any{}, fail: map[string]error{}}
}

func (f *fakeEngine) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	f.mu.Lock()
	f.executed = append(f.executed, q)
	name := callName(q)
	err := f.fail[name]
	rows := f.rows[name]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return resultWithRows(rows), nil
}

// resultWithRows builds an ExecuteResult a caller can materialise rows from.
//
// It goes through the BUNDLE rather than the shaped-output path, because that
// is the shape a query with no `shape` clause actually produces and the
// generated mirror reads carry none. Building the friendlier shape here would
// make every test agree with a projection the engine does not apply.
func resultWithRows(rows []map[string]any) *memql.ExecuteResult {
	if rows == nil {
		return &memql.ExecuteResult{}
	}
	nodes := make([]*memqlv1.MemoryNode, 0, len(rows))
	for _, r := range rows {
		payload := map[string]any{}
		for k, v := range r {
			if k == "id" {
				continue
			}
			payload[k] = v
		}
		pb, err := structpb.NewStruct(payload)
		if err != nil {
			panic("harness: row is not representable as a struct: " + err.Error())
		}
		id, _ := r["id"].(string)
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: pb})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

func (f *fakeEngine) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.executed...)
}

func (f *fakeEngine) callsTo(name string) []string {
	var out []string
	for _, q := range f.calls() {
		if callName(q) == name {
			out = append(out, q)
		}
	}
	return out
}

func (f *fakeEngine) setRows(name string, rows []map[string]any) {
	f.mu.Lock()
	f.rows[name] = rows
	f.mu.Unlock()
}

func callName(q string) string {
	q = strings.TrimSpace(q)
	for _, prefix := range []string{"query ", "mutation ", "builtin "} {
		q = strings.TrimPrefix(q, prefix)
	}
	if i := strings.IndexByte(q, '('); i > 0 {
		return q[:i]
	}
	return q
}

// fakeAdmin is a Shopify Admin GraphQL endpoint that answers by operation
// name.
type fakeAdmin struct {
	server *httptest.Server

	mu        stdsync.Mutex
	requests  []adminRequest
	replies   map[string]any
	errors    map[string][]GraphQLError
	status    map[string]int
	throttle  ThrottleStatus
	downloads map[string]string
}

type adminRequest struct {
	Operation string
	Query     string
	Variables map[string]any
	Token     string
}

func newFakeAdmin(t *testing.T) *fakeAdmin {
	t.Helper()
	f := &fakeAdmin{
		replies:   map[string]any{},
		errors:    map[string][]GraphQLError{},
		status:    map[string]int{},
		downloads: map[string]string{},
		throttle:  ThrottleStatus{MaximumAvailable: 2000, CurrentlyAvailable: 1800, RestoreRate: 100},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", f.serveGraphQL)
	mux.HandleFunc("/download/", f.serveDownload)
	mux.HandleFunc("/staged", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAdmin) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query         string         `json:"query"`
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	op := body.OperationName
	if op == "" {
		op = operationNameOf(body.Query)
	}

	f.mu.Lock()
	f.requests = append(f.requests, adminRequest{
		Operation: op, Query: body.Query, Variables: body.Variables,
		Token: r.Header.Get("X-Shopify-Access-Token"),
	})
	reply, hasReply := f.replies[op]
	errs := f.errors[op]
	status := f.status[op]
	throttle := f.throttle
	f.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": errs})
		return
	}
	envelope := map[string]any{
		"extensions": map[string]any{"cost": map[string]any{
			"actualQueryCost": 10,
			"throttleStatus": map[string]any{
				"maximumAvailable":   throttle.MaximumAvailable,
				"currentlyAvailable": throttle.CurrentlyAvailable,
				"restoreRate":        throttle.RestoreRate,
			},
		}},
	}
	if len(errs) > 0 {
		envelope["errors"] = errs
	}
	if hasReply {
		envelope["data"] = reply
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

func (f *fakeAdmin) serveDownload(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	body, ok := f.downloads[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	_, _ = w.Write([]byte(body))
}

func (f *fakeAdmin) reply(op string, data any) {
	f.mu.Lock()
	f.replies[op] = data
	f.mu.Unlock()
}

func (f *fakeAdmin) userError(op, field, message string) {
	f.mu.Lock()
	f.replies[op] = map[string]any{
		field: map[string]any{"userErrors": []any{map[string]any{"message": message}}},
	}
	f.mu.Unlock()
}

func (f *fakeAdmin) graphQLError(op, code, message string) {
	f.mu.Lock()
	f.errors[op] = []GraphQLError{{Message: message, Extensions: map[string]any{"code": code}}}
	f.mu.Unlock()
}

func (f *fakeAdmin) httpStatus(op string, code int) {
	f.mu.Lock()
	f.status[op] = code
	f.mu.Unlock()
}

func (f *fakeAdmin) setThrottle(s ThrottleStatus) {
	f.mu.Lock()
	f.throttle = s
	f.mu.Unlock()
}

func (f *fakeAdmin) serveJSONL(path, body string) string {
	f.mu.Lock()
	f.downloads[path] = body
	f.mu.Unlock()
	return f.server.URL + path
}

func (f *fakeAdmin) seen() []adminRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]adminRequest(nil), f.requests...)
}

func (f *fakeAdmin) seenOps() []string {
	out := []string{}
	for _, r := range f.seen() {
		out = append(out, r.Operation)
	}
	return out
}

func (f *fakeAdmin) countOp(op string) int {
	n := 0
	for _, r := range f.seen() {
		if r.Operation == op {
			n++
		}
	}
	return n
}

func operationNameOf(document string) string {
	for _, line := range strings.Split(document, "\n") {
		line = strings.TrimSpace(line)
		for _, kw := range []string{"query ", "mutation "} {
			if !strings.HasPrefix(line, kw) {
				continue
			}
			rest := line[len(kw):]
			cut := strings.IndexAny(rest, "( {")
			if cut < 0 {
				return strings.TrimSpace(rest)
			}
			return strings.TrimSpace(rest[:cut])
		}
	}
	return ""
}

// testHarness wires a connector against both fakes.
type testHarness struct {
	engine *fakeEngine
	admin  *fakeAdmin
	conn   *Connector
	now    time.Time
	slept  []time.Duration
}

const testStoreID = "acme"

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	engine := newFakeEngine()
	admin := newFakeAdmin(t)
	h := &testHarness{engine: engine, admin: admin, now: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}

	engine.setRows("stores", []map[string]any{{
		"id":                 testStoreID,
		"domain":             "acme-widgets.myshopify.com",
		"name":               "Acme Widgets",
		"adminTokenRef":      "ACME_ADMIN",
		"webhookSecretRef":   "ACME_WEBHOOK",
		"apiVersion":         "",
		"protectedDataLevel": ProtectedLevel1,
		"status":             StatusLive,
		"ownerUserId":        "user-1",
	}})

	registry := NewStoreRegistry(engine, func(_ context.Context, name string) (string, error) {
		switch name {
		case "ACME_ADMIN":
			return "shpat_test", nil
		case "ACME_WEBHOOK":
			return "webhook-secret", nil
		}
		return "", fmt.Errorf("no such secret %q", name)
	})
	registry.ttl = 0 // every read re-walks, so a test's row edits take effect

	client := NewAdminClient()
	client.endpoint = func(Store) string { return admin.server.URL + "/graphql" }
	client.sleep = func(_ context.Context, d time.Duration) error {
		h.slept = append(h.slept, d)
		return nil
	}
	client.now = func() time.Time { return h.now }

	conn := NewConnector(engine, slog.New(slog.DiscardHandler), registry, client)
	conn.now = func() time.Time { return h.now }
	conn.deliver = func(s Store) string { return "https://api.example.test/inbound/shopify-" + s.ID }
	h.conn = conn
	return h
}

func (h *testHarness) store(t *testing.T) Store {
	t.Helper()
	s, ok := h.conn.stores.ByID(context.Background(), testStoreID)
	if !ok {
		t.Fatal("the harness store did not resolve")
	}
	return s
}


