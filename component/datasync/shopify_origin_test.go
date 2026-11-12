package datasync

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// shopify_origin_test.go -- a scripted Shopify for the tests that compose the
// real connector with the runtime.
//
// The connector's own tests script the Admin API on an httptest server reached
// through seams the connector does not export. A test in this module has only
// what production has -- shopify.NewAdminClient, which speaks https to the
// store's domain -- so the origin is scripted one layer down, at the HTTP
// transport: the Admin GraphQL endpoint of any *.myshopify.com host, and the
// files of a bulk export on downloadHost. Every request the connector makes
// is the one production makes.
//
// IT ANSWERS ONLY WHAT IT WAS ASKED, as the connector's own fake does and for
// the same reason: a reply or a bulk line that carries a version field the
// document does not select is a failure of the test that scripted it, and the
// field is not in the answer. A fake that hands updatedAt back whether or not
// it was selected is how the mirror stamped its own clock for months behind a
// green suite.

// scriptedOrigin answers the Admin API by operation name.
type scriptedOrigin struct {
	mu        stdsync.Mutex
	requests  []originRequest
	replies   map[string]any
	replyFns  map[string]func(vars map[string]any) any
	errors    map[string][]shopify.GraphQLError
	status    map[string]int
	downloads map[string]string
	// bulkDocuments are the bulk queries the origin was asked to run, in
	// order: what a file it serves answers.
	bulkDocuments []string
	// unselected is told of every version field a scripted reply or a served
	// bulk line carried that the document did not select. It fails the test.
	unselected func(message string)
}

// originRequest is one Admin request as the origin received it.
type originRequest struct {
	Operation string
	Query     string
	Variables map[string]any
	Token     string
	Host      string
}

// newScriptedOrigin puts a scripted Shopify in front of every outbound request
// of the test, and takes it away when the test ends.
func newScriptedOrigin(t *testing.T) *scriptedOrigin {
	t.Helper()
	o := &scriptedOrigin{
		replies:   map[string]any{},
		replyFns:  map[string]func(map[string]any) any{},
		errors:    map[string][]shopify.GraphQLError{},
		status:    map[string]int{},
		downloads: map[string]string{},
	}
	o.unselected = func(message string) { t.Errorf("%s", message) }
	previous := http.DefaultTransport
	http.DefaultTransport = o
	t.Cleanup(func() { http.DefaultTransport = previous })
	return o
}

func (o *scriptedOrigin) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.URL.Host == downloadHost:
		return o.serveDownload(r), nil
	case strings.HasSuffix(r.URL.Host, ".myshopify.com"):
		return o.serveGraphQL(r)
	}
	return nil, fmt.Errorf("unexpected external request: %s", r.URL.Host)
}

func originAnswer(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func (o *scriptedOrigin) serveGraphQL(r *http.Request) (*http.Response, error) {
	var body struct {
		Query         string         `json:"query"`
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	op := body.OperationName
	if op == "" {
		op = originOperationName(body.Query)
	}

	o.mu.Lock()
	o.requests = append(o.requests, originRequest{
		Operation: op, Query: body.Query, Variables: body.Variables,
		Token: r.Header.Get("X-Shopify-Access-Token"), Host: r.URL.Host,
	})
	fn := o.replyFns[op]
	o.mu.Unlock()
	// Called outside the lock, so an answer may itself call the origin: a
	// delivery that lands while a sweep's page is out.
	var fnReply any
	if fn != nil {
		fnReply = fn(body.Variables)
	}
	o.mu.Lock()
	reply, hasReply := o.replies[op]
	if fn != nil {
		reply, hasReply = fnReply, true
	}
	errs := o.errors[op]
	if op == "ShopifyBulkRun" {
		if document, ok := body.Variables["query"].(string); ok {
			o.bulkDocuments = append(o.bulkDocuments, document)
		}
	}
	status := o.status[op]
	unselected := o.unselected
	o.mu.Unlock()
	if hasReply {
		reply = originAnswerSelected(reply, []string{body.Query}, func(field string) {
			unselected(fmt.Sprintf("scripted Shopify: the reply scripted for %s carries %s, which the document it was sent does not select", op, field))
		})
	}

	if status != 0 && status != http.StatusOK {
		raw, _ := json.Marshal(map[string]any{"errors": errs})
		return originAnswer(r, status, string(raw)), nil
	}
	envelope := map[string]any{
		"extensions": map[string]any{"cost": map[string]any{
			"actualQueryCost": 10,
			"throttleStatus":  map[string]any{"maximumAvailable": 2000, "currentlyAvailable": 1800, "restoreRate": 100},
		}},
	}
	if len(errs) > 0 {
		envelope["errors"] = errs
	}
	if hasReply {
		envelope["data"] = reply
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return originAnswer(r, http.StatusOK, string(raw)), nil
}

func (o *scriptedOrigin) serveDownload(r *http.Request) *http.Response {
	o.mu.Lock()
	body, ok := o.downloads[r.URL.Path]
	documents := append([]string(nil), o.bulkDocuments...)
	unselected := o.unselected
	o.mu.Unlock()
	if !ok {
		return originAnswer(r, http.StatusForbidden, "")
	}
	if len(documents) == 0 {
		// A test that resumes from a cursor naming the operation never had the
		// origin start it; the line answers the export the generated bulk
		// documents would run.
		for _, spec := range generated.Types {
			if spec.BulkDocument != "" {
				documents = append(documents, spec.BulkDocument)
			}
		}
	}
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var obj any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			out.WriteString(line + "\n")
			continue
		}
		raw, _ := json.Marshal(originAnswerSelected(obj, documents, func(field string) {
			unselected(fmt.Sprintf("scripted Shopify: a bulk line served from %s carries %s, which no bulk document it was sent selects", r.URL.Path, field))
		}))
		out.Write(raw)
		out.WriteByte('\n')
	}
	return originAnswer(r, http.StatusOK, out.String())
}

// reply answers every request for op with data.
func (o *scriptedOrigin) reply(op string, data any) {
	o.mu.Lock()
	o.replies[op] = data
	o.mu.Unlock()
}

// replyWith answers op from each request's variables.
func (o *scriptedOrigin) replyWith(op string, fn func(vars map[string]any) any) {
	o.mu.Lock()
	o.replyFns[op] = fn
	o.mu.Unlock()
}

// fail answers every request for op with errs, alongside whatever reply is
// scripted (GraphQL returns data and errors together).
func (o *scriptedOrigin) fail(op string, errs ...shopify.GraphQLError) {
	o.mu.Lock()
	o.errors[op] = errs
	o.mu.Unlock()
}

// httpStatus answers every request for op with an HTTP status; 0 clears it.
func (o *scriptedOrigin) httpStatus(op string, code int) {
	o.mu.Lock()
	o.status[op] = code
	o.mu.Unlock()
}

// serveJSONL serves body as the file of a bulk export and returns its link.
func (o *scriptedOrigin) serveJSONL(path, body string) string {
	o.mu.Lock()
	o.downloads[path] = body
	o.mu.Unlock()
	return "https://" + downloadHost + path
}

func (o *scriptedOrigin) seen() []originRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]originRequest(nil), o.requests...)
}

func (o *scriptedOrigin) countOp(op string) int {
	n := 0
	for _, r := range o.seen() {
		if r.Operation == op {
			n++
		}
	}
	return n
}

func originOperationName(document string) string {
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

// originVersionFields are the Admin fields a mirrored type's version is read
// from.
var originVersionFields = []string{"updatedAt", "createdAt"}

// originAnswerSelected is a reply as the origin would send it: a version field
// the documents do not select AT THAT OBJECT'S PATH is not in it, and
// `unselected` is told, once per field.
//
// The path is the reply's keys from the root (an array adds nothing), read
// against the document's selection sets (originSelectionSets). A bulk line
// carries no path: a parent line (no __parentId) is checked against the sets
// of the documents' top-level nodes, a child line against any nested set.
func originAnswerSelected(reply any, documents []string, unselected func(field string)) any {
	sets := map[string]map[string]bool{}
	for _, document := range documents {
		for path, fields := range originSelectionSets(document) {
			if sets[path] == nil {
				sets[path] = map[string]bool{}
			}
			for f := range fields {
				sets[path][f] = true
			}
		}
	}
	told := map[string]bool{}
	tell := func(field string) {
		if !told[field] {
			told[field] = true
			unselected(field)
		}
	}
	var strip func(v any, path string, selected func(path, field string) bool) any
	strip = func(v any, path string, selected func(path, field string) bool) any {
		switch x := v.(type) {
		case map[string]any:
			if x == nil {
				return nil
			}
			out := make(map[string]any, len(x))
			for k, item := range x {
				if slices.Contains(originVersionFields, k) && !selected(path, k) {
					tell(k)
					continue
				}
				out[k] = strip(item, path+"/"+k, selected)
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, item := range x {
				out[i] = strip(item, path, selected)
			}
			return out
		}
		return v
	}
	exact := func(path, field string) bool { return sets[path][field] }
	if line, ok := reply.(map[string]any); ok && originIsBulkLine(documents) {
		_, child := line["__parentId"]
		return strip(line, "", func(path, field string) bool {
			if path != "" {
				return false
			}
			for set, fields := range sets {
				depth := strings.Count(set, "/")
				if fields[field] && strings.HasSuffix(set, "/edges/node") && (depth == 3) != child {
					return true
				}
				if fields[field] && child && depth > 3 {
					return true
				}
			}
			return false
		})
	}
	return strip(reply, "", exact)
}

// originIsBulkLine reports whether the documents are bulk queries: their root
// connection is read through edges, never pageInfo.
func originIsBulkLine(documents []string) bool {
	for _, d := range documents {
		if !strings.Contains(d, "edges {") || strings.Contains(d, "pageInfo") {
			return false
		}
	}
	return len(documents) > 0
}

// originSelectionSets reads a document the generator emitted -- one field per
// line, a set opened by a line ending in "{" and closed by a lone "}" -- into
// the direct fields of each selection set, keyed by the path of field names
// from the operation's root ("/node", "/orders/nodes", "/orders/edges/node").
// An inline fragment ("... on Order {") adds nothing to the path.
func originSelectionSets(document string) map[string]map[string]bool {
	sets := map[string]map[string]bool{}
	var stack []string // "" for an operation or a fragment
	path := func() string {
		var b strings.Builder
		for _, name := range stack {
			if name != "" {
				b.WriteString("/" + name)
			}
		}
		return b.String()
	}
	add := func(field string) {
		key := path()
		if sets[key] == nil {
			sets[key] = map[string]bool{}
		}
		sets[key][field] = true
	}
	for _, raw := range strings.Split(document, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case line == "}":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case strings.HasSuffix(line, "{"):
			opening := strings.TrimSpace(strings.TrimSuffix(line, "{"))
			switch {
			case len(stack) == 0, strings.HasPrefix(opening, "..."):
				stack = append(stack, "")
			default:
				name := opening
				if k := strings.IndexAny(name, "( "); k > 0 {
					name = name[:k]
				}
				add(name)
				stack = append(stack, name)
			}
		default:
			name := line
			if k := strings.IndexAny(name, "( {"); k > 0 {
				name = name[:k]
			}
			add(name)
		}
	}
	return sets
}

// ---- answers a test scripts ----

// originBulkLines is a bulk export's file: one JSON object a line.
func originBulkLines(lines ...map[string]any) string {
	var b strings.Builder
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// originStartedOperation is the origin's answer to a bulk run it accepted.
func originStartedOperation(id string) map[string]any {
	return map[string]any{"bulkOperationRunQuery": map[string]any{
		"bulkOperation": map[string]any{"id": id, "status": "CREATED"},
		"userErrors":    []any{},
	}}
}

// originOperationStatus is the origin's answer for a bulk operation: its
// status and, when it has one, the link of its file.
func originOperationStatus(id, status, link string) map[string]any {
	return map[string]any{"node": map[string]any{"id": id, "status": status, "url": link, "objectCount": "2"}}
}

// originListPage is one page of a list operation's root connection.
func originListPage(root string, hasNext bool, end string, nodes ...map[string]any) map[string]any {
	list := make([]any, 0, len(nodes))
	for _, n := range nodes {
		list = append(list, n)
	}
	return map[string]any{root: map[string]any{
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": end},
		"nodes":    list,
	}}
}
